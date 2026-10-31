package schemaview

import "testing"

func TestLazyReferencesPreserveVariants(t *testing.T) {
	calls := map[string]int{}
	reader := NewLazy(map[string]any{"$defs": map[string]any{"Local": map[string]any{"type": "string"}}}, func(ref string) (map[string]any, bool) {
		calls[ref]++
		if ref != "#/components/schemas/Choice" {
			return nil, false
		}
		return map[string]any{"discriminator": map[string]any{"propertyName": "kind"}, "oneOf": []any{
			map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"const": "a"}, "a": map[string]any{"type": "string"}}, "required": []any{"kind", "a"}},
			map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"const": "b"}, "b": map[string]any{"type": "integer"}}, "required": []any{"kind", "b"}},
		}}, true
	})
	if len(calls) != 0 {
		t.Fatal("resolver eagerly called")
	}
	if _, err := reader.Resolve(map[string]any{"$ref": "#/$defs/Local"}, ""); err != nil || len(calls) != 0 {
		t.Fatal("local definition did not stay local", err)
	}
	all, err := reader.Resolve(map[string]any{"$ref": "#/components/schemas/Choice"}, "")
	if err != nil || all.Variants == nil || all.Variants.Discriminator != "kind" {
		t.Fatal("lazy variants missing", err)
	}
	selected, err := reader.Resolve(map[string]any{"$ref": "#/components/schemas/Choice"}, "b")
	if err != nil {
		t.Fatal(err)
	}
	props := selected.Schema["properties"].(map[string]any)
	if props["b"] == nil || props["a"] != nil {
		t.Fatal("selected branch differs", props)
	}
	if _, err := reader.Resolve(map[string]any{"$ref": "#/components/schemas/Missing"}, ""); err == nil {
		t.Fatal("missing reference accepted")
	}
}
