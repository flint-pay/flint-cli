package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestMCPMoneyDisplaysCurrencyMinorUnits(t *testing.T) {
	for _, tc := range []struct{ amount, currency, want string }{
		{"2500", "USD", "25.00 USD"}, {"2500", "KWD", "2.500 KWD"}, {"2500", "JPY", "2500 JPY"},
		{"-1", "USD", "-0.01 USD"}, {"-9223372036854775808", "USD", "-92233720368547758.08 USD"}, {"1e3", "USD", "10.00 USD"},
	} {
		t.Run(tc.currency+tc.amount, func(t *testing.T) {
			if got := mcpMoneyDisplay(json.Number(tc.amount), tc.currency); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestMCPEffectIncludesNestedAmounts(t *testing.T) {
	body := map[string]any{"lines": []any{map[string]any{"amount_money": map[string]any{"amount": json.Number("1234"), "currency": "KWD"}}}, "amount_money": map[string]any{"amount": json.Number("100"), "currency": "JPY"}}
	before, _ := json.Marshal(body)
	effect := mcpEffect("Create a refund", true, false, true, body, []string{"ord_fixture"})
	amounts := effect["amounts"].([]map[string]any)
	if len(amounts) != 2 || amounts[0]["display"] != "100 JPY" || amounts[1]["field"] != "lines[0].amount_money" || amounts[1]["display"] != "1.234 KWD" || effect["confirm_required"] != true || effect["sensitive"] != true {
		t.Fatalf("effect=%v", effect)
	}
	after, _ := json.Marshal(body)
	if string(after) != string(before) {
		t.Fatal("effect changed the request")
	}
}

func TestMCPDryRunEffectIsOfflineWithoutLiveOrConfirm(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; t.Errorf("dry run sent %s", r.URL.Path) }))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "flint_live_fixture")
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	app.mcpAuth = &mcpAuthState{index: index, auth: AuthContext{AuthType: "api_key", Environment: "live", Scopes: allTestScopes()}, known: true}
	body := map[string]any{"order_id": "ord_fixture", "refund_type": "amount", "amount_money": map[string]any{"amount": 2500, "currency": "USD"}}
	result := mcpCall(t, app, defaultOptions(), "flint_write", map[string]any{"operation": "refunds.create", "arguments": body, "idempotency_key": "refund-offline-fixture", "dry_run": true})
	if result["isError"] == true || requests != 0 {
		t.Fatalf("requests=%d result=%s", requests, mcpResultText(result))
	}
	var envelope map[string]any
	raw := result["content"].([]map[string]any)[0]["text"].(string)
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatal(err)
	}
	data := envelope["data"].(map[string]any)
	effect := data["effect"].(map[string]any)
	if data["dry_run"] != true || data["operation"] != "refunds.create" || data["method"] != "POST" || data["path"] != "/v1/refunds" || data["idempotency_key"] != "refund-offline-fixture" || effect["confirm_required"] != true || !strings.Contains(fmt.Sprint(effect["summary"]), "25.00 USD") || !reflect.DeepEqual(effect["resources"], []any{"ord_fixture"}) || lookupPathValue(data, "permission_check.status") != "pass" {
		t.Fatalf("dryrun=%s", raw)
	}
	auth, known := app.mcpAuth.snapshot()
	if !known || len(auth.Scopes) == 0 {
		t.Fatal("offline dry run discarded known scopes")
	}
}

func TestMCPWriteSelectionKeepsDeclaredIdentity(t *testing.T) {
	app, _, _ := testApp(t, "")
	cmd := mustMCPTool(t, app, "customers.create")
	if got := mcpOwnIDField(cmd); got != "customer_id" {
		t.Fatalf("identity=%s", got)
	}
	e := mcpValidateSelect(cmd, map[string]any{"select": []any{"phone", "not_a_field"}})
	if e == nil || e.Code != "INVALID_REQUEST" || e.Param != "select[1]" {
		t.Fatalf("error=%v", e)
	}
}

func TestMCPDryRunWorkloadKeepsVerifiedPermissions(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_API_KEY", "")
	t.Setenv("FLINT_ACCESS_TOKEN", "")
	t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "wid_synthetic")
	t.Setenv("FLINT_OIDC_SOURCE", "github_actions")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "http://127.0.0.1:1/subject-token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "synthetic")
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	app.mcpAuth = &mcpAuthState{index: index, auth: AuthContext{AuthType: "workload_identity", Environment: "live", Scopes: allTestScopes()}, known: true}
	result := mcpCall(t, app, defaultOptions(), "flint_write", map[string]any{"operation": "refunds.create", "arguments": map[string]any{"payment_intent_id": "pi_synthetic", "refund_type": "full"}, "idempotency_key": "offline-workload", "dry_run": true})
	if result["isError"] == true {
		t.Fatal(mcpResultText(result))
	}
	data := mcpResultJSON(t, result).(map[string]any)
	if lookupPathValue(data, "data.permission_check.status") != "pass" {
		t.Fatalf("permissions=%v", data)
	}
	auth, known := app.mcpAuth.snapshot()
	if !known || auth.Environment != "live" || len(auth.Scopes) == 0 {
		t.Fatalf("dry run replaced verified context: %+v", auth)
	}
}

func TestMCPSelectedWriteIdentityThroughResponseVariants(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, tc := range []struct{ operation, id, selection string }{
		{"customers.create", "customer_id", "name"},
		{"refunds.create", "refund_id", "reason"},
		{"checkout-sessions.create", "checkout_session_id", "url"},
		{"api-keys.create", "api_key_id", "name"},
		{"payment-intents.create", "payment_intent_id", "capture_method"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			cmd := mustMCPTool(t, app, tc.operation)
			if got := mcpOwnIDField(cmd); got != tc.id {
				t.Fatalf("identity=%q want %s", got, tc.id)
			}
			value := map[string]any{"data": map[string]any{tc.id: "own_id", tc.selection: "selected", "status": "succeeded", "unselected": "hidden"}}
			got, warning, e := mcpProjectResult(context.Background(), cmd, value, Options{Select: []string{tc.selection}})
			if e != nil || warning != "" {
				t.Fatalf("error=%v warning=%s", e, warning)
			}
			if lookupPathValue(got, "data."+tc.id) != "own_id" || lookupPathValue(got, "data.status") != "succeeded" || lookupPathValue(got, "data.unselected") != nil {
				t.Fatalf("projection=%v", got)
			}
		})
	}
}
