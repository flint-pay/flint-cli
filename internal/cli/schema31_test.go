package cli

import (
	"encoding/json"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPaymentIntentMCPInputPreservesStandaloneConditions(t *testing.T) {
	registry := NewRegistry()
	standalone, _ := registry.ByName("payment-intents.create")
	for _, fixture := range []struct {
		fields map[string]any
		valid  bool
	}{
		{map[string]any{}, false},
		{map[string]any{"payment_options": []any{}}, false},
		{map[string]any{"payment_options": []any{"card"}}, true},
		{map[string]any{"payment_options": []any{"ach_debit"}}, false},
		{map[string]any{"payment_options": []any{"ach_debit"}, "transaction_purpose": "goods"}, true},
		{map[string]any{"payment_options": []any{"card"}, "transaction_purpose": "goods"}, false},
		{map[string]any{"payment_options": []any{"affirm"}}, false},
		{map[string]any{"payment_options": []any{"affirm"}, "payment_return_url": "https://example.com/return"}, true},
		{map[string]any{"payment_options": []any{"ach_debit", "affirm"}, "transaction_purpose": "services", "payment_return_url": "https://example.com/return"}, true},
	} {
		body := deepCopyMap(fixture.fields)
		body["amount_money"] = map[string]any{"amount": float64(1000), "currency": "USD"}
		body["idempotency_key"] = "saved-test-action-1"
		if err := validateMCPArguments(standalone, body); (err == nil) != fixture.valid {
			t.Errorf("standalone arguments %#v: %v (valid=%v)", body, err, fixture.valid)
		}
	}
	for _, body := range []map[string]any{
		{"amount": float64(1000), "currency": "USD", "payment_options": []any{"card"}},
		{"order": "ord_123"},
		{"amount_money": map[string]any{"amount": float64(1000), "currency": "USD"}, "payment_options": []any{"card"}, "order": "ord_123"},
	} {
		body["idempotency_key"] = "saved-test-action-1"
		if err := validateMCPArguments(standalone, body); err == nil {
			t.Errorf("accepted CLI convenience arguments: %#v", body)
		}
	}
	// The order-specific API accepts omitted or empty payment_options and does
	// not apply the standalone payment intent's ACH condition.
	orderLinked, _ := registry.ByName("orders.payment-intents.create")
	for _, body := range []map[string]any{
		{"order_id": "ord_123"},
		{"order_id": "ord_123", "payment_options": []any{}},
		{"order_id": "ord_123", "payment_options": []any{"ach_debit"}},
	} {
		body["idempotency_key"] = "saved-test-action-1"
		if err := validateMCPArguments(orderLinked, body); err != nil {
			t.Errorf("standalone conditions leaked into order arguments %#v: %v", body, err)
		}
	}
}

func TestSchemaExportsRelocateNativeNullReferences(t *testing.T) {
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.2" {
		t.Fatalf("version = %s", doc.OpenAPI)
	}
	command, _ := NewRegistry().ByName("refunds.get")
	schema, schemaErr := schemaForCommand(command, false)
	if schemaErr != nil {
		t.Fatal(schemaErr)
	}
	encoded, _ := json.Marshal(schema)
	if strings.Contains(string(encoded), "#/components/schemas/") {
		t.Fatal("unrelocated component reference")
	}
	if !strings.Contains(string(encoded), `"null"`) {
		t.Fatal("native null type missing")
	}
	for event := range doc.Webhooks {
		if _, exists := NewRegistry().ByName("receive_" + strings.ReplaceAll(event, ".", "_")); exists {
			t.Fatalf("webhook %s became callable", event)
		}
	}
}

func TestSchemaReferenceWalkPreservesLiteralPayloads(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{"examples": map[string]any{"$ref": "#/components/schemas/Money"}},
		"examples":   []any{map[string]any{"$ref": "#/components/schemas/Literal", "nullable": true}},
		"if":         map[string]any{"properties": map[string]any{"money": map[string]any{"$ref": "#/components/schemas/Money"}}},
	}
	refs := map[string]bool{}
	collectComponentRefs(schema, refs)
	if len(refs) != 1 || !refs["Money"] {
		t.Fatalf("references = %#v", refs)
	}
	rewriteSchemaRefs(schema)
	property := schema["properties"].(map[string]any)["examples"].(map[string]any)
	if property["$ref"] != "#/$defs/Money" {
		t.Fatalf("property reference = %#v", property)
	}
	example := schema["examples"].([]any)[0].(map[string]any)
	if example["$ref"] != "#/components/schemas/Literal" || example["nullable"] != true {
		t.Fatalf("literal example rewritten: %#v", example)
	}
}

func TestExportedNullEnumAndReferenceAcceptance(t *testing.T) {
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		component, field string
		valid, invalid   []any
	}{
		{"Refund", "reason", []any{nil, "duplicate"}, []any{"unknown", float64(0)}},
		{"PaymentReview", "refunded_money", []any{nil, map[string]any{"amount": float64(100), "currency": "USD"}}, []any{map[string]any{}, "100"}},
		{"UpdateProductRequest", "metadata", []any{nil, map[string]any{"color": "red", "old": nil}}, []any{map[string]any{"color": float64(2)}, []any{}}},
	} {
		property := doc.Components.Schemas[fixture.component]["properties"].(map[string]any)[fixture.field].(map[string]any)
		schema := deepCopyMap(property)
		defs := reachableSchemaDefinitions(schema, doc.Components.Schemas)
		rewriteSchemaRefs(schema)
		schema["$defs"] = defs
		schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("https://example.com/schema", schema); err != nil {
			t.Fatal(err)
		}
		compiled, err := compiler.Compile("https://example.com/schema")
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range fixture.valid {
			if err := compiled.Validate(value); err != nil {
				t.Errorf("%s.%s rejected %#v: %v", fixture.component, fixture.field, value, err)
			}
		}
		for _, value := range fixture.invalid {
			if err := compiled.Validate(value); err == nil {
				t.Errorf("%s.%s accepted %#v", fixture.component, fixture.field, value)
			}
		}
	}
}

func TestWebhookCatalogContainsItsReferencedComponents(t *testing.T) {
	value, err := eventCatalog()
	if err != nil {
		t.Fatal(err)
	}
	catalog := value.(map[string]any)
	components := catalog["components"].(map[string]any)["schemas"].(map[string]any)
	if len(components) == 0 {
		t.Fatal("webhook components missing")
	}
	for name, component := range components {
		refs := map[string]bool{}
		collectComponentRefs(component, refs)
		for ref := range refs {
			if components[ref] == nil {
				t.Errorf("%s references missing %s", name, ref)
			}
		}
	}
	if _, exists := catalog["paths"]; exists {
		t.Fatal("outbound event catalog includes callable paths")
	}
}
