package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type outputPath struct{ Path, Param string }

// Timeline NDJSON emits entries as resources, while JSON returns the timeline.
func outputCommand(cmd *Command, opts Options) *Command {
	if cmd != nil && opts.Output == "ndjson" && cmd.OperationID == "getResourceTimeline" {
		entry := *cmd
		entry.OperationID = ""
		entry.OutputSchema = "ResourceTimelineEntry"
		return &entry
	}
	return cmd
}

func outputPaths(opts Options) ([]outputPath, *CLIError) {
	paths := []outputPath{}
	if opts.Field != "" {
		paths = append(paths, outputPath{opts.Field, "field"})
	}
	for _, field := range opts.Select {
		paths = append(paths, outputPath{field, "select"})
	}
	conditions, e := parseWaitConditions(opts.WaitFor)
	if e != nil {
		return nil, e
	}
	for _, condition := range conditions {
		paths = append(paths, outputPath{condition.Path, "wait-for"})
	}
	return paths, nil
}

func resourceRoot(value any) any {
	if envelope, ok := value.(map[string]any); ok {
		if data, exists := envelope["data"]; exists {
			return data
		}
	}
	return value
}

func outputResources(value any) []any {
	root := resourceRoot(value)
	if list, ok := root.([]any); ok {
		return list
	}
	return []any{root}
}

func runtimeOutputSchema(cmd *Command) bool {
	return cmd == nil || cmd.Local || cmd.OperationID == "" && cmd.OutputSchema == ""
}

func resourceOutputSchema(cmd *Command) (map[string]any, map[string]any) {
	if runtimeOutputSchema(cmd) {
		return nil, nil
	}
	schema, e := schemaForCommand(cmd, false)
	if e != nil {
		return nil, nil
	}
	defs, _ := schema["$defs"].(map[string]any)
	root := schema
	if data, ok, _ := outputSchemaPath(root, defs, []string{"data"}, 0); ok {
		root = data
	}
	if items, ok := root["items"].(map[string]any); ok {
		root = items
	}
	return root, defs
}

// outputSchemaPath follows declared properties across response variants. Schema
// objects with additionalProperties admit user-defined keys; closed objects don't.
func outputSchemaPath(node map[string]any, defs map[string]any, parts []string, depth int) (map[string]any, bool, []string) {
	if node == nil || depth > 64 {
		return nil, false, nil
	}
	if ref, ok := node["$ref"].(string); ok {
		target, _ := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		return outputSchemaPath(target, defs, parts, depth+1)
	}
	if len(parts) == 0 {
		return node, true, nil
	}
	names := map[string]bool{}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := node[keyword].([]any)
		for _, raw := range branches {
			branch, _ := raw.(map[string]any)
			child, ok, candidates := outputSchemaPath(branch, defs, parts, depth+1)
			if ok {
				return child, true, nil
			}
			for _, name := range candidates {
				names[name] = true
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		index, err := strconv.Atoi(parts[0])
		if err != nil || index < 0 {
			return nil, false, []string{"0"}
		}
		return outputSchemaPath(items, defs, parts[1:], depth+1)
	}
	properties, _ := node["properties"].(map[string]any)
	for name := range properties {
		names[name] = true
	}
	if child, ok := properties[parts[0]].(map[string]any); ok {
		return outputSchemaPath(child, defs, parts[1:], depth+1)
	}
	if extra, ok := node["additionalProperties"].(map[string]any); ok {
		return outputSchemaPath(extra, defs, parts[1:], depth+1)
	}
	if node["additionalProperties"] == true || len(node) == 0 || len(properties) == 0 && node["type"] == "object" && node["additionalProperties"] != false && node["allOf"] == nil && node["anyOf"] == nil && node["oneOf"] == nil {
		return map[string]any{}, true, nil
	}
	candidates := []string{}
	for name := range names {
		candidates = append(candidates, name)
	}
	sort.Strings(candidates)
	return nil, false, candidates
}

func outputPathError(cmd *Command, path outputPath, root bool, candidates []string) *CLIError {
	if root {
		replacement := strings.TrimPrefix(path.Path, "data.")
		e := usageError("OUTPUT_FIELD_ROOT", fmt.Sprintf("Paths start inside the resource: use --%s %s. For the whole response, use --jq '.%s'.", path.Param, replacement, path.Path), path.Param)
		e.Details = suggestionDetails(path.Path, []string{replacement})
		return e
	}
	last := path.Path[strings.LastIndex(path.Path, ".")+1:]
	suggestions := []string{}
	message := fmt.Sprintf("%s isn't a field of this resource.", path.Path)
	if cmd != nil {
		message = fmt.Sprintf("%s isn't a field of %s.", path.Path, strings.ReplaceAll(cmd.CanonicalName, ".", " "))
	}
	if closest := closestName(last, candidates); closest != "" {
		prefix := strings.TrimSuffix(path.Path, last)
		suggestions = append(suggestions, prefix+closest)
		message += " Did you mean " + prefix + closest + "?"
	}
	e := usageError("UNKNOWN_OUTPUT_FIELD", message, path.Param)
	e.Details = suggestionDetails(path.Path, suggestions)
	return e
}

func validateOutputPaths(cmd *Command, paths []outputPath) *CLIError {
	if len(paths) == 0 || runtimeOutputSchema(cmd) {
		return nil
	}
	schema, defs := resourceOutputSchema(cmd)
	if schema == nil {
		return nil
	}
	for _, path := range paths {
		if strings.HasPrefix(path.Path, "data.") {
			if _, declared, _ := outputSchemaPath(schema, defs, []string{"data"}, 0); !declared {
				return outputPathError(cmd, path, true, nil)
			}
		}
		if _, ok, names := outputSchemaPath(schema, defs, strings.Split(path.Path, "."), 0); !ok {
			return outputPathError(cmd, path, false, names)
		}
	}
	return nil
}

func outputSchemaDeclaresPath(cmd *Command, path string) bool {
	if cmd == nil {
		return false
	}
	schema, defs := resourceOutputSchema(cmd)
	_, ok, _ := outputSchemaPath(schema, defs, strings.Split(path, "."), 0)
	return ok
}

func prepareOptionalFields(value any, cmd *Command, opts Options) (any, *CLIError) {
	normalized, e := normalizeOutput(value)
	if e != nil {
		return nil, e
	}
	paths, e := outputPaths(opts)
	if e != nil {
		return nil, e
	}
	if e := validateOutputPaths(cmd, paths); e != nil {
		return nil, e
	}
	resources := outputResources(normalized)
	for _, path := range paths {
		if runtimeOutputSchema(cmd) {
			found := len(resources) == 0
			names := map[string]bool{}
			for _, resource := range resources {
				if _, ok := lookupPath(resource, path.Path); ok {
					found = true
				}
				if object, ok := resource.(map[string]any); ok {
					for name := range object {
						names[name] = true
					}
				}
			}
			if !found {
				candidates := []string{}
				for name := range names {
					candidates = append(candidates, name)
				}
				sort.Strings(candidates)
				return nil, outputPathError(cmd, path, strings.HasPrefix(path.Path, "data.") && !names["data"], candidates)
			}
		}
		for _, resource := range resources {
			if _, exists := lookupPath(resource, path.Path); !exists {
				if object, ok := resource.(map[string]any); ok {
					if err := setMissingOutputField(object, path.Path); err != nil {
						return nil, outputPathError(cmd, path, false, nil)
					}
				}
			}
		}
	}
	return normalized, nil
}

func setMissingOutputField(resource map[string]any, field string) error {
	parts := strings.Split(field, ".")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], ".")
		if value, exists := lookupPath(resource, parent); exists && value == nil {
			var container any = map[string]any{}
			if _, err := strconv.Atoi(parts[i]); err == nil {
				container = []any{}
			}
			if err := setBodyPath(resource, parent, container); err != nil {
				return err
			}
		}
	}
	return setBodyPath(resource, field, nil)
}
