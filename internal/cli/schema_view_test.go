package cli

import (
	"encoding/json"
	"github.com/flint-pay/flint-cli/internal/schemaview"
	"slices"
	"testing"
)

func discoveryField(t *testing.T, v schemaview.View, name string) map[string]any {
	t.Helper()
	for _, f := range v.Fields {
		if f["name"] == name {
			return f
		}
	}
	t.Fatalf("missing %s in %+v", name, v)
	return nil
}
func TestSchemaViewRealVariants(t *testing.T) {
	registry := NewRegistry()
	for _, tc := range []struct{ name, discriminator, variant, required, absent string }{
		{"orders.pay", "action", "setup", "setup_payment_source", "order_payment_attempt_id"},
		{"inventory-adjustments.create", "reason", "correction", "note", ""},
		{"checkout-sessions.create", "type", "order", "order_id", "quick_pay_item"},
		{"products.create", "variant_mode", "simple", "default_variant", "options"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, ok := registry.ByName(tc.name)
			if !ok {
				t.Fatal(tc.name)
			}
			reader, schema, e := commandDiscoverySchema(cmd, true)
			if e != nil {
				t.Fatal(e)
			}
			view, err := reader.Describe(schema, schemaview.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if view.Variants == nil || view.Variants.Discriminator != tc.discriminator || !slices.Contains(view.Variants.Requires[tc.variant], tc.required) {
				t.Fatalf("variants: %+v", view)
			}
			selected, err := reader.Describe(schema, schemaview.Options{Variant: tc.variant})
			if err != nil {
				t.Fatal(err)
			}
			if discoveryField(t, selected, tc.required)["required"] != true {
				t.Fatal(selected)
			}
			for _, field := range selected.Fields {
				if tc.absent != "" && field["name"] == tc.absent {
					t.Fatalf("forbidden field %s in selected variant", tc.absent)
				}
			}
			if tc.name == "orders.pay" {
				if discoveryField(t, selected, "order_id")["required"] != true {
					t.Fatal("path argument lost")
				}
				t.Logf("pay args=%d", len(view.Fields))
			}
			if _, err := reader.Flatten(schema, 2, 600); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestSchemaViewTimeFormats(t *testing.T) {
	registry := NewRegistry()
	cmd, _ := registry.ByName("orders.list")
	reader, schema, e := commandDiscoverySchema(cmd, true)
	if e != nil {
		t.Fatal(e)
	}
	view, err := reader.Describe(schema, schemaview.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if discoveryField(t, view, "created_after")["format"] != nil {
		t.Fatal("relative query advertised date-time")
	}
	cmd, _ = registry.ByName("inventory-adjustments.create")
	reader, schema, e = commandDiscoverySchema(cmd, true)
	if e != nil {
		t.Fatal(e)
	}
	view, err = reader.Describe(schema, schemaview.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if discoveryField(t, view, "occurred_at")["format"] != "date-time" {
		t.Fatal("body timestamp format lost")
	}
}
func TestSchemaViewDescribeBudgets(t *testing.T) {
	registry := NewRegistry()
	operations := 0
	for _, cmd := range registry.Commands {
		if cmd.CanonicalName != cmd.Name || cmd.Local {
			continue
		}
		operations++
		for _, input := range []bool{true, false} {
			reader, schema, e := commandDiscoverySchema(cmd, input)
			if e != nil {
				t.Fatalf("%s: %v", cmd.Name, e)
			}
			view, err := reader.Describe(schema, schemaview.Options{})
			if err != nil {
				t.Fatalf("%s input=%v: %v", cmd.Name, input, err)
			}
			encoded, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			budget := 15000
			if input {
				budget = 7000
			}
			if len(encoded) > budget {
				t.Errorf("%s input=%v: %d exceeds %d", cmd.Name, input, len(encoded), budget)
			}
			if input {
				flat, err := reader.Flatten(schema, 2, 600)
				if err != nil {
					t.Fatalf("%s: %v", cmd.Name, err)
				}
				if flat["type"] != "object" || flat["additionalProperties"] != false {
					t.Fatalf("root %s", cmd.Name)
				}
			}
		}
	}
	if operations < 600 {
		t.Fatalf("only %d commands checked", operations)
	}
}
