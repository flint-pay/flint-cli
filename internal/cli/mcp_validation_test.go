package cli

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

func mcpValidationDetails(t *testing.T, e *CLIError) []map[string]any {
	t.Helper()
	if e == nil || e.Code != "INVALID_REQUEST" || e.Type != "validation_error" || e.ExitCode != ExitUsage {
		t.Fatalf("invalid validation error: %#v", e)
	}
	details, ok := e.Details.([]map[string]any)
	if !ok || len(details) == 0 || e.Param != details[0]["param"] {
		t.Fatalf("invalid details: %#v", e)
	}
	for _, detail := range details {
		if len(detail) != 3 || detail["param"] == "" || detail["param"] == nil || strings.HasPrefix(fmt.Sprint(detail["param"]), "$") || strings.Contains(fmt.Sprint(detail["message"]), "urn:") || strings.Contains(fmt.Sprint(detail["message"]), "https://") {
			t.Fatalf("unexpected validation detail: %#v", detail)
		}
	}
	return details
}

func mcpValidationCodeParams(details []map[string]any) []string {
	out := []string{}
	for _, detail := range details {
		out = append(out, fmt.Sprint(detail["code"], ":", detail["param"]))
	}
	return out
}

func TestMCPInputValidationNestedCollections(t *testing.T) {
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"line_items": map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name"}, "properties": map[string]any{
			"name": map[string]any{"type": "string"}, "quantity": map[string]any{"type": "integer", "minimum": 1},
		}}},
		"metadata": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
	}}
	input := map[string]any{
		"line_items": []any{map[string]any{"name": false, "quantity": 0, "naem": "x"}, map[string]any{"quantity": "one"}},
		"metadata":   map[string]any{"a.b": false, "0": false},
	}
	details := mcpValidationDetails(t, mcpInputError(mcpSchemaProblems(schema, input, mcpInputBodyLocation, "")))
	want := []string{"INVALID_VALUE:line_items[0].name", "UNKNOWN_FIELD:line_items[0].naem", "INVALID_VALUE:line_items[0].quantity", "REQUIRED:line_items[1].name", "INVALID_VALUE:line_items[1].quantity", "INVALID_VALUE:metadata.0", `INVALID_VALUE:metadata["a.b"]`}
	// Segment ordering is lexical for object keys, including numeric map keys.
	want[0], want[1] = want[1], want[0]
	if got := mcpValidationCodeParams(details); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v; details=%#v", got, want, details)
	}
	if !strings.Contains(fmt.Sprint(details[0]["message"]), "Did you mean name?") {
		t.Fatalf("unknown field has no suggestion: %#v", details[0])
	}
}

func mcpValidationRefundSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"refund_type"}, "properties": map[string]any{
		"refund_type":  map[string]any{"type": "string", "enum": []string{"full", "amount", "targets"}},
		"amount_money": map[string]any{"type": "object", "required": []string{"amount"}, "properties": map[string]any{"amount": map[string]any{"type": "integer", "minimum": 1}}},
		"targets":      map[string]any{"type": "array"},
	}, "oneOf": []any{
		map[string]any{"properties": map[string]any{"refund_type": map[string]any{"const": "full"}}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"amount_money"}}, map[string]any{"required": []string{"targets"}}}}},
		map[string]any{"properties": map[string]any{"refund_type": map[string]any{"const": "amount"}}, "required": []string{"amount_money"}, "not": map[string]any{"required": []string{"targets"}}},
		map[string]any{"properties": map[string]any{"refund_type": map[string]any{"const": "targets"}}, "required": []string{"targets"}, "not": map[string]any{"required": []string{"amount_money"}}},
	}}
}

func TestMCPInputValidationSelectedVariant(t *testing.T) {
	schema := mcpValidationRefundSchema()
	input := map[string]any{"refund_type": "amount", "targets": []any{}}
	details := mcpValidationDetails(t, mcpInputError(mcpSchemaProblems(schema, input, mcpInputBodyLocation, "")))
	want := []string{"REQUIRED:amount_money", "NOT_ALLOWED:targets"}
	if got := mcpValidationCodeParams(details); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v; details=%#v", got, want, details)
	}
	if got := details[0]["message"]; got != "amount_money is required when refund_type is amount." {
		t.Fatalf("conditional requirement: %v", got)
	}
	for _, input := range []map[string]any{{}, {"refund_type": "bogus"}, {"refund_type": false}} {
		details := mcpValidationDetails(t, mcpInputError(mcpSchemaProblems(schema, input, mcpInputBodyLocation, "")))
		if len(details) != 1 || details[0]["param"] != "refund_type" || !strings.Contains(fmt.Sprint(details[0]["message"]), "amount, full, targets") {
			t.Fatalf("discriminator must produce one useful detail: %#v", details)
		}
	}
}

func TestMCPInputValidationReferencedVariantsAndClosedBranches(t *testing.T) {
	schema := map[string]any{"$defs": map[string]any{
		"amount": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"kind": map[string]any{"const": "amount"}, "amount": map[string]any{"type": "integer"}}, "required": []string{"kind", "amount"}},
		"full":   map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"kind": map[string]any{"const": "full"}, "reason": map[string]any{"type": "string"}}, "required": []string{"kind"}},
	}, "type": "object", "properties": map[string]any{"request": map[string]any{"oneOf": []any{map[string]any{"$ref": "#/$defs/amount"}, map[string]any{"$ref": "#/$defs/full"}}}}}
	input := map[string]any{"request": map[string]any{"kind": "amount", "reason": "x", "amout": 2}}
	details := mcpValidationDetails(t, mcpInputError(mcpSchemaProblems(schema, input, mcpInputBodyLocation, "")))
	want := []string{"REQUIRED:request.amount", "UNKNOWN_FIELD:request.amout", "NOT_ALLOWED:request.reason"}
	if got := mcpValidationCodeParams(details); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMCPInputValidationClosestStructuralUnion(t *testing.T) {
	schema := map[string]any{"anyOf": []any{
		map[string]any{"required": []string{"a", "b", "c"}},
		map[string]any{"required": []string{"d"}},
	}}
	details := mcpValidationDetails(t, mcpInputError(mcpSchemaProblems(schema, map[string]any{}, mcpInputBodyLocation, "")))
	if got := mcpValidationCodeParams(details); !reflect.DeepEqual(got, []string{"REQUIRED:d"}) {
		t.Fatalf("closest branch: %v", got)
	}
}

func TestMCPInputValidationQueryNamesAndControlIndexes(t *testing.T) {
	array := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	problems := mcpSchemaProblems(array, []any{false, false}, mcpInputQueryLocation, "status")
	problems = append(problems, mcpSchemaProblems(map[string]any{"type": "integer"}, false, mcpInputQueryLocation, "page_size")...)
	details := mcpValidationDetails(t, mcpInputError(problems))
	if got := mcpValidationCodeParams(details); !reflect.DeepEqual(got, []string{"INVALID_VALUE:page_size", "INVALID_VALUE:status"}) {
		t.Fatalf("query names must sort by param without array indexes: %v", got)
	}
	cmd, _ := NewRegistry().ByName("customers.get")
	problems = mcpControlProblems(cmd, opindex.Entry{ID: "customers.get", Kind: "read", Get: true}, map[string]any{"select": []any{"customer_id", " ", 5}, "wait_for": []any{"", false}}, mcpPolicy{})
	details = mcpValidationDetails(t, mcpInputError(problems))
	want := []string{"INVALID_VALUE:select[1]", "INVALID_VALUE:select[2]", "INVALID_VALUE:wait_for[0]", "INVALID_VALUE:wait_for[1]"}
	if got := mcpValidationCodeParams(details); !reflect.DeepEqual(got, want) {
		t.Fatalf("control item failures: got %v want %v", got, want)
	}
}

func TestMCPInputValidationAggregatesBodyAndControls(t *testing.T) {
	cmd, _ := NewRegistry().ByName("customers.create")
	entry := opindex.Entry{ID: "customers.create", Kind: "write", Idempotency: "required"}
	_, e := mcpValidateInputs(cmd, entry, map[string]any{"email": false, "customer": "x"}, map[string]any{"select": []any{"customer_id", false, "definitely_missing", "also_missing"}, "dry_run": "yes"}, mcpPolicy{})
	details := mcpValidationDetails(t, e)
	want := []string{"UNKNOWN_FIELD:customer", "INVALID_VALUE:email", "INVALID_VALUE:dry_run", "REQUIRED:idempotency_key", "INVALID_VALUE:select[1]", "INVALID_VALUE:select[2]", "INVALID_VALUE:select[3]"}
	if got := mcpValidationCodeParams(details); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMCPInputValidationRealRefundVariants(t *testing.T) {
	cmd, _ := NewRegistry().ByName("refunds.create")
	entry := opindex.Entry{ID: "refunds.create", Kind: "write", Idempotency: "required"}
	_, e := mcpValidateInputs(cmd, entry, map[string]any{"refund_type": "amount", "order_id": "ord_fixture"}, nil, mcpPolicy{})
	details := mcpValidationDetails(t, e)
	if got := mcpValidationCodeParams(details); !reflect.DeepEqual(got, []string{"REQUIRED:amount_money", "REQUIRED:idempotency_key"}) || details[0]["message"] != "amount_money is required when refund_type is amount." {
		t.Fatalf("real conditional requirement: %#v", details)
	}
	_, e = mcpValidateInputs(cmd, entry, map[string]any{"refund_type": "amount", "amount_money": map[string]any{"amount": 10, "currency": "USD"}}, map[string]any{"idempotency_key": "fixture"}, mcpPolicy{})
	details = mcpValidationDetails(t, e)
	if got := mcpValidationCodeParams(details); !reflect.DeepEqual(got, []string{"REQUIRED:order_id"}) {
		t.Fatalf("independent anyOf must choose the closest branch: %#v", details)
	}
	_, e = mcpValidateInputs(cmd, entry, map[string]any{"refund_type": "unknown", "order_id": "ord_fixture"}, map[string]any{"idempotency_key": "fixture"}, mcpPolicy{})
	details = mcpValidationDetails(t, e)
	if len(details) != 1 || details[0]["param"] != "refund_type" || !strings.Contains(fmt.Sprint(details[0]["message"]), "amount, full, targets") {
		t.Fatalf("real unknown discriminator: %#v", details)
	}
}

func TestMCPInputValidationLocationOrderAndQueryNormalization(t *testing.T) {
	registry := NewRegistry()
	cmd, _ := registry.ByName("customers.get")
	_, e := mcpValidateInputs(cmd, opindex.Entry{ID: "customers.get", Kind: "read", Get: true}, map[string]any{"page_size": 0, "extra": true}, map[string]any{"all": true}, mcpPolicy{})
	details := mcpValidationDetails(t, e)
	if details[0]["param"] != "customer_id" || details[len(details)-1]["param"] != "all" || details[len(details)-1]["code"] != "NOT_ALLOWED" {
		t.Fatalf("location order: %#v", details)
	}
	cmd, _ = registry.ByName("orders.list")
	// Inspect the public contract rather than relying on one filter's enum.
	operation, _ := openAPIOperationByID(cmd.OperationID)
	for _, parameter := range operation.Parameters {
		name, _ := parameter["name"].(string)
		schema, _ := parameter["schema"].(map[string]any)
		if parameter["in"] == "query" && schema["type"] == "array" {
			items, _ := schema["items"].(map[string]any)
			values := mcpSchemaStrings(items["enum"])
			if len(values) == 0 {
				continue
			}
			value := values[0]
			args, e := mcpValidateInputs(cmd, opindex.Entry{ID: "orders.list", Kind: "read", Paginated: true}, map[string]any{name: value, "created_after": "-7d"}, nil, mcpPolicy{})
			if e != nil || !reflect.DeepEqual(args[name], []any{value}) {
				t.Fatalf("query shorthand %s: args=%#v error=%#v", name, args, e)
			}
			return
		}
	}
	t.Fatal("fixture has no array query parameter")
}

func TestMCPInputValidationLocalBoundsAndTypes(t *testing.T) {
	for _, fixture := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{"trigger", map[string]any{"max_seconds": 61, "unknown": true}, []string{"INVALID_VALUE:max_seconds", "UNKNOWN_FIELD:unknown"}},
		{"listen", map[string]any{"max_seconds": "60", "max_events": 101}, []string{"INVALID_VALUE:max_events", "INVALID_VALUE:max_seconds"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cmd, _ := NewRegistry().ByName(fixture.name)
			_, e := mcpValidateInputs(cmd, opindex.Entry{ID: fixture.name, Kind: "read"}, fixture.args, nil, mcpPolicy{})
			if got := mcpValidationCodeParams(mcpValidationDetails(t, e)); !reflect.DeepEqual(got, fixture.want) {
				t.Fatalf("got %v, want %v", got, fixture.want)
			}
		})
	}
}

func TestMCPInputValidationFormats(t *testing.T) {
	cmd, _ := NewRegistry().ByName("inventory-adjustments.create")
	input := minimalRequestBody(t, cmd)
	input["occurred_at"] = "yesterday"
	if _, e := validateRequestBody(cmd, cmd.CanonicalName, input); e != nil {
		t.Fatalf("MCP format assertion changed terminal compatibility: %v", e)
	}
	_, e := mcpValidateInputs(cmd, opindex.Entry{ID: cmd.CanonicalName, Kind: "write"}, input, nil, mcpPolicy{})
	details := mcpValidationDetails(t, e)
	found := false
	for _, detail := range details {
		if detail["param"] == "occurred_at" && detail["code"] == "INVALID_VALUE" && detail["message"] == "occurred_at must be a valid date-time." {
			found = true
		}
	}
	if !found {
		t.Fatalf("body date-time format was not asserted: %#v", details)
	}
	if problems := mcpSchemaProblems(map[string]any{"type": "string", "format": "flint-resource-id"}, "fixture", mcpInputBodyLocation, ""); len(problems) > 0 {
		t.Fatalf("unknown format should remain an annotation: %#v", problems)
	}
}

func TestMCPInputValidationNumericOrderingAndCap(t *testing.T) {
	input := []any{}
	for i := 0; i < 55; i++ {
		input = append(input, false)
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"items": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}
	details := mcpValidationDetails(t, mcpInputError(mcpSchemaProblems(schema, map[string]any{"items": input}, mcpInputBodyLocation, "")))
	if len(details) != 50 {
		t.Fatalf("detail count=%d", len(details))
	}
	for i, detail := range details {
		if detail["param"] != fmt.Sprintf("items[%d]", i) {
			t.Fatalf("numeric ordering: detail %d=%#v", i, detail)
		}
	}
}

func TestMCPInputValidationLegacyWarningPathCollision(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}}}}}
	input := map[string]any{"a.b": "unknown", "a": map[string]any{"b": "known"}}
	if got := mcpLegacyWarningTokens(input, "$.a.b", schema); !reflect.DeepEqual(got, [][]string{{"a.b"}}) {
		t.Fatalf("ambiguous legacy path must not reject the declared nested field: %#v", got)
	}
}
