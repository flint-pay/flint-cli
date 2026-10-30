package schemaview

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Flatten builds the compact advertised schema. Validation must use the original
// schema: deep and large object shapes are intentionally replaced with hints.
func (r *Reader) Flatten(schema map[string]any, depth, inlineBudget int) (map[string]any, error) {
	if depth <= 0 {
		depth = 2
	}
	if depth > 3 {
		depth = 3
	}
	if inlineBudget <= 0 {
		inlineBudget = 600
	}
	return r.flattenNode(schema, depth, inlineBudget, "", true)
}

func (r *Reader) flattenNode(schema map[string]any, depth, budget int, path string, root bool) (map[string]any, error) {
	resolved, err := r.Resolve(schema, "")
	if err != nil {
		return nil, fmt.Errorf("flatten %q: %w", path, err)
	}
	schema = resolved.Schema
	out := flattenScalars(schema)
	types := stringsOf(out["type"])
	if t, ok := out["type"].(string); ok {
		types = []string{t}
	}
	isObject, isArray := root || schema["properties"] != nil, schema["items"] != nil
	for _, t := range types {
		isObject = isObject || t == "object"
		isArray = isArray || t == "array"
	}
	if isObject {
		if root {
			out["type"] = "object"
		} else if out["type"] == nil {
			out["type"] = "object"
		}
		properties := schemaMap(schema["properties"])
		if !root && depth <= 0 {
			return flattenObjectHint(out, properties, path), nil
		}
		flatProperties := make(map[string]any, len(properties))
		for _, name := range sortedKeys(properties) {
			childPath := name
			if path != "" {
				childPath = path + "." + name
			}
			child, err := r.flattenNode(schemaMap(properties[name]), depth-1, budget, childPath, false)
			if err != nil {
				return nil, err
			}
			flatProperties[name] = child
		}
		out["properties"] = flatProperties
		required := stringsOf(schema["required"])
		if required == nil {
			required = []string{}
		}
		out["required"] = required
		if root || schema["additionalProperties"] == false {
			out["additionalProperties"] = false
		}
		if resolved.Variants != nil {
			flattenVariants(flatProperties, resolved.Variants)
		}
		if !root {
			encoded, err := json.Marshal(out)
			if err != nil {
				return nil, fmt.Errorf("flatten %q: %w", path, err)
			}
			if len(encoded) > budget {
				return flattenObjectHint(flattenScalars(schema), properties, path), nil
			}
		}
	} else if isArray {
		if out["type"] == nil {
			out["type"] = "array"
		}
		if depth < 0 {
			return out, nil
		}
		// Array items occupy the containing property's level, so an object
		// item opens at the same depth as a direct object property.
		itemSchema := schemaMap(schema["items"])
		item, err := r.Resolve(itemSchema, "")
		if err != nil {
			return nil, fmt.Errorf("flatten %q items: %w", path, err)
		}
		itemDepth := depth
		itemTypes := stringsOf(item.Schema["type"])
		if t, ok := item.Schema["type"].(string); ok {
			itemTypes = []string{t}
		}
		arrayItem := item.Schema["items"] != nil
		for _, t := range itemTypes {
			arrayItem = arrayItem || t == "array"
		}
		if arrayItem {
			itemDepth--
		}
		items, err := r.flattenNode(itemSchema, itemDepth, budget, path, false)
		if err != nil {
			return nil, err
		}
		out["items"] = items
	}
	return out, nil
}

func flattenScalars(schema map[string]any) map[string]any {
	out := make(map[string]any)
	for _, key := range []string{"type", "const", "default", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems"} {
		if value, ok := schema[key]; ok {
			out[key] = value
		}
	}
	if schema["nullable"] == true {
		switch t := out["type"].(type) {
		case string:
			if t != "null" {
				out["type"] = []string{t, "null"}
			}
		case []any:
			if !flattenHasNull(t) {
				out["type"] = append(append([]any{}, t...), "null")
			}
		case []string:
			values := make([]any, len(t))
			for i, value := range t {
				values[i] = value
			}
			if !flattenHasNull(values) {
				out["type"] = append(append([]string{}, t...), "null")
			}
		}
	}
	if description, ok := schema["description"].(string); ok && description != "" {
		out["description"] = flattenDescription(description)
	}
	if format, ok := schema["format"].(string); ok {
		switch format {
		case "date", "uri", "email", "date-time":
			out["format"] = format
		}
	}
	if values, ok := schema["enum"].([]any); ok {
		flattenEnum(out, values)
	} else if values, ok := schema["enum"].([]string); ok {
		items := make([]any, len(values))
		for i, value := range values {
			items[i] = value
		}
		flattenEnum(out, items)
	}
	return out
}

func flattenHasNull(types []any) bool {
	for _, t := range types {
		if t == "null" {
			return true
		}
	}
	return false
}

func flattenEnum(schema map[string]any, values []any) {
	if len(values) <= 40 {
		schema["enum"] = values
	} else {
		delete(schema, "enum")
		flattenNote(schema, fmt.Sprintf("%d allowed values; use flint_describe for the full enum.", len(values)))
	}
}

func flattenVariants(properties map[string]any, variants *Variants) {
	property := schemaMap(properties[variants.Discriminator])
	if property == nil || variants.Discriminator == "" {
		return
	}
	values := make([]string, 0, len(variants.Requires))
	for value := range variants.Requires {
		values = append(values, value)
	}
	sort.Strings(values)
	enum := make([]any, 0, len(values))
	parts := make([]string, 0, len(values))
	for _, value := range values {
		enum = append(enum, value)
		required := append([]string{}, variants.Requires[value]...)
		sort.Strings(required)
		needs := "no additional fields"
		if len(required) > 0 {
			needs = strings.Join(required, ", ")
		}
		parts = append(parts, value+" needs "+needs)
	}
	delete(property, "const")
	flattenEnum(property, enum)
	flattenNote(property, "Each value has its own fields: "+strings.Join(parts, "; ")+".")
}

func flattenObjectHint(schema, properties map[string]any, path string) map[string]any {
	out := make(map[string]any)
	if t := schema["type"]; t != nil {
		out["type"] = t
	} else {
		out["type"] = "object"
	}
	if description := schema["description"]; description != nil {
		out["description"] = description
	}
	names := sortedKeys(properties)
	if len(names) > 12 {
		names = names[:12]
	}
	suffix := "flint_describe path=" + path + "."
	for len(names) > 0 && len([]rune("Fields: "+strings.Join(names, ", ")+"; "+suffix)) > 160 {
		names = names[:len(names)-1]
	}
	if len(names) > 0 {
		suffix = "Fields: " + strings.Join(names, ", ") + "; " + suffix
	}
	flattenNote(out, suffix)
	return out
}

func flattenDescription(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	for offset := 0; offset < len(text); {
		i := strings.Index(text[offset:], ". ")
		if i < 0 {
			break
		}
		i += offset
		if !strings.HasSuffix(text[:i], "e.g") && !strings.HasSuffix(text[:i], "i.e") {
			text = text[:i+1]
			break
		}
		offset = i + 2
	}
	return flattenClip(text, 160)
}

func flattenNote(schema map[string]any, note string) {
	note = flattenClip(note, 160)
	text, _ := schema["description"].(string)
	if text != "" && len([]rune(note)) < 158 {
		text = flattenClip(text, 159-len([]rune(note))) + " "
	} else {
		text = ""
	}
	schema["description"] = text + note
}

func flattenClip(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return strings.TrimSpace(string(runes[:limit-1])) + "…"
	}
	return text
}
