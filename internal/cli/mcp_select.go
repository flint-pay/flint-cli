package cli

import (
	"sort"
	"strconv"
	"strings"
)

func mcpSelectPath(field string) string {
	return strings.ReplaceAll(strings.ReplaceAll(field, "[]", ""), ".*.", ".")
}

func mcpValidateSelectPath(cmd *Command, field string) *CLIError {
	if runtimeOutputSchema(cmd) {
		return nil
	}
	schema, defs := resourceOutputSchema(cmd)
	path := mcpSelectPath(field)
	if strings.HasPrefix(path, "data.") {
		if _, declared, _ := outputSchemaPath(schema, defs, []string{"data"}, 0); !declared {
			return outputPathError(cmd, outputPath{field, "select"}, true, nil)
		}
	}
	if _, ok, names := outputSchemaPath(schema, defs, strings.Split(path, "."), 0, true); !ok {
		return outputPathError(cmd, outputPath{field, "select"}, false, names)
	}
	return nil
}

// A nil node selects the whole field. Selecting a parent wins over its children.
func mcpSelectTree(tree map[string]any, parts []string) {
	if len(parts) == 0 {
		return
	}
	if len(parts) == 1 {
		tree[parts[0]] = nil
		return
	}
	child, exists := tree[parts[0]]
	if exists && child == nil {
		return
	}
	if !exists {
		child = map[string]any{}
		tree[parts[0]] = child
	}
	mcpSelectTree(child.(map[string]any), parts[1:])
}

func mcpSelectObject(value any, tree map[string]any, prefix string, missing func(string)) any {
	if list, ok := value.([]any); ok {
		out := make([]any, len(list))
		for i, item := range list {
			// Numeric paths continue to address an individual array element.
			if branch, exists := tree[strconv.Itoa(i)]; exists {
				if branch == nil {
					out[i] = item
				} else {
					out[i] = mcpSelectObject(item, branch.(map[string]any), prefix+"."+strconv.Itoa(i), missing)
				}
			} else {
				out[i] = mcpSelectObject(item, tree, prefix, missing)
			}
		}
		return out
	}
	object, _ := value.(map[string]any)
	out := map[string]any{}
	// Sort paths so warnings stay stable across runs.
	keys := make([]string, 0, len(tree))
	for key := range tree {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		branch := tree[key]
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		item, present := object[key]
		if !present {
			missing(path)
			continue
		}
		if branch == nil {
			out[key] = item
			continue
		}
		if item == nil {
			missing(path)
			continue
		}
		out[key] = mcpSelectObject(item, branch.(map[string]any), path, missing)
	}
	return out
}
