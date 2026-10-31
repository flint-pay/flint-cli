// Package schemaview builds compact discovery views of JSON Schema. These views
// are intentionally lossy; execution must validate against the original schema.
package schemaview

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type Reader struct{ root map[string]any }

func New(root map[string]any) *Reader { return &Reader{root: root} }

type Variants struct {
	Discriminator string              `json:"discriminator"`
	Requires      map[string][]string `json:"requires"`
}
type Resolved struct {
	Schema     map[string]any
	Variants   *Variants
	OneOf      [][]string
	Membership map[string][]string
	forbidden  []string
	selected   bool
}

// Resolve merges the current level, leaving child schemas lazy to handle cycles.
func (r *Reader) Resolve(schema map[string]any, variant string) (Resolved, error) {
	out, err := r.resolve(schema, variant, map[string]bool{}, 0)
	if err == nil && variant != "" && !out.selected {
		err = fmt.Errorf("variant %q is not available", variant)
	}
	return out, err
}
func (r *Reader) resolve(schema map[string]any, variant string, seen map[string]bool, depth int) (Resolved, error) {
	if depth > 64 {
		return Resolved{Schema: map[string]any{"type": "object"}}, nil
	}
	base := maps.Clone(schema)
	if base == nil {
		base = map[string]any{}
	}
	result := Resolved{Schema: base}
	if ref, ok := base["$ref"].(string); ok {
		delete(base, "$ref")
		if seen[ref] {
			result.Schema = conjoin(map[string]any{"type": "object"}, base)
			return result, nil
		}
		target, ok := r.reference(ref)
		if !ok {
			return result, fmt.Errorf("schema reference %q is not available", ref)
		}
		next := maps.Clone(seen)
		next[ref] = true
		resolved, err := r.resolve(target, variant, next, depth+1)
		if err != nil {
			return result, err
		}
		base = conjoin(resolved.Schema, base)
		result = resolved
		result.Schema = base
	}
	for _, raw := range arrayOf(base["allOf"]) {
		part, err := r.resolve(schemaMap(raw), variant, maps.Clone(seen), depth+1)
		if err != nil {
			return result, err
		}
		result.Schema = conjoin(result.Schema, part.Schema)
		result.forbidden = append(result.forbidden, part.forbidden...)
		if part.Variants != nil {
			result.Variants = part.Variants
			result.Membership = part.Membership
		}
		if part.OneOf != nil {
			result.OneOf = part.OneOf
		}
		result.selected = result.selected || part.selected
	}
	base = result.Schema
	result.forbidden = append(result.forbidden, forbiddenFields(base)...)
	branches := arrayOf(base["oneOf"])
	if len(branches) == 0 {
		branches = arrayOf(base["anyOf"])
	}
	if len(branches) > 0 {
		effective := make([]map[string]any, len(branches))
		for i, raw := range branches {
			part, err := r.resolve(schemaMap(raw), "", maps.Clone(seen), depth+1)
			if err != nil {
				return result, err
			}
			effective[i] = conjoin(base, part.Schema)
			// A closed branch defines its own allowed fields. Open constraint branches
			// inherit the parent's fields, as in conditional inventory schemas.
			if part.Schema["additionalProperties"] == false {
				props := maps.Clone(schemaMap(effective[i]["properties"]))
				allowed := schemaMap(part.Schema["properties"])
				for name := range props {
					if _, ok := allowed[name]; !ok {
						delete(props, name)
					}
				}
				effective[i]["properties"] = props
			}
			removeFields(effective[i], part.forbidden)
		}
		discriminator := r.discriminatorName(base, effective)
		labels := make([][]string, len(effective))
		mapping := schemaMap(schemaMap(base["discriminator"])["mapping"])
		for i, branch := range effective {
			if discriminator != "" {
				labels[i] = r.fixedStrings(schemaMap(schemaMap(branch["properties"])[discriminator]))
			}
			// Explicit OpenAPI mappings can label branches without const/enum.
			if len(labels[i]) == 0 {
				for label, ref := range mapping {
					if ref == schemaMap(branches[i])["$ref"] {
						labels[i] = append(labels[i], label)
					}
				}
				sort.Strings(labels[i])
			}
		}
		if variant != "" && discriminator != "" {
			found := false
			for i, values := range labels {
				if slices.Contains(values, variant) {
					result.Schema = effective[i]
					result.selected = true
					result.Variants = nil
					result.OneOf = nil
					result.Membership = nil
					found = true
					break
				}
			}
			if !found {
				return result, fmt.Errorf("unknown variant %q; available values: %s", variant, strings.Join(allLabels(labels), ", "))
			}
		} else {
			merged := effective[0]
			for _, branch := range effective[1:] {
				merged = unite(merged, branch)
			}
			result.Schema = merged
			if discriminator != "" {
				common := stringsOf(merged["required"])
				requires := map[string][]string{}
				membership := map[string][]string{}
				for i, branch := range effective {
					extra := []string{}
					for _, name := range stringsOf(branch["required"]) {
						if !slices.Contains(common, name) {
							extra = append(extra, name)
						}
					}
					for _, label := range labels[i] {
						requires[label] = append([]string{}, extra...)
					}
					for name := range schemaMap(branch["properties"]) {
						membership[name] = append(membership[name], labels[i]...)
					}
				}
				values := allLabels(labels)
				props := maps.Clone(schemaMap(merged["properties"]))
				disc := maps.Clone(schemaMap(props[discriminator]))
				if disc == nil {
					disc = map[string]any{"type": "string"}
				}
				delete(disc, "const")
				disc["enum"] = values
				props[discriminator] = disc
				merged["properties"] = props
				for name, valuesForField := range membership {
					valuesForField = unique(valuesForField)
					if len(valuesForField) == len(values) {
						delete(membership, name)
					} else {
						membership[name] = valuesForField
					}
				}
				result.Variants = &Variants{discriminator, requires}
				result.Membership = membership
			} else {
				result.OneOf = [][]string{}
				for _, branch := range effective {
					result.OneOf = append(result.OneOf, stringsOf(branch["required"]))
				}
			}
		}
	}
	removeFields(result.Schema, result.forbidden)
	for _, key := range []string{"$ref", "$defs", "definitions", "allOf", "oneOf", "anyOf", "not", "if", "then", "else", "discriminator", "example", "examples", "writeOnly", "readOnly", "xml", "externalDocs", "deprecated"} {
		delete(result.Schema, key)
	}
	for key := range result.Schema {
		if strings.HasPrefix(key, "x-") {
			delete(result.Schema, key)
		}
	}
	return result, nil
}
func (r *Reader) reference(ref string) (map[string]any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var value any = r.root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		value = schemaMap(value)[part]
	}
	node, ok := value.(map[string]any)
	return node, ok
}
func conjoin(a, b map[string]any) map[string]any {
	out := maps.Clone(a)
	if out == nil {
		out = map[string]any{}
	}
	for key, value := range b {
		switch key {
		case "properties":
			props := maps.Clone(schemaMap(out[key]))
			if props == nil {
				props = map[string]any{}
			}
			for name, child := range schemaMap(value) {
				if old, ok := props[name]; ok {
					if !equalSchema(old, child) {
						props[name] = map[string]any{"allOf": []any{old, child}}
					}
				} else {
					props[name] = child
				}
			}
			out[key] = props
		case "required":
			out[key] = unique(append(stringsOf(out[key]), stringsOf(value)...))
		default:
			if tighter, ok := mergeBound(key, out[key], value, true); ok {
				out[key] = tighter
			} else {
				out[key] = value
			}
		}
	}
	return out
}
func unite(a, b map[string]any) map[string]any {
	out := maps.Clone(a)
	props := maps.Clone(schemaMap(a["properties"]))
	if props == nil {
		props = map[string]any{}
	}
	for name, child := range schemaMap(b["properties"]) {
		if old, ok := props[name]; ok {
			if !equalSchema(old, child) {
				props[name] = map[string]any{"anyOf": []any{old, child}}
			}
		} else {
			props[name] = child
		}
	}
	if len(props) > 0 {
		out["properties"] = props
	}
	required := []string{}
	for _, name := range stringsOf(a["required"]) {
		if slices.Contains(stringsOf(b["required"]), name) {
			required = append(required, name)
		}
	}
	out["required"] = required
	if left, right := schemaMap(a["items"]), schemaMap(b["items"]); left != nil && right != nil {
		if !equalSchema(left, right) {
			out["items"] = map[string]any{"anyOf": []any{left, right}}
		}
	} else if right != nil {
		out["items"] = right
	}
	if a["additionalProperties"] != false || b["additionalProperties"] != false {
		delete(out, "additionalProperties")
	}
	for _, key := range []string{"minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems"} {
		left, right := a[key], b[key]
		if left == nil || right == nil {
			delete(out, key)
		} else if value, ok := mergeBound(key, left, right, false); ok {
			out[key] = value
		}
	}
	for _, key := range []string{"format", "default"} {
		if !reflect.DeepEqual(a[key], b[key]) {
			delete(out, key)
		}
	}
	// Scalar alternatives keep all possible types and fixed enum values.
	types := unique(append(slices.Clone(stringsOf(a["type"])), stringsOf(b["type"])...))
	if len(types) == 1 {
		out["type"] = types[0]
	} else if len(types) > 1 {
		out["type"] = types
	}
	av, bv := fixedValues(a), fixedValues(b)
	delete(out, "const")
	if len(av) > 0 && len(bv) > 0 {
		out["enum"] = uniqueValues(append(slices.Clone(av), bv...))
	} else {
		delete(out, "enum")
	}
	return out
}
func (r *Reader) discriminatorName(base map[string]any, branches []map[string]any) string {
	if name, ok := schemaMap(base["discriminator"])["propertyName"].(string); ok {
		return name
	}
	if len(branches) == 0 {
		return ""
	}
	for _, name := range sortedKeys(schemaMap(branches[0]["properties"])) {
		values := map[string]bool{}
		valid := true
		for _, branch := range branches {
			fixed := r.fixedStrings(schemaMap(schemaMap(branch["properties"])[name]))
			if len(fixed) == 0 {
				valid = false
				break
			}
			for _, v := range fixed {
				if values[v] {
					valid = false
				}
				values[v] = true
			}
		}
		if valid {
			return name
		}
	}
	return ""
}
func forbiddenFields(schema map[string]any) []string {
	not := schemaMap(schema["not"])
	// Only unconditional exclusions remove fields from a discovery view.
	// not:{required:[status],anyOf:[{required:[name]}]} forbids the
	// combination, while both fields remain valid on their own.
	if len(not) != 1 {
		return nil
	}
	sets := arrayOf(not["anyOf"])
	if len(sets) == 0 {
		sets = []any{not}
	}
	out := []string{}
	for _, raw := range sets {
		branch := schemaMap(raw)
		required := stringsOf(branch["required"])
		if len(branch) == 1 && len(required) == 1 {
			out = append(out, required[0])
		}
	}
	return out
}
func removeFields(schema map[string]any, names []string) {
	if len(names) == 0 {
		return
	}
	props := maps.Clone(schemaMap(schema["properties"]))
	for _, name := range names {
		delete(props, name)
	}
	schema["properties"] = props
}
func fixedValues(schema map[string]any) []any {
	if v, ok := schema["const"]; ok {
		return []any{v}
	}
	return arrayOf(schema["enum"])
}
func fixedStrings(schema map[string]any) []string {
	out := []string{}
	for _, v := range fixedValues(schema) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		} else {
			return nil
		}
	}
	return out
}
func allLabels(labels [][]string) []string {
	out := []string{}
	for _, values := range labels {
		out = append(out, values...)
	}
	return unique(out)
}
func unique(values []string) []string { sort.Strings(values); return slices.Compact(values) }
func stringsOf(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []string:
		return append([]string{}, v...)
	case []any:
		out := []string{}
		for _, raw := range v {
			if s, ok := raw.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
func arrayOf(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out
	}
	return nil
}
func schemaMap(value any) map[string]any { node, _ := value.(map[string]any); return node }
func sortedKeys(m map[string]any) []string {
	out := []string{}
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
func uniqueValues(values []any) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, v := range values {
		key := fmt.Sprintf("%T:%v", v, v)
		if !seen[key] {
			out = append(out, v)
			seen[key] = true
		}
	}
	return out
}
func equalSchema(a, b any) bool { return reflect.DeepEqual(a, b) }

// Lookup follows declared fields. Array paths may omit the numeric item index
// for describe, or include it for output selectors. Maps admit arbitrary keys.
func (r *Reader) Lookup(schema map[string]any, path string) (map[string]any, error) {
	node := schema
	if path == "" {
		return node, nil
	}
	for _, part := range strings.Split(path, ".") {
		resolved, err := r.Resolve(node, "")
		if err != nil {
			return nil, err
		}
		node = resolved.Schema
		if items := schemaMap(node["items"]); items != nil {
			node = items
			if index, err := strconv.Atoi(part); err == nil && index >= 0 {
				continue
			}
			resolved, err = r.Resolve(node, "")
			if err != nil {
				return nil, err
			}
			node = resolved.Schema
		}
		props := schemaMap(node["properties"])
		if child := schemaMap(props[part]); child != nil {
			node = child
			continue
		}
		if child := schemaMap(node["additionalProperties"]); child != nil {
			node = child
			continue
		}
		if node["additionalProperties"] == true || len(props) == 0 && node["type"] == "object" && node["additionalProperties"] != false || len(node) == 0 {
			node = map[string]any{}
			continue
		}
		return nil, &PathError{Path: path, Segment: part, Fields: sortedKeys(props)}
	}
	return node, nil
}

type PathError struct {
	Path, Segment string
	Fields        []string
}

func (e *PathError) Error() string {
	return fmt.Sprintf("unknown path segment %q in %q; fields: %s", e.Segment, e.Path, strings.Join(e.Fields, ", "))
}

func (r *Reader) fixedStrings(schema map[string]any) []string {
	var visit func(map[string]any, map[string]bool, int) []string
	visit = func(node map[string]any, seen map[string]bool, depth int) []string {
		if depth > 32 {
			return nil
		}
		values := fixedStrings(node)
		mergeValues := func(part []string) {
			if len(part) == 0 {
				return
			}
			if values == nil {
				values = append([]string{}, part...)
				return
			}
			common := []string{}
			for _, value := range values {
				if slices.Contains(part, value) {
					common = append(common, value)
				}
			}
			values = common
		}
		if ref, ok := node["$ref"].(string); ok {
			if seen[ref] {
				return nil
			}
			next := maps.Clone(seen)
			next[ref] = true
			target, ok := r.reference(ref)
			if !ok {
				return nil
			}
			mergeValues(visit(target, next, depth+1))
		}
		for _, raw := range arrayOf(node["allOf"]) {
			mergeValues(visit(schemaMap(raw), seen, depth+1))
		}
		if values != nil {
			return unique(values)
		}
		for _, key := range []string{"oneOf", "anyOf"} {
			branches := arrayOf(node[key])
			if len(branches) == 0 {
				continue
			}
			values := []string{}
			for _, raw := range branches {
				part := visit(schemaMap(raw), seen, depth+1)
				if len(part) == 0 {
					return nil
				}
				values = append(values, part...)
			}
			return unique(values)
		}
		return nil
	}
	return visit(schema, map[string]bool{}, 0)
}

func mergeBound(key string, a, b any, intersection bool) (any, bool) {
	left, lok := numberOf(a)
	right, rok := numberOf(b)
	if !lok || !rok {
		return nil, false
	}
	lower := key == "minimum" || key == "minLength" || key == "minItems"
	if !lower && key != "maximum" && key != "maxLength" && key != "maxItems" {
		return nil, false
	}
	takeLarger := lower == intersection
	if (left > right) == takeLarger {
		return a, true
	}
	return b, true
}
func numberOf(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}
