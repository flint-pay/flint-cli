package schemaview

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func readFixture(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func fieldNamed(t *testing.T, view View, name string) map[string]any {
	t.Helper()
	for _, field := range view.Fields {
		if field["name"] == name {
			return field
		}
	}
	t.Fatalf("missing %s in %+v", name, view)
	return nil
}
func TestResolveAllOfUnionAndSelectedVariant(t *testing.T) {
	schema := readFixture(t, `{"type":"object","properties":{"common":{"type":"string"}},"required":["common"],"allOf":[{"properties":{"extra":{"type":"integer"}},"required":["extra"]}],"oneOf":[{"properties":{"mode":{"type":"string","const":"a"},"a":{"type":"string"}},"required":["mode","a"]},{"properties":{"mode":{"type":"string","enum":["b"]},"b":{"type":"integer"}},"required":["mode","b"]}]}`)
	reader := New(schema)
	before, _ := json.Marshal(schema)
	view, err := reader.Describe(schema, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Variants == nil || view.Variants.Discriminator != "mode" || !reflect.DeepEqual(view.Variants.Requires["a"], []string{"a"}) {
		t.Fatalf("variants: %+v", view)
	}
	if fieldNamed(t, view, "common")["required"] != true || fieldNamed(t, view, "extra")["required"] != true || fieldNamed(t, view, "a")["required"] == true {
		t.Fatal(view)
	}
	if !reflect.DeepEqual(fieldNamed(t, view, "a")["variants"], []string{"a"}) {
		t.Fatal(view)
	}
	selected, err := reader.Describe(schema, Options{Variant: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if fieldNamed(t, selected, "a")["required"] != true || len(selected.Fields) != 4 {
		t.Fatalf("selected: %+v", selected)
	}
	if _, err = reader.Describe(schema, Options{Variant: "bad"}); err == nil {
		t.Fatal("unknown variant accepted")
	}
	after, _ := json.Marshal(schema)
	if string(before) != string(after) {
		t.Fatal("source mutated")
	}
}
func TestDescribePathsDepthEnumsAndRecursion(t *testing.T) {
	schema := readFixture(t, `{"type":"object","properties":{"items":{"type":"array","items":{"$ref":"#/$defs/node"}},"mode":{"type":"string","enum":[]}},"$defs":{"node":{"type":"object","properties":{"label":{"type":["string","null"]},"next":{"$ref":"#/$defs/node"}}}}}`)
	props := schemaMap(schema["properties"])
	values := []any{}
	for i := range 45 {
		values = append(values, i)
	}
	schemaMap(props["mode"])["enum"] = values
	reader := New(schema)
	view, err := reader.Describe(schema, Options{Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if fieldNamed(t, view, "mode")["enum_count"] != 45 || len(fieldNamed(t, view, "mode")["enum_sample"].([]any)) != 10 {
		t.Fatal(view)
	}
	opened, err := reader.Describe(schema, Options{Path: "mode"})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Fields[0]["enum"].([]any)) != 45 {
		t.Fatal(opened)
	}
	nested, err := reader.Describe(schema, Options{Path: "items.next"})
	if err != nil {
		t.Fatal(err)
	}
	if fieldNamed(t, nested, "label")["nullable"] != true {
		t.Fatal(nested)
	}
	if _, err := reader.Lookup(schema, "items.0.label"); err != nil {
		t.Fatal(err)
	}
	_, err = reader.Lookup(schema, "items.missing")
	var pathErr *PathError
	if !errors.As(err, &pathErr) || !reflect.DeepEqual(pathErr.Fields, []string{"label", "next"}) {
		t.Fatalf("path error: %v", err)
	}
	encoded, _ := json.Marshal(view)
	if len(encoded) > 3000 {
		t.Fatal("recursive view did not stop")
	}
	if _, err = reader.Describe(schema, Options{Depth: 4}); err == nil {
		t.Fatal("invalid depth accepted")
	}
}
func TestResolveUnlabelledUnionAndReferences(t *testing.T) {
	schema := readFixture(t, `{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"integer"}},"required":["b"]}]}`)
	view, err := New(schema).Describe(schema, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Variants != nil || len(view.OneOf) != 2 {
		t.Fatal(view)
	}
	if _, err = New(schema).Resolve(map[string]any{"$ref": "#/missing"}, ""); err == nil {
		t.Fatal("missing reference accepted")
	}
	escaped := readFixture(t, `{"$defs":{"a/b~c":{"type":"string"}}}`)
	resolved, err := New(escaped).Resolve(map[string]any{"$ref": "#/$defs/a~1b~0c"}, "")
	if err != nil || resolved.Schema["type"] != "string" {
		t.Fatal(resolved, err)
	}
	if text := compactText("First. Second. Third.", 2, 300); strings.Contains(text, "Third") {
		t.Fatal(text)
	}
}

func TestResolveArrayUnionAndBounds(t *testing.T) {
	schema := readFixture(t, `{"type":"object","properties":{"lines":{"anyOf":[{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"quantity":{"type":"integer"}},"required":["id","quantity"]}},{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"on_hand":{"type":"integer"}},"required":["id","on_hand"]}}]},"bounded":{"allOf":[{"type":"integer","minimum":3},{"type":"integer","minimum":1}]},"mixed":{"anyOf":[{"type":"integer","minimum":5},{"type":"integer"}]},"enum_array":{"type":"array","items":{"type":"string","enum":["a","b"]}}}}`)
	reader := New(schema)
	view, err := reader.Describe(schema, Options{Path: "lines"})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Fields) != 3 || fieldNamed(t, view, "id")["required"] != true || fieldNamed(t, view, "quantity")["required"] == true {
		t.Fatal(view)
	}
	view, err = reader.Describe(schema, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if fieldNamed(t, view, "bounded")["minimum"] != float64(3) || fieldNamed(t, view, "mixed")["minimum"] != nil {
		t.Fatal(view)
	}
	if len(fieldNamed(t, view, "enum_array")["enum"].([]any)) != 2 {
		t.Fatal("array enum lost")
	}
}

func TestConditionalExclusionKeepsIndependentFields(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"status": map[string]any{"type": "string"}, "signing_methods": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
		"not": map[string]any{"required": []string{"status"}, "anyOf": []any{map[string]any{"required": []string{"signing_methods"}}}},
	}
	reader := New(nil)
	flat, err := reader.Flatten(schema, 2, 600)
	if err != nil {
		t.Fatal(err)
	}
	props := flat["properties"].(map[string]any)
	if props["signing_methods"] == nil || props["status"] == nil {
		t.Fatalf("conditional exclusion removed independently valid fields: %v", props)
	}
}
