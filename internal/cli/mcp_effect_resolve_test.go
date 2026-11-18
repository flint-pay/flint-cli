package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const effectOrder = `{"customer_id":"cus_effect","pricing_amounts":{"total_money":{"amount":6300,"currency":"USD"}},"settlement_amounts":{"net_collected_money":{"amount":5800,"currency":"USD"},"outstanding_money":{"amount":1300,"currency":"USD"}},"charges":[{"order_charge_id":"och_shipping","total_money":{"amount":900,"currency":"USD"},"refunded_money":{"amount":0,"currency":"USD"}}],"line_items":[{"order_line_item_id":"li_effect","total_money":{"amount":5400,"currency":"USD"},"refunded_money":{"amount":500,"currency":"USD"}}]}`

func TestMCPImplicitEffectsDryRunAndApproval(t *testing.T) {
	for _, tc := range []struct {
		name, operation string
		args            map[string]any
		want            []string
		resolution      string
	}{
		{"full refund", "refunds.create", map[string]any{"order_id": "ord_effect", "refund_type": "full"}, []string{"up to 58.00 USD", "customer cus_effect"}, "upper_bound"},
		{"shipping refund", "refunds.create", map[string]any{"order_id": "ord_effect", "refund_type": "targets", "charges": []any{map[string]any{"order_charge_id": "och_shipping"}}}, []string{"9.00 USD", "customer cus_effect"}, "snapshot"},
		{"partial line refund", "refunds.create", map[string]any{"order_id": "ord_effect", "refund_type": "targets", "line_items": []any{map[string]any{"order_line_item_id": "li_effect", "quantity": 1}}}, []string{"up to 49.00 USD", "customer cus_effect"}, "upper_bound"},
		{"payment refund", "refunds.create", map[string]any{"payment_intent_id": "pi_effect", "refund_type": "full"}, []string{"up to 53.00 USD", "customer cus_effect"}, "upper_bound"},
		{"credit note refund", "refunds.create", map[string]any{"credit_note_id": "cn_00000000000000000000000000", "refund_type": "credit_note"}, []string{"up to 20.00 USD", "customer cus_effect"}, "upper_bound"},
		{"order invoice", "invoices.create", map[string]any{"type": "order", "order_id": "ord_effect", "issue": map[string]any{"delivery_mode": "email"}}, []string{"63.00 USD", "customer cus_effect", "recipient buyer@example.com"}, "snapshot"},
		{"custom invoice", "invoices.create", map[string]any{"type": "quick_pay", "quick_pay": map[string]any{"customer_id": "cus_effect", "line_items": []any{map[string]any{"type": "custom", "name": "Vase", "quantity": 40, "unit_price_money": map[string]any{"amount": 3000, "currency": "USD"}, "fulfillment": map[string]any{"requirement": "none"}}}}, "issue": map[string]any{"delivery_mode": "email"}}, []string{"subtotal 1200.00 USD", "Final invoice amount resolved by Flint", "customer cus_effect", "recipient buyer@example.com"}, "subtotal"},
		{"catalog invoice unresolved", "invoices.create", map[string]any{"type": "quick_pay", "quick_pay": map[string]any{"customer_id": "cus_effect", "line_items": []any{map[string]any{"type": "variant", "variant_id": "var_effect", "quantity": 40}}}, "recipient_email": "recipient@example.com", "issue": map[string]any{"delivery_mode": "email"}}, []string{"amount resolved by Flint: unavailable before execution", "catalog prices", "customer cus_effect", "recipient recipient@example.com"}, "unresolved"},
		{"issue invoice", "invoices.issue", map[string]any{"invoice_id": "inv_effect", "delivery_mode": "email"}, []string{"63.00 USD", "customer cus_effect", "recipient invoice@example.com"}, "snapshot"},
		{"collect invoice", "invoices.collect", map[string]any{"invoice_id": "inv_effect"}, []string{"13.00 USD", "customer cus_effect"}, "snapshot"},
		{"order pay", "orders.pay", map[string]any{"order_id": "ord_effect", "action": "pay"}, []string{"13.00 USD", "customer cus_effect"}, "snapshot"},
		{"cancel payout", "payouts.cancel", map[string]any{"payout_id": "po_effect"}, []string{"43.00 USD", "pdest_effect", "merchant payout; no customer"}, "snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, environment := range []string{"sandbox", "live"} {
				for _, dry := range []bool{true, false} {
					if environment == "sandbox" && !dry {
						continue
					}
					t.Run(fmt.Sprint(environment, " dry=", dry), func(t *testing.T) {
						writes := 0
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == "/v1/auth-context" {
								fmt.Fprint(w, authContextJSON(environment))
								return
							}
							if r.Method != "GET" {
								writes++
								fmt.Fprint(w, `{"data":{"status":"pending"}}`)
								return
							}
							switch r.URL.Path {
							case "/v1/orders/ord_effect":
								fmt.Fprint(w, `{"data":`+effectOrder+`}`)
							case "/v1/payment-intents/pi_effect":
								fmt.Fprint(w, `{"data":{"order_id":"ord_effect","customer_id":"cus_effect","captured_money":{"amount":6300,"currency":"USD"},"refunded_money":{"amount":1000,"currency":"USD"}}}`)
							case "/v1/credit-notes/cn_00000000000000000000000000":
								fmt.Fprint(w, `{"data":{"invoice_id":"inv_effect","available_money":{"amount":2000,"currency":"USD"}}}`)
							case "/v1/invoices/inv_effect":
								fmt.Fprint(w, `{"data":{"order_id":"ord_effect","recipient_email":"invoice@example.com","pricing_amounts":{"total_money":{"amount":6300,"currency":"USD"}},"settlement_amounts":{"currently_due_money":{"amount":1300,"currency":"USD"}}}}`)
							case "/v1/customers/cus_effect":
								fmt.Fprint(w, `{"data":{"email":"buyer@example.com"}}`)
							case "/v1/payouts/po_effect":
								fmt.Fprint(w, `{"data":{"amount_money":{"amount":4300,"currency":"USD"},"payout_destination_id":"pdest_effect"}}`)
							default:
								t.Errorf("unexpected lookup %s", r.URL.Path)
								http.NotFound(w, r)
							}
						}))
						defer server.Close()
						app, _, _ := testApp(t, server.URL)
						app.mcpAuth = &mcpAuthState{}
						cmd := mustMCPTool(t, app, tc.operation)
						args := map[string]any{}
						for k, v := range tc.args {
							args[k] = v
						}
						args["_flint"] = map[string]any{"dry_run": dry, "confirm": true, "idempotency_key": "effect-fixture"}
						if environment == "live" {
							args["_flint"].(map[string]any)["live"] = true
						}
						before, _ := json.Marshal(tc.args)
						prompt := ""
						var session *mcpSession
						session = newMCPSession(func(message map[string]any) error {
							if message["method"] == "elicitation/create" {
								if writes != 0 {
									t.Error("write before approval")
								}
								prompt = fmt.Sprint(lookupPathValue(message, "params.message"))
								session.deliver(message["id"], map[string]any{"result": map[string]any{"action": "decline"}})
							}
							return nil
						})
						session.configure(map[string]any{"elicitation": map[string]any{}}, latestMCPProtocolVersion)
						ctx := context.WithValue(context.Background(), mcpSessionKey{}, session)
						result := app.runMCPTool(ctx, cmd, args, defaultOptions(), mcpPolicy{allowLive: true}, false)
						if writes != 0 {
							t.Fatalf("preview dispatched %d writes", writes)
						}
						summary := prompt
						if dry {
							if result["isError"] == true {
								t.Fatal(mcpResultText(result))
							}
							value := mcpResultJSON(t, result)
							summary = fmt.Sprint(lookupPathValue(value, "data.effect.summary"))
							if lookupPathValue(value, "data.effect.amounts.0.resolution") != tc.resolution {
								t.Fatalf("effect=%v", value)
							}
							if lookupPathValue(value, "data.permission_check.status") != "pass" {
								t.Fatalf("permissions=%v", value)
							}
						} else if prompt == "" {
							t.Fatalf("no approval: %s", mcpResultText(result))
						}
						for _, want := range tc.want {
							if !strings.Contains(summary, want) {
								t.Errorf("missing %q: %s", want, summary)
							}
						}
						after, _ := json.Marshal(tc.args)
						if string(before) != string(after) {
							t.Fatal("request mutated")
						}
					})
				}
			}
		})
	}
}

func TestMCPImplicitLookupFailureIsExplicit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.Method != "GET" {
			t.Fatalf("unexpected mutation")
		}
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":{"code":"INSUFFICIENT_SCOPE","message":"Not permitted."}}`)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	result := mcpCall(t, app, defaultOptions(), "flint_write", map[string]any{"operation": "refunds.create", "arguments": map[string]any{"order_id": "ord_effect", "refund_type": "full"}, "dry_run": true, "idempotency_key": "lookup-denied"})
	if result["isError"] == true {
		t.Fatal(mcpResultText(result))
	}
	text := mcpResultText(result)
	for _, want := range []string{"unavailable before execution", "INSUFFICIENT_SCOPE", "customer unavailable"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s: %s", want, text)
		}
	}
}
