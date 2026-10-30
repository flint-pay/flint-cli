package schemaview

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFlattenStrictRootAndSafeKeywords(t *testing.T) {
	source := map[string]any{"type": "object", "properties": map[string]any{
		"when":  map[string]any{"type": "string", "format": "date-time", "description": "First sentence. More detail.", "pattern": "^x", "example": "x", "x-extra": true, "writeOnly": true},
		"count": map[string]any{"type": "integer", "minimum": 1, "maximum": 9, "default": 2},
		"id":    map[string]any{"type": "string", "format": "uuid", "minLength": 1, "maxLength": 99, "const": "fixed"},
		"value": map[string]any{"type": []any{"string", "null"}, "enum": []any{"a", "b", nil}},
	}, "required": []any{"when"}, "additionalProperties": true}
	flat, err := New(source).Flatten(source, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flat["type"] != "object" || flat["additionalProperties"] != false {
		t.Fatalf("root: %#v", flat)
	}
	if !reflect.DeepEqual(stringsOf(flat["required"]), []string{"when"}) {
		t.Fatalf("required: %#v", flat["required"])
	}
	props := schemaMap(flat["properties"])
	when := schemaMap(props["when"])
	if when["description"] != "First sentence." || when["format"] != "date-time" {
		t.Fatalf("when: %#v", when)
	}
	if schemaMap(props["id"])["format"] != nil {
		t.Fatal("unsupported format retained")
	}
	if !reflect.DeepEqual(stringsOf(schemaMap(props["value"])["type"]), []string{"string", "null"}) {
		t.Fatal("nullable type lost")
	}
	flattenAssertNoKeywords(t, flat)
}

func TestFlattenDepthArrayItemsAndBudget(t *testing.T) {
	money := map[string]any{"type": "object", "properties": map[string]any{"amount": map[string]any{"type": "integer"}, "currency": map[string]any{"type": "string"}}, "required": []any{"amount", "currency"}}
	source := map[string]any{"type": "object", "properties": map[string]any{
		"money": money,
		"lines": map[string]any{"type": "array", "items": money, "minItems": 1},
		"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}}
	reader := New(source)
	for _, tc := range []struct {
		depth, budget int
		inline        bool
	}{{1, 600, false}, {2, 600, true}, {2, 40, false}} {
		flat, err := reader.Flatten(source, tc.depth, tc.budget)
		if err != nil {
			t.Fatal(err)
		}
		props := schemaMap(flat["properties"])
		for name, node := range map[string]map[string]any{"money": schemaMap(props["money"]), "lines": schemaMap(schemaMap(props["lines"])["items"])} {
			if got := node["properties"] != nil; got != tc.inline {
				t.Errorf("depth=%d budget=%d %s inline=%v", tc.depth, tc.budget, name, got)
			}
			if !tc.inline && !strings.Contains(node["description"].(string), "flint_describe path="+name) {
				t.Errorf("missing %s path hint: %#v", name, node)
			}
		}
		if schemaMap(schemaMap(props["tags"])["items"])["type"] != "string" {
			t.Fatal("scalar array items lost")
		}
		if flat["properties"] == nil {
			t.Fatal("root was truncated by inline budget")
		}
	}
}

func TestFlattenUnionDiscriminator(t *testing.T) {
	source := map[string]any{"oneOf": []any{
		map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"type": "string", "const": "full"}}, "required": []any{"mode"}},
		map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"type": "string", "const": "amount"}, "amount": map[string]any{"type": "integer"}}, "required": []any{"mode", "amount"}},
	}}
	flat, err := New(source).Flatten(source, 2, 600)
	if err != nil {
		t.Fatal(err)
	}
	props := schemaMap(flat["properties"])
	mode := schemaMap(props["mode"])
	if !reflect.DeepEqual(stringsOf(mode["enum"]), []string{"amount", "full"}) {
		t.Fatalf("discriminator enum: %#v", mode)
	}
	if mode["const"] != nil || !strings.Contains(mode["description"].(string), "amount needs amount") {
		t.Fatalf("discriminator summary: %#v", mode)
	}
	if !reflect.DeepEqual(stringsOf(flat["required"]), []string{"mode"}) {
		t.Fatalf("required must be intersection: %#v", flat["required"])
	}
	flattenAssertNoKeywords(t, flat)
}

func TestFlattenEnumAndDescriptionLimits(t *testing.T) {
	values := make([]any, 41)
	for i := range values {
		values[i] = i
	}
	source := map[string]any{"type": "object", "properties": map[string]any{
		"large":   map[string]any{"type": "integer", "enum": values, "description": strings.Repeat("Long ", 80)},
		"small":   map[string]any{"type": "integer", "enum": values[:40]},
		"unicode": map[string]any{"type": "string", "description": strings.Repeat("界", 180)},
	}}
	flat, err := New(source).Flatten(source, 2, 600)
	if err != nil {
		t.Fatal(err)
	}
	props := schemaMap(flat["properties"])
	large := schemaMap(props["large"])
	if large["enum"] != nil || !strings.Contains(large["description"].(string), "41 allowed values") {
		t.Fatalf("large enum: %#v", large)
	}
	if len(schemaMap(props["small"])["enum"].([]any)) != 40 {
		t.Fatal("40-value enum omitted")
	}
	for _, name := range []string{"large", "unicode"} {
		description := schemaMap(props[name])["description"].(string)
		if utf8.RuneCountInString(description) > 160 || !utf8.ValidString(description) {
			t.Fatalf("description: %q", description)
		}
	}
}

func TestFlattenReferencesRecursionAndNoMutation(t *testing.T) {
	source := map[string]any{"type": "object", "properties": map[string]any{"node": map[string]any{"$ref": "#/$defs/node"}}, "$defs": map[string]any{
		"node": map[string]any{"type": []any{"object", "null"}, "properties": map[string]any{"label": map[string]any{"type": "string"}, "next": map[string]any{"$ref": "#/$defs/node"}}},
	}}
	before, _ := json.Marshal(source)
	flat, err := New(source).Flatten(source, 3, 600)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(source)
	if string(before) != string(after) {
		t.Fatal("Flatten mutated its source")
	}
	flattenAssertNoKeywords(t, flat)
	node := schemaMap(schemaMap(flat["properties"])["node"])
	if !reflect.DeepEqual(stringsOf(node["type"]), []string{"object", "null"}) {
		t.Fatalf("nullable object: %#v", node)
	}
}

func TestFlattenRecursiveArray(t *testing.T) {
	source := map[string]any{"type": "object", "properties": map[string]any{"nested": map[string]any{"$ref": "#/$defs/array"}}, "$defs": map[string]any{
		"array": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/array"}},
	}}
	flat, err := New(source).Flatten(source, 2, 600)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(flat)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 1000 {
		t.Fatalf("recursive array did not stop: %s", encoded)
	}
	flattenAssertNoKeywords(t, flat)
}

func TestFlattenMissingReference(t *testing.T) {
	source := map[string]any{"type": "object", "properties": map[string]any{"missing": map[string]any{"$ref": "#/$defs/missing"}}}
	if _, err := New(source).Flatten(source, 2, 600); err == nil {
		t.Fatal("missing reference was silently dropped")
	}
}

func flattenAssertNoKeywords(t *testing.T, value any) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			switch key {
			case "$ref", "$defs", "oneOf", "anyOf", "allOf", "not", "if", "discriminator", "pattern", "example", "examples", "readOnly", "writeOnly", "deprecated", "xml", "externalDocs":
				t.Errorf("forbidden keyword %s", key)
			}
			if strings.HasPrefix(key, "x-") {
				t.Errorf("extension %s retained", key)
			}
			flattenAssertNoKeywords(t, child)
		}
	case []any:
		for _, child := range value {
			flattenAssertNoKeywords(t, child)
		}
	}
}
