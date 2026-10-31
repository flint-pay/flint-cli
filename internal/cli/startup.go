package cli

import (
	"encoding/json"
	"os"
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

func indexedRegistry(includeAPI bool) *Registry {
	var commands []startupCommand
	raw := apispec.Startup().Commands
	if !includeAPI {
		raw = apispec.Startup().LocalCommands
	}
	if err := json.Unmarshal(raw, &commands); err != nil {
		panic(err)
	}
	registry := &Registry{treeBuild: &sync.Once{}, localOnly: !includeAPI, byName: map[string]*Command{}, byPath: map[string]*Command{}}
	for _, stored := range commands {
		cmd := stored.Command
		if !includeAPI && !cmd.Local {
			continue
		}
		cmd.Render, cmd.MCPHidden = stored.Render, stored.MCPHidden
		for i, generated := range stored.Generated {
			cmd.Arguments[i].bodyGenerated = generated
		}
		registry.add(cmd)
	}
	registry.treeSnapshot()
	return registry
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
