package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRelease019ReturnChildCreateRoutes(t *testing.T) {
	for _, resource := range []string{"inspections", "receipts", "dispositions", "resolutions"} {
		t.Run(resource, func(t *testing.T) {
			app, out, _ := testApp(t, "http://127.0.0.1:1")
			name := "return-" + resource + ".create"
			cmd, ok := app.Registry.ByName(name)
			if !ok {
				t.Fatal("missing", name)
			}
			body := minimalRequestBody(t, cmd)
			delete(body, "return_id")
			raw, _ := json.Marshal(body)
			args := []string{"return-" + resource, "create", "--return-id", "ret_test", "--input", string(raw), "--idempotency-key", "return-child-test", "--dry-run=client"}
			exit, result := runJSON(t, app, out, args...)
			if exit != ExitOK || lookupPathValue(result, "data.path") != "/v1/return-"+resource || lookupPathValue(result, "data.body.return_id") != "ret_test" {
				t.Fatalf("exit=%d result=%v", exit, result)
			}
			out.Reset()
			exit, result = runJSON(t, app, out, "returns", resource, "create", "ret_test")
			if exit != ExitUsage || lookupPathValue(result, "error.code") != "UNKNOWN_COMMAND" {
				t.Fatalf("retired: exit=%d result=%v", exit, result)
			}
		})
	}
}

func TestRelease019CustomLineProductTypeFlags(t *testing.T) {
	const line = `{"type":"custom","name":"Consulting","unit_price_money":{"amount":1000,"currency":"USD"},"fulfillment":{"requirement":"none"}}`
	const fixed = `{"type":"custom_fixed","name":"Consulting","unit_price_money":{"amount":1000,"currency":"USD"}}`
	const variable = `{"type":"custom_variable","name":"Consulting","allow_unit_price_adjustment":true}`
	for _, tc := range []struct {
		args        []string
		input, path string
	}{
		{[]string{"orders", "create"}, `{"line_items":[` + line + `]}`, "line_items.0.product_type"},
		{[]string{"orders", "line-items", "create", "ord_test"}, `{"line_items":[` + line + `]}`, "line_items.0.product_type"},
		{[]string{"orders", "line-items", "update", "ord_test", "li_test"}, `{}`, "product_type"},
		{[]string{"invoices", "create"}, `{"type":"quick_pay","quick_pay":{"line_items":[` + line + `]}}`, "quick_pay.line_items.0.product_type"},
		{[]string{"payment-links", "create", "--name", "Service"}, `{"line_items":[` + fixed + `]}`, "line_items.0.product_type"},
		{[]string{"payment-links", "create", "--name", "Service"}, `{"line_items":[` + variable + `]}`, "line_items.0.product_type"},
		{[]string{"payment-links", "update", "plink_test", "--expected-version", "1"}, `{"line_items":[` + fixed + `]}`, "line_items.0.product_type"},
		{[]string{"payment-links", "update", "plink_test", "--expected-version", "1"}, `{"line_items":[` + variable + `]}`, "line_items.0.product_type"},
	} {
		t.Run(strings.Join(tc.args, " ")+tc.input, func(t *testing.T) {
			app, out, _ := testApp(t, "http://127.0.0.1:1")
			for _, value := range []string{"physical", "digital", "service", "fee", "invalid"} {
				args := append(append([]string{}, tc.args...), "--input", tc.input, "--product-type", value, "--dry-run=client", "--idempotency-key", "product-type-test")
				exit, result := runJSON(t, app, out, args...)
				if value == "invalid" {
					if exit == ExitOK {
						t.Fatal("accepted invalid product type")
					}
					continue
				}
				if exit != ExitOK || lookupPathValue(result, "data.body."+tc.path) != value {
					t.Fatalf("value=%s exit=%d result=%v", value, exit, result)
				}
				out.Reset()
			}
		})
	}
}

func TestRelease019RetiredOrderPaymentIntentCreate(t *testing.T) {
	app, out, _ := testApp(t, "http://127.0.0.1:1")
	exit, result := runJSON(t, app, out, "orders", "payment-intents", "create", "ord_test")
	if exit != ExitUsage || lookupPathValue(result, "error.code") != "UNKNOWN_COMMAND" {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
}
