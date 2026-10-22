package cli

import (
	"sort"
	"strconv"
	"strings"
)

func bodyProperties(schema map[string]any, defs map[string]any, seen map[string]bool) (map[string]any, bool) {
	properties := map[string]any{}
	open := false
	if ref, ok := schema["$ref"].(string); ok && !seen[ref] {
		seen[ref] = true
		target, _ := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		inherited, allowed := bodyProperties(target, defs, seen)
		for key, value := range inherited {
			properties[key] = value
		}
		open = allowed
	}
	if own, ok := schema["properties"].(map[string]any); ok {
		for key, value := range own {
			properties[key] = value
		}
	}
	if extra, ok := schema["additionalProperties"]; ok {
		switch v := extra.(type) {
		case bool:
			open = open || v
		case map[string]any:
			open = true
		}
	}
	if patterns, ok := schema["patternProperties"].(map[string]any); ok && len(patterns) > 0 {
		open = true
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := schema[keyword].([]any)
		for _, raw := range branches {
			branch, _ := raw.(map[string]any)
			inherited, allowed := bodyProperties(branch, defs, seen)
			for key, value := range inherited {
				if previous, exists := properties[key]; exists {
					properties[key] = map[string]any{"anyOf": []any{previous, value}}
				} else {
					properties[key] = value
				}
			}
			open = open || allowed
		}
	}
	return properties, open
}

func unknownBodyFields(value any, schema map[string]any, defs map[string]any, path []string, depth int) []map[string]any {
	if depth > 64 {
		return nil
	}
	if ref, ok := schema["$ref"].(string); ok {
		if target, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any); ok {
			return unknownBodyFields(value, target, defs, path, depth+1)
		}
	}
	var problems []map[string]any
	switch value := value.(type) {
	case map[string]any:
		properties, open := bodyProperties(schema, defs, map[string]bool{})
		// Unconstrained JSON objects (metadata, for example) have no named fields.
		if len(properties) == 0 {
			return nil
		}
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			childPath := append(append([]string(nil), path...), key)
			property, known := properties[key]
			if !known && !open {
				problem := map[string]any{"path": jsonFieldPath(childPath), "message": "not a documented request field"}
				if suggestion := closestName(key, names); suggestion != "" {
					problem["suggestion"] = suggestion
					problem["message"] = "not a documented request field; did you mean " + suggestion + "?"
				}
				problems = append(problems, problem)
			} else if known {
				child, _ := property.(map[string]any)
				problems = append(problems, unknownBodyFields(value[key], child, defs, childPath, depth+1)...)
			}
		}
	case []any:
		items := bodyArrayItems(schema, defs, map[string]bool{})
		for i, child := range value {
			problems = append(problems, unknownBodyFields(child, items, defs, append(append([]string(nil), path...), strconv.Itoa(i)), depth+1)...)
		}
	}
	return problems
}

func bodyArrayItems(schema map[string]any, defs map[string]any, seen map[string]bool) map[string]any {
	if items, ok := schema["items"].(map[string]any); ok {
		return items
	}
	if ref, ok := schema["$ref"].(string); ok && !seen[ref] {
		seen[ref] = true
		target, _ := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		return bodyArrayItems(target, defs, seen)
	}
	items := []any{}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := schema[keyword].([]any)
		for _, raw := range branches {
			branch, _ := raw.(map[string]any)
			if item := bodyArrayItems(branch, defs, seen); item != nil {
				items = append(items, item)
			}
		}
	}
	if len(items) > 0 {
		return map[string]any{"anyOf": items}
	}
	return nil
}
