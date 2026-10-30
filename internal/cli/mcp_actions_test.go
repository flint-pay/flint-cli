package cli

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func mcpActionAPIError(code, retry string, details []any) *CLIError {
	return apiErrorFromResponse(&apiResponse{Status: http.StatusForbidden, Headers: http.Header{}, Value: map[string]any{"error": map[string]any{"type": "authorization_error", "code": code, "message": "The request was refused.", "details": details, "remediation": map[string]any{"retry": retry}}}})
}

func TestMCPScopeActionsChooseCredentialSource(t *testing.T) {
	for _, source := range []string{"key", "login", "identity", "token"} {
		t.Run(source, func(t *testing.T) {
			app, _, _ := testApp(t, "")
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_ACCESS_TOKEN", "")
			t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
			switch source {
			case "key":
				t.Setenv("FLINT_API_KEY", "flint_test_synthetic")
			case "identity":
				t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "wid_synthetic")
			case "token":
				t.Setenv("FLINT_ACCESS_TOKEN", "synthetic-access-token")
			}
			cmd := mustMCPTool(t, app, "refunds.create")
			e := mcpActionAPIError("INSUFFICIENT_SCOPE", "none", []any{map[string]any{"missing_scopes": []any{"commerce.refunds.write"}}})
			action := app.mcpNextActions(cmd, nil, defaultOptions(), app.mcpPolicy(defaultOptions()), e, "sandbox")[0]
			if action["actor"] != "user" || !strings.HasPrefix(fmt.Sprint(action["text"]), "Ask the user to") || !strings.Contains(fmt.Sprint(action["text"]), "commerce.refunds.write") {
				t.Fatal(action)
			}
			switch source {
			case "login":
				if !strings.Contains(fmt.Sprint(action["command"]), "flint reauth --scope commerce.refunds.write") {
					t.Fatal(action)
				}
			case "key":
				if action["url"] != dashboardAPIKeysURL || !strings.Contains(fmt.Sprint(action["text"]), "restricted API key") {
					t.Fatal(action)
				}
			default:
				if action["command"] != nil {
					t.Fatal(action)
				}
			}
		})
	}
}

func TestMCPScopeActionsHonorLiveOnlyMetadata(t *testing.T) {
	app, _, _ := testApp(t, "")
	cmd := mustMCPTool(t, app, "organizations.create")
	for _, facts := range []map[string]any{
		{"missing_scopes": []any{"accounts.organizations.write"}},
		{"reason": "live_mode_required", "missing_scopes": []any{"future.scope"}},
		{"key_modes": []any{"live"}, "missing_scopes": []any{"future.scope"}},
	} {
		e := mcpActionAPIError("INSUFFICIENT_SCOPE", "none", []any{facts})
		action := app.mcpNextActions(cmd, nil, defaultOptions(), app.mcpPolicy(defaultOptions()), e, "sandbox")[0]
		if action["actor"] != "user" || !strings.Contains(fmt.Sprint(action["text"]), "cannot be added to a sandbox key") || action["command"] != nil {
			t.Fatal(action)
		}
	}
}

func TestMCPActionsRetryAndRecoveryUseCurrentTools(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(fmt.Sprint(typed), func(t *testing.T) {
			app, _, _ := testApp(t, "")
			opts := defaultOptions()
			if typed {
				opts = mcpAllToolsOptions(t)
			}
			policy := app.mcpPolicy(opts)
			cmd := mustMCPTool(t, app, "customers.create")
			args := map[string]any{"name": "A 'quoted' name", "_flint": map[string]any{"idempotency_key": "same-key", "select": []any{"name"}}}
			before := cloneErrorJSON(args)
			e := mcpActionAPIError("RATE_LIMIT_EXCEEDED", "same_request", nil)
			e.HTTPStatus = 429
			e.idempotencyKey = "same-key"
			actions := app.mcpNextActions(cmd, args, opts, policy, e, "sandbox")
			call := actions[0]["call"]
			if !reflect.DeepEqual(call, app.mcpOperationCall(cmd, args, policy)) || !reflect.DeepEqual(args, before) {
				t.Fatalf("call=%v args=%v", call, args)
			}
			refund := mustMCPTool(t, app, "refunds.create")
			unknown := networkError("CLIENT_TIMEOUT", "The request timed out.", nil)
			unknown.requestSent = true
			unknown.Details = map[string]any{"outcome": "unknown", "idempotency_key": "refund-key"}
			actions = app.mcpNextActions(refund, map[string]any{"payment_intent_id": "pi_synthetic"}, opts, policy, unknown, "sandbox")
			read := actions[0]["call"].(map[string]any)
			want := "flint_read"
			if typed {
				want = "refunds_list"
			}
			if read["tool"] != want || !strings.Contains(fmt.Sprint(read["arguments"]), "refund-key") || !strings.Contains(fmt.Sprint(actions[0]["text"]), "same idempotency_key") {
				t.Fatal(actions)
			}
		})
	}
}

func TestMCPAPINextActionsMapRoutesAndPreserveHumanOwnership(t *testing.T) {
	app, _, _ := testApp(t, "")
	raw := map[string]any{"type": "api_error", "code": "ACTION_NOT_ALLOWED", "message": "Read the payment.", "remediation": map[string]any{"retry": "none", "next_actions": []any{
		map[string]any{"action_type": "get_payment_intent", "url": "/v1/payment-intents/pi_synthetic", "reason_message": "Read the payment intent."},
		map[string]any{"action_type": "get_order", "url": "/v1/orders/ord_synthetic", "reason_message": "Approve this action.", "requires_human_confirmation": true},
	}}}
	before := cloneErrorJSON(raw)
	e := apiErrorFromResponse(&apiResponse{Status: 409, Headers: http.Header{}, Value: map[string]any{"error": raw}})
	actions := app.mcpNextActions(mustMCPTool(t, app, "refunds.create"), nil, defaultOptions(), app.mcpPolicy(defaultOptions()), e, "sandbox")
	if len(actions) != 2 || actions[0]["actor"] != "agent" || actions[1]["actor"] != "user" || !strings.HasPrefix(fmt.Sprint(actions[1]["text"]), "Ask the user to") {
		t.Fatal(actions)
	}
	call := actions[0]["call"].(map[string]any)
	args := call["arguments"].(map[string]any)
	if call["tool"] != "flint_read" || args["operation"] != "payment-intents.get" || args["arguments"].(map[string]any)["payment_intent_id"] != "pi_synthetic" || !reflect.DeepEqual(raw, before) {
		t.Fatalf("actions=%v raw=%v", actions, raw)
	}
	// Narrow tool scopes must never offer an agent a call to an unavailable tool.
	policy := app.mcpPolicy(mcpServeOptions(t, "--tools", "refunds"))
	actions = app.mcpNextActions(mustMCPTool(t, app, "refunds.create"), nil, defaultOptions(), policy, e, "sandbox")
	if actions[0]["actor"] != "user" {
		t.Fatal(actions)
	}
}

func TestMCPDefiniteRefusalNeverOffersAutomaticResend(t *testing.T) {
	app, _, _ := testApp(t, "")
	cmd := mustMCPTool(t, app, "customers.create")
	e := mcpActionAPIError("ACTION_NOT_ALLOWED", "none", nil)
	args := map[string]any{"name": "Fixture", "_flint": map[string]any{"idempotency_key": "released-key"}}
	actions := app.mcpNextActions(cmd, args, defaultOptions(), app.mcpPolicy(defaultOptions()), e, "sandbox")
	for _, action := range actions {
		call, _ := action["call"].(map[string]any)
		if call["tool"] == "flint_write" || call["tool"] == "customers_create" || strings.Contains(fmt.Sprint(action), "same idempotency_key") {
			t.Fatal(actions)
		}
	}
}

func TestMCPRefreshAndReadRetriesDoNotLoseTheirResource(t *testing.T) {
	app, _, _ := testApp(t, "")
	policy := app.mcpPolicy(defaultOptions())
	cmd := mustMCPTool(t, app, "customers.update")
	e := mcpActionAPIError("VERSION_CONFLICT", "after_refresh", nil)
	e.HTTPStatus = 409
	actions := app.mcpNextActions(cmd, map[string]any{"customer_id": "cus_synthetic", "name": "Fixture"}, defaultOptions(), policy, e, "sandbox")
	call := actions[0]["call"].(map[string]any)
	if call["tool"] != "flint_read" || lookupPathValue(call, "arguments.operation") != "customers.get" || lookupPathValue(call, "arguments.arguments.customer_id") != "cus_synthetic" {
		t.Fatal(actions)
	}
	read := mustMCPTool(t, app, "customers.get")
	unavailable := mcpActionAPIError("SERVICE_UNAVAILABLE", "same_request", nil)
	unavailable.HTTPStatus = 503
	result := app.mcpCommandError(read, map[string]any{"customer_id": "cus_synthetic"}, defaultOptions(), mcpCallResult{cliError: unavailable, exit: unavailable.ExitCode}, policy, "")
	envelope := mcpErrorJSON(t, result)
	if lookupPathValue(envelope, "cli.outcome") != nil || lookupPathValue(envelope, "cli.next_actions.0.call.arguments.operation") != "customers.get" {
		t.Fatal(envelope)
	}
}

func TestMCPScopeAnyAndFutureModesDoNotForceLive(t *testing.T) {
	app, _, _ := testApp(t, "")
	cmd := mustMCPTool(t, app, "organizations.get")
	for _, facts := range []map[string]any{
		{"missing_scopes": []any{"accounts.organizations.write", "customers.read"}, "scope_mode": "any"},
		{"missing_scopes": []any{"future.scope"}, "key_modes": []any{"live", "future"}},
	} {
		e := mcpActionAPIError("INSUFFICIENT_SCOPE", "none", []any{facts})
		action := app.mcpNextActions(cmd, nil, defaultOptions(), app.mcpPolicy(defaultOptions()), e, "sandbox")[0]
		if strings.Contains(fmt.Sprint(action), "cannot be added to a sandbox key") {
			t.Fatal(action)
		}
	}
}

func TestMCPValidationActionsOnlyDescribeDeclaredVariants(t *testing.T) {
	app, _, _ := testApp(t, "")
	cmd := mustMCPTool(t, app, "refunds.create")
	for _, variant := range []string{"amount", "not-a-variant"} {
		args := map[string]any{"refund_type": variant}
		e := usageError("INVALID_REQUEST", "The arguments are invalid.", "refund_type")
		actions := app.mcpNextActions(cmd, args, defaultOptions(), app.mcpPolicy(defaultOptions()), e, "sandbox")
		call := actions[0]["call"].(map[string]any)
		if variant == "amount" && lookupPathValue(call, "arguments.variant") != "amount" {
			t.Fatal(actions)
		}
		if variant != "amount" && lookupPathValue(call, "arguments.variant") != nil {
			t.Fatal(actions)
		}
	}
}
