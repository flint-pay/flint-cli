package schemaview

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

type Options struct {
	Path, Variant string
	Depth         int
}
type View struct {
	Fields   []map[string]any `json:"fields"`
	Variants *Variants        `json:"variants,omitempty"`
	OneOf    [][]string       `json:"one_of,omitempty"`
}

func (r *Reader) Describe(schema map[string]any, opts Options) (View, error) {
	if opts.Depth == 0 {
		opts.Depth = 1
	}
	if opts.Depth < 1 || opts.Depth > 3 {
		return View{}, fmt.Errorf("depth must be from 1 to 3")
	}
	resolved, err := r.Resolve(schema, opts.Variant)
	if err != nil {
		return View{}, err
	}
	node := resolved.Schema
	if opts.Path != "" {
		resolved = Resolved{}
		node, err = r.Lookup(node, opts.Path)
		if err != nil {
			return View{}, err
		}
	}
	return r.describe(node, opts.Depth, opts.Path, opts.Path != "", map[string]bool{}, resolved)
}
func (r *Reader) describe(schema map[string]any, depth int, path string, fullEnum bool, ancestors map[string]bool, metadata Resolved) (View, error) {
	resolved, err := r.Resolve(schema, "")
	if err != nil {
		return View{}, err
	}
	node := resolved.Schema
	if items := schemaMap(node["items"]); items != nil {
		resolved, err = r.Resolve(items, "")
		if err != nil {
			return View{}, err
		}
		node = resolved.Schema
	}
	if metadata.Variants == nil && metadata.OneOf == nil {
		metadata = resolved
	}
	out := View{Fields: []map[string]any{}, Variants: metadata.Variants, OneOf: metadata.OneOf}
	props := schemaMap(node["properties"])
	if len(props) == 0 && node["type"] != "object" {
		name := path
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		if name == "" {
			name = "value"
		}
		field, err := r.field(name, node, false, depth, path, fullEnum, ancestors)
		if err != nil {
			return out, err
		}
		out.Fields = append(out.Fields, field)
		return out, nil
	}
	for _, name := range sortedKeys(props) {
		childPath := name
		if path != "" {
			childPath = path + "." + name
		}
		field, err := r.field(name, schemaMap(props[name]), slices.Contains(stringsOf(node["required"]), name), depth, childPath, false, ancestors)
		if err != nil {
			return out, err
		}
		if values := metadata.Membership[name]; len(values) > 0 {
			field["variants"] = values
		}
		out.Fields = append(out.Fields, field)
	}
	sort.SliceStable(out.Fields, func(i, j int) bool {
		a, b := out.Fields[i]["required"] == true, out.Fields[j]["required"] == true
		if a != b {
			return a
		}
		return out.Fields[i]["name"].(string) < out.Fields[j]["name"].(string)
	})
	return out, nil
}
func (r *Reader) field(name string, schema map[string]any, required bool, depth int, path string, fullEnum bool, ancestors map[string]bool) (map[string]any, error) {
	resolved, err := r.Resolve(schema, "")
	if err != nil {
		return nil, err
	}
	node := resolved.Schema
	field := map[string]any{"name": name, "type": displayType(node)}
	if required {
		field["required"] = true
	}
	if node["nullable"] == true || slices.Contains(stringsOf(node["type"]), "null") {
		field["nullable"] = true
	}
	if description, _ := node["description"].(string); description != "" {
		field["description"] = compactText(description, 2, 300)
	}
	for _, key := range []string{"default", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems", "format"} {
		if v, ok := node[key]; ok {
			field[key] = v
		}
	}
	values := fixedValues(node)
	if items := schemaMap(node["items"]); items != nil {
		item, err := r.Resolve(items, "")
		if err != nil {
			return nil, err
		}
		field["type"] = "array of " + displayType(item.Schema)
		if len(values) == 0 {
			values = fixedValues(item.Schema)
		}
		if format, ok := item.Schema["format"]; ok {
			field["format"] = format
		}
	}
	if len(values) > 40 && !fullEnum {
		field["enum_count"] = len(values)
		field["enum_sample"] = values[:10]
	} else if len(values) > 0 {
		field["enum"] = values
	}
	nested := node
	if items := schemaMap(node["items"]); items != nil {
		resolved, err = r.Resolve(items, "")
		if err != nil {
			return nil, err
		}
		nested = resolved.Schema
	}
	if nested["type"] == "object" || nested["properties"] != nil {
		ref, _ := schema["$ref"].(string)
		if depth <= 1 || ref != "" && ancestors[ref] {
			field["fields_count"] = len(schemaMap(nested["properties"]))
		} else {
			next := maps.Clone(ancestors)
			if ref != "" {
				next[ref] = true
			}
			view, err := r.describe(nested, depth-1, path, false, next, Resolved{})
			if err != nil {
				return nil, err
			}
			field["fields"] = view.Fields
			if view.Variants != nil {
				field["variant_requirements"] = view.Variants
			}
			if view.OneOf != nil {
				field["one_of"] = view.OneOf
			}
		}
	}
	return field, nil
}
func displayType(schema map[string]any) string {
	types := []string{}
	for _, name := range stringsOf(schema["type"]) {
		if name != "null" {
			types = append(types, name)
		}
	}
	if len(types) == 0 {
		if schema["properties"] != nil {
			types = []string{"object"}
		} else if schema["items"] != nil {
			types = []string{"array"}
		} else {
			types = []string{"any"}
		}
	}
	if slices.Contains(types, "array") {
		itemType := displayType(schemaMap(schema["items"]))
		if itemType == "any" {
			itemType = "object"
		}
		return "array of " + itemType
	}
	return strings.Join(types, " or ")
}
func compactText(text string, sentences, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	end := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '.' && (i+1 == len(text) || text[i+1] == ' ') {
			prefix := text[:i]
			if strings.HasSuffix(prefix, "e.g") || strings.HasSuffix(prefix, "i.e") {
				continue
			}
			sentences--
			if sentences == 0 {
				end = i + 1
				break
			}
		}
	}
	if end > 0 {
		text = text[:end]
	}
	runes := []rune(text)
	if len(runes) > limit {
		text = strings.TrimSpace(string(runes[:limit-3])) + "..."
	}
	return text
}
