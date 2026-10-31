package cli

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"

	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

//go:generate go run ../../cmd/flint-index

// Command's public JSON deliberately omits renderer and discovery controls.
// Preserve those and generated argument markers in the private build artifact.
type startupCommand struct {
	*Command
	Render    string `json:"render_internal,omitempty"`
	MCPHidden bool   `json:"mcp_hidden_internal,omitempty"`
	Generated []bool `json:"generated_arguments_internal,omitempty"`
}

func GenerateStartupIndex(path string) error {
	registry := newRegistryFromSnapshot(true)
	commands := make([]startupCommand, 0, len(registry.Commands))
	for _, cmd := range registry.Commands {
		flags := make([]bool, len(cmd.Arguments))
		for i, arg := range cmd.Arguments {
			flags[i] = arg.bodyGenerated
		}
		commands = append(commands, startupCommand{cmd, cmd.Render, cmd.MCPHidden, flags})
	}
	raw, err := json.Marshal(commands)
	if err != nil {
		return err
	}
	var locals []startupCommand
	for _, cmd := range commands {
		if cmd.Local {
			locals = append(locals, cmd)
		}
	}
	localRaw, err := json.Marshal(locals)
	if err != nil {
		return err
	}
	raw, err = apispec.GenerateStartup(raw, localRaw)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0644)
}

func newRegistry(includeAPI bool) *Registry { return indexedRegistry(includeAPI) }

// The parsed catalogs are immutable. Every registry receives an isolated copy,
// so App/test mutations never reach another caller or the cached catalog.
var startupCommands = sync.OnceValue(func() []*Command {
	return decodeStartupCommands(apispec.Startup().Commands)
})
var startupLocalCommands = sync.OnceValue(func() []*Command {
	return decodeStartupCommands(apispec.Startup().LocalCommands)
})

func decodeStartupCommands(raw json.RawMessage) []*Command {
	var stored []startupCommand
	if err := json.Unmarshal(raw, &stored); err != nil {
		panic(err)
	}
	commands := make([]*Command, len(stored))
	for i, entry := range stored {
		cmd := entry.Command
		cmd.Render, cmd.MCPHidden = entry.Render, entry.MCPHidden
		for n, generated := range entry.Generated {
			cmd.Arguments[n].bodyGenerated = generated
		}
		commands[i] = cmd
	}
	return commands
}

func indexedRegistry(includeAPI bool) *Registry {
	var commands []*Command
	if includeAPI {
		commands = startupCommands()
	} else {
		commands = startupLocalCommands()
	}
	registry := &Registry{Commands: make([]*Command, 0, len(commands)), treeBuild: &sync.Once{}, localOnly: !includeAPI, byName: make(map[string]*Command, len(commands)), byPath: make(map[string]*Command, len(commands))}
	for _, cmd := range commands {
		registry.add(cloneStartupCommand(cmd))
	}
	registry.treeSnapshot()
	return registry
}

func cloneStartupCommand(source *Command) *Command {
	clone := *source
	clone.Path = slices.Clone(source.Path)
	clone.CanonicalPath = slices.Clone(source.CanonicalPath)
	clone.Aliases = slices.Clone(source.Aliases)
	clone.AliasPaths = slices.Clone(source.AliasPaths)
	for i, path := range clone.AliasPaths {
		clone.AliasPaths[i] = slices.Clone(path)
	}
	clone.Examples = slices.Clone(source.Examples)
	clone.Security = slices.Clone(source.Security)
	for i, requirement := range source.Security {
		if requirement == nil {
			continue
		}
		clone.Security[i] = make(map[string][]string, len(requirement))
		for scheme, scopes := range requirement {
			clone.Security[i][scheme] = slices.Clone(scopes)
		}
	}
	clone.Arguments = slices.Clone(source.Arguments)
	for i := range clone.Arguments {
		arg := &clone.Arguments[i]
		arg.Enum = slices.Clone(arg.Enum)
		arg.Variants = slices.Clone(arg.Variants)
		arg.RequiredFor = slices.Clone(arg.RequiredFor)
		if arg.Explode != nil {
			value := *arg.Explode
			arg.Explode = &value
		}
	}
	return &clone
}

var metadataDocument = sync.OnceValue(func() openAPIDocument {
	var doc openAPIDocument
	if err := json.Unmarshal(apispec.Startup().Metadata, &doc); err != nil {
		panic(err)
	}
	return doc
})

func loadOperationMetadata() openAPIDocument {
	if fullSnapshotLoaded.Load() {
		doc, _ := loadOpenAPI()
		return doc
	}
	return metadataDocument()
}
func operationMetadataByID(id string) (openAPIOperation, bool) {
	if fullSnapshotLoaded.Load() {
		return openAPIOperationByID(id)
	}
	op, ok := operationMetadata()[id]
	return op, ok
}

var operationMetadata = sync.OnceValue(func() map[string]openAPIOperation {
	result := map[string]openAPIOperation{}
	for _, methods := range loadOperationMetadata().Paths {
		for _, op := range methods {
			result[op.OperationID] = op
		}
	}
	return result
})

var schemaComponents sync.Map

func schemaComponent(name string) (map[string]any, bool) {
	if fullSnapshotLoaded.Load() {
		doc, _ := loadOpenAPI()
		node, ok := doc.Components.Schemas[name]
		return node, ok
	}
	if node, ok := schemaComponents.Load(name); ok {
		return node.(map[string]any), true
	}
	var node map[string]any
	if !apispec.Startup().Schema(name, &node) {
		return nil, false
	}
	actual, _ := schemaComponents.LoadOrStore(name, node)
	return actual.(map[string]any), true
}
func resolveComponentReference(ref string) (map[string]any, bool) {
	name, ok := strings.CutPrefix(ref, "#/components/schemas/")
	if !ok {
		return nil, false
	}
	name = strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
	return schemaComponent(name)
}

// Load only the definitions reachable from this command, including discriminator
// mapping targets. Full event/catalog inspection still uses loadOpenAPI.
func commandOpenAPI(cmd *Command) openAPIDocument {
	doc := openAPIDocument{}
	doc.Components.Schemas = map[string]map[string]any{}
	op, _ := openAPIOperationByID(cmd.OperationID)
	wanted := map[string]bool{}
	if cmd.InputSchema != "" {
		wanted[cmd.InputSchema] = true
	}
	if cmd.OutputSchema != "" {
		wanted[cmd.OutputSchema] = true
	}
	_, body := requestContentSchema(op.RequestBody)
	collectComponentRefs(body, wanted)
	collectComponentRefs(responseSchema(op.Responses), wanted)
	for _, parameter := range op.Parameters {
		collectComponentRefs(parameter["schema"], wanted)
	}
	for len(wanted) > 0 {
		var name string
		for name = range wanted {
			break
		}
		delete(wanted, name)
		if _, ok := doc.Components.Schemas[name]; ok {
			continue
		}
		if node, ok := schemaComponent(name); ok {
			doc.Components.Schemas[name] = node
			collectComponentRefs(node, wanted)
		}
	}
	return doc
}
