package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMCPNestedSelectionPreservesAbsenceAndNull(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, operation := range []string{"orders.get", "orders.list"} {
		t.Run(operation, func(t *testing.T) {
			cmd := mustMCPTool(t, app, operation)
			for _, tc := range []struct {
				name     string
				resource map[string]any
				want     map[string]any
				warning  string
			}{
				{"unexpanded", map[string]any{"customer_id": "cus_fixture"}, map[string]any{"customer_id": "cus_fixture"}, `arguments.expand: ["customer"]`},
				{"expanded", map[string]any{"customer_id": "cus_fixture", "customer": map[string]any{"name": "Frank"}}, map[string]any{"customer_id": "cus_fixture", "customer": map[string]any{"name": "Frank"}}, ""},
				{"explicit null", map[string]any{"customer_id": "cus_fixture", "customer": map[string]any{"name": nil}}, map[string]any{"customer_id": "cus_fixture", "customer": map[string]any{"name": nil}}, ""},
				{"null parent", map[string]any{"customer_id": "cus_fixture", "customer": nil}, map[string]any{"customer_id": "cus_fixture"}, "select path customer.name is not in this result"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var data any = tc.resource
					var want any = tc.want
					if operation == "orders.list" {
						data = []any{tc.resource}
						want = []any{tc.want}
					}
					input := map[string]any{"data": data, "request_id": "req_selected", "next_page_token": "next"}
					got, warning, e := mcpProjectResult(context.Background(), cmd, input, Options{Select: []string{"customer_id", "customer.name"}})
					if e != nil || !reflect.DeepEqual(resourceRoot(got), want) || !strings.Contains(warning, tc.warning) || tc.warning == "" && warning != "" {
						t.Fatalf("got=%v warning=%s e=%v", got, warning, e)
					}
					if lookupPathValue(got, "request_id") != "req_selected" || lookupPathValue(got, "next_page_token") != "next" {
						t.Fatal("lost envelope")
					}
					if !reflect.DeepEqual(resourceRoot(input), data) {
						t.Fatal("modified input")
					}
				})
			}
		})
	}
}

func TestMCPArraySelection(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, op := range []string{"orders.get", "orders.list", "orders.create"} {
		t.Run(op, func(t *testing.T) {
			cmd := mustMCPTool(t, app, op)
			for _, fields := range [][]string{{"line_items.name", "line_items.quantity"}, {"line_items[].name", "line_items[].quantity"}, {"line_items.*.name", "line_items.*.quantity"}} {
				if e := mcpValidateSelect(cmd, map[string]any{"select": fields}); e != nil {
					t.Fatalf("fields=%v e=%v", fields, e)
				}
				input := map[string]any{"data": map[string]any{"order_id": "ord_fixture", "status": "open", "line_items": []any{map[string]any{"name": "Vase", "quantity": 2, "secret": "excluded"}, map[string]any{"quantity": 1}, map[string]any{"name": nil, "quantity": 3}}}}
				if op == "orders.list" {
					input["data"] = []any{input["data"]}
				}
				got, warning, e := mcpProjectResult(context.Background(), cmd, input, Options{Select: fields})
				root := resourceRoot(got)
				if op == "orders.list" {
					root = root.([]any)[0]
				}
				want := []any{map[string]any{"name": "Vase", "quantity": json.Number("2")}, map[string]any{"quantity": json.Number("1")}, map[string]any{"name": nil, "quantity": json.Number("3")}}
				if e != nil || !reflect.DeepEqual(lookupPathValue(root, "line_items"), want) || !strings.Contains(warning, "is not in this result") {
					t.Fatalf("got=%v e=%v warning=%s", got, e, warning)
				}
				if op == "orders.create" && lookupPathValue(root, "order_id") != "ord_fixture" {
					t.Fatal("lost write identity")
				}
			}
			if e := mcpValidateSelect(cmd, map[string]any{"select": []string{"line_items.not_a_field"}}); e == nil {
				t.Fatal("accepted unknown array field")
			}
		})
	}
}

func TestMCPAbsentWebhookDataIsNotFabricated(t *testing.T) {
	app, _, _ := testApp(t, "")
	cmd := mustMCPTool(t, app, "webhook-events.list")
	input := map[string]any{"data": []any{map[string]any{"resource_id": "pi_fixture"}}}
	got, warning, e := mcpProjectResult(context.Background(), cmd, input, Options{Select: []string{"data.object.last_payment_error.code"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, exists := lookupPath(got, "data.0.data"); exists || !strings.Contains(warning, "not in this result") {
		t.Fatalf("got=%v warning=%s", got, warning)
	}
}
