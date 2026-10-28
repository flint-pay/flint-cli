package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvenienceFlagsSelectRequiredUnionTags(t *testing.T) {
	for _, test := range []struct {
		argv []string
		path string
		want string
	}{
		{[]string{"payment-intents", "confirm", "pi_test", "--confirmation-token", "ctoken_test"}, "credential_type", "stripe_confirmation_token"},
		{[]string{"payment-intents", "confirm", "pi_test", "--payment-source-token", "pm_card_visa"}, "credential_type", "token"},
		{[]string{"payment-intents", "confirm", "pi_test", "--payment-method", "pm_test"}, "credential_type", "payment_method"},
		{[]string{"payment-intents", "confirm", "pi_test"}, "credential_type", "attached"},
		{[]string{"invoices", "create", "--order", "ord_test"}, "type", "order"},
		{[]string{"payment-links", "create", "--name", "Tee", "--item-name", "Tee", "--amount", "2500", "--currency", "USD"}, "line_items.0.type", "custom_fixed"},
		{[]string{"orders", "pay", "ord_test", "--payment-source-token", "pi_test=pm_card_visa"}, "payment_intents.0.credential_type", "token"},
	} {
		t.Run(strings.Join(test.argv, " "), func(t *testing.T) {
			app, stdout, _ := testApp(t, "")
			argv := append(append([]string{}, test.argv...), "--idempotency-key", "saved-test-action-1", "--dry-run=client")
			exit, result := runJSON(t, app, stdout, argv...)
			if got, _ := lookupPath(result, "data.body."+test.path); exit != ExitOK || got != test.want {
				t.Fatalf("exit=%d %s=%v result=%v", exit, test.path, got, result)
			}
		})
	}
	for _, argv := range [][]string{
		{"payment-intents", "confirm", "pi_test", "--payment-method", "pm_test", "--input", `{"credential_type":"token","payment_source_token":"pm_card_visa"}`},
		{"invoices", "create", "--order", "ord_test", "--input", `{"type":"quick_pay"}`},
	} {
		app, stdout, _ := testApp(t, "")
		argv = append(argv, "--idempotency-key", "saved-test-action-1", "--dry-run=client")
		if exit, result := runJSON(t, app, stdout, argv...); exit != ExitUsage {
			t.Fatalf("%v: exit=%d result=%v", argv, exit, result)
		}
	}
}

func TestChallengeTokenHeaderFollowsTheContract(t *testing.T) {
	t.Setenv("FLINT_CHALLENGE_TOKEN", "challenge_test")
	var header, legacy string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		header, legacy = r.Header.Get("Flint-Challenge-Token"), r.Header.Get("X-Turnstile-Token")
		fmt.Fprint(w, `{"data":{"demo_session_id":"demo_test"}}`)
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	if exit := app.Run([]string{"demo-sessions", "create", "--idempotency-key", "saved-test-action-1", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if header != "challenge_test" || legacy != "" {
		t.Fatalf("Flint-Challenge-Token=%q X-Turnstile-Token=%q", header, legacy)
	}
}
