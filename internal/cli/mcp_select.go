package cli

import (
	"maps"
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
		mapped := maps.Clone(tree)
		for key := range tree {
			if index, e := strconv.Atoi(key); e == nil {
				delete(mapped, key)
				if index < 0 || index >= len(list) {
					missing(prefix + "." + key)
				}
			}
		}
		out := make([]any, len(list))
		for i, item := range list {
			selected := mapped
			whole := false
			for key, branch := range tree {
				if index, e := strconv.Atoi(key); e == nil && index == i {
					if branch == nil {
						whole = true
						break
					}
					selected = mcpMergeSelectTree(selected, branch.(map[string]any))
				}
			}
			if whole {
				out[i] = item
				continue
			}
			// Project the combined paths, then report missing indexed paths against
			// their original selector rather than inventing an object key "0".
			out[i] = mcpSelectObject(item, selected, prefix, func(path string) {
				suffix := strings.TrimPrefix(path, prefix+".")
				root := strings.Split(suffix, ".")[0]
				if _, mappedField := mapped[root]; mappedField {
					missing(path)
				}
				for key, branch := range tree {
					if index, e := strconv.Atoi(key); e == nil && index == i && branch != nil {
						if _, indexedField := branch.(map[string]any)[root]; indexedField {
							missing(prefix + "." + key + "." + suffix)
						}
					}
				}
			})
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

func mcpMergeSelectTree(left, right map[string]any) map[string]any {
	out := maps.Clone(left)
	for key, value := range right {
		prior, exists := out[key]
		if !exists {
			out[key] = value
		} else if prior == nil || value == nil {
			out[key] = nil
		} else {
			out[key] = mcpMergeSelectTree(prior.(map[string]any), value.(map[string]any))
		}
	}
	return out
}
