package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestAuditRemovedCommandsUseUnknownCommandSuggestions(t *testing.T) {
	app, stdout, _ := testApp(t, "")
	for _, name := range []string{
		"capabilities.list", "locations.geography.update", "locations.inventory.update",
		"organizations.transfer-ownership", "payouts.entries.list",
		"gift-cards.adjustments.create", "gift-cards.cash-outs.create",
		"merchant-subscription-invoices.get", "merchant-subscription-invoices.list", "webhook-events.deliveries",
	} {
		if _, ok := app.Registry.ByName(name); ok {
			t.Errorf("removed command %s is registered", name)
		}
	}
	exit, result := runJSON(t, app, stdout, "gift-cards", "cash-outs", "create", "gc_test")
	code, _ := lookupPath(result, "error.code")
	if exit != ExitUsage || code != "UNKNOWN_COMMAND" {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	suggestions, _ := lookupPath(result, "error.details.suggestions")
	if suggestions != nil && !strings.Contains(fmt.Sprint(suggestions), "cash-out") {
		t.Fatalf("unhelpful suggestions: %v", suggestions)
	}
}

func TestAuditRenamedParametersKeepTheirRequestBindings(t *testing.T) {
	for _, test := range []struct {
		argv []string
		path string
	}{
		{[]string{"checkout-sessions", "customer-verifications", "confirm", "cs_test", "cscv_test", "--input", `{"code":"123456"}`}, "/v1/checkout-sessions/cs_test/customer-verifications/cscv_test/confirm"},
		{[]string{"webhook-deliveries", "list", "--webhook-event-id", "whev_test"}, "/v1/webhook-deliveries?webhook_event_id=whev_test"},
		{[]string{"subscriptions", "list", "--next-billing-after", "2026-10-22"}, "/v1/subscriptions?next_billing_after=2026-10-22T00%3A00%3A00Z"},
		{[]string{"inventory-movements", "list", "--sort-direction", "asc"}, "/v1/inventory-movements?sort_direction=asc"},
	} {
		t.Run(strings.Join(test.argv[:2], "."), func(t *testing.T) {
			app, _, _ := testApp(t, "")
			command, options, _, err := parseInvocation(app.Registry, test.argv)
			if err != nil {
				t.Fatal(err)
			}
			request, err := app.prepareRequest(context.Background(), command, options, ResolvedConfig{}, "", "")
			if err != nil || request.Path != test.path {
				t.Fatalf("path=%s error=%v; want %s", request.Path, err, test.path)
			}
		})
	}
}

func TestAuditRefundRequiresAnExplicitMode(t *testing.T) {
	app, stdout, _ := testApp(t, "")
	args := []string{"refunds", "create", "--payment-intent", "pi_test", "--dry-run=client"}
	exit, result := runJSON(t, app, stdout, args...)
	if code, _ := lookupPath(result, "error.code"); exit != ExitUsage || code != "INVALID_REQUEST_BODY" {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	for _, mode := range []string{"full", "amount"} {
		argv := append(append([]string{}, args...), "--refund-type", mode)
		if mode == "amount" {
			argv = append(argv, "--amount", "100", "--currency", "USD")
		}
		if exit, result := runJSON(t, app, stdout, argv...); exit != ExitOK {
			t.Fatalf("%s exit=%d result=%v", mode, exit, result)
		}
	}
}

func TestAuditRefundWaitAllowsLaterBankRejections(t *testing.T) {
	command, _ := NewRegistry().ByName("refunds.get")
	conditions := []waitCondition{{Path: "status", Expected: "failed"}}
	for _, status := range []string{"succeeded", "partially_succeeded"} {
		if terminallyImpossible(command, conditions, map[string]string{"status": status}) {
			t.Errorf("refund status %s can still become failed", status)
		}
	}
}

func TestAuditConfirmationCredentialsAreExclusive(t *testing.T) {
	for _, flags := range [][]string{
		{"--confirmation-token", "ctoken_test"},
		{"--payment-source-token", "pm_card_visa"},
		{"--payment-method", "pm_test"},
		{},
		{"--confirmation-token", "ctoken_test", "--payment-method", "pm_test"},
	} {
		app, stdout, _ := testApp(t, "")
		argv := append([]string{"payment-intents", "confirm", "pi_test", "--dry-run=client"}, flags...)
		exit, result := runJSON(t, app, stdout, argv...)
		want := ExitOK
		if len(flags) > 2 {
			want = ExitUsage
		}
		if exit != want {
			t.Fatalf("flags=%v exit=%d result=%v", flags, exit, result)
		}
	}
}

func TestAuditCustomerEmailIsOptional(t *testing.T) {
	app, stdout, _ := testApp(t, "")
	if exit, result := runJSON(t, app, stdout, "customers", "create", "--name", "Ada", "--dry-run=client"); exit != ExitOK {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
}
