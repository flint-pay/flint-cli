package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateBaseURL(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
		valid bool
	}{
		{name: "https", value: "https://api.example.com/", want: "https://api.example.com", valid: true},
		{name: "localhost", value: "http://localhost:8080/", want: "http://localhost:8080", valid: true},
		{name: "localhost subdomain", value: "http://api.localhost:8080", want: "http://api.localhost:8080", valid: true},
		{name: "ipv4 loopback", value: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080", valid: true},
		{name: "ipv6 loopback", value: "http://[::1]:8080", want: "http://[::1]:8080", valid: true},
		{name: "remote http", value: "http://api.withflintpay.com", valid: false},
		{name: "lookalike localhost", value: "http://localhost.example.com", valid: false},
		{name: "credentials", value: "https://user:secret@example.com", valid: false},
		{name: "query", value: "https://example.com?token=secret", valid: false},
		{name: "fragment", value: "https://example.com#fragment", valid: false},
		{name: "relative", value: "/v1", valid: false},
		{name: "unsupported scheme", value: "file:///tmp/flint", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateBaseURL(test.value)
			if test.valid && (err != nil || got != test.want) {
				t.Fatalf("validateBaseURL(%q) = %q, %v; want %q", test.value, got, err, test.want)
			}
			if !test.valid && err == nil {
				t.Fatalf("validateBaseURL(%q) unexpectedly succeeded with %q", test.value, got)
			}
		})
	}
}

func TestNonJSONUnauthorizedResponsePreservesAuthExitAndRequestID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req_plaintext")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "unauthorized")
	}))
	defer server.Close()

	app, _, _ := testApp(t, server.URL)
	_, cliErr := app.doRequest(context.Background(), server.URL, "flint_test_test", http.MethodGet, "/v1/test", nil, "", false)
	if cliErr == nil || cliErr.ExitCode != ExitAuth || cliErr.Code != "HTTP_401" || cliErr.RequestID != "req_plaintext" {
		t.Fatalf("error = %#v", cliErr)
	}
}

func TestAPIClientDoesNotForwardCredentialsAcrossRedirects(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	app, _, _ := testApp(t, server.URL)
	_, cliErr := app.doRequest(context.Background(), server.URL, "flint_test_secret", http.MethodGet, "/v1/test", nil, "", false)
	if cliErr == nil || cliErr.ExitCode != ExitAPI || cliErr.Code != "HTTP_302" || reached {
		t.Fatalf("error=%#v reached=%t", cliErr, reached)
	}
}

func TestJSONErrorUsesHeaderRequestIDWhenBodyOmitsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_header")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"type":"validation_error","code":"INVALID","message":"invalid"}}`)
	}))
	defer server.Close()

	app, stdout, _ := testApp(t, server.URL)
	_, cliErr := app.doRequest(context.Background(), server.URL, "flint_test_test", http.MethodGet, "/v1/test", nil, "", false)
	if cliErr == nil {
		t.Fatal("expected API error")
	}
	app.writeError(cliErr, Options{Output: "json"})
	if !strings.Contains(stdout.String(), `"request_id":"req_header"`) {
		t.Fatalf("request ID missing from JSON error: %s", stdout)
	}
}

func TestReadInputRejectsTrailingJSONAndOversizedDocuments(t *testing.T) {
	app, _, _ := testApp(t, "")
	app.Stdin = strings.NewReader(`{"first":1} {"second":2}`)
	if _, cliErr := app.readInput("-"); cliErr == nil || cliErr.Code != "INVALID_INPUT_JSON" {
		t.Fatalf("trailing input error = %#v", cliErr)
	}

	app.Stdin = strings.NewReader(`{"value":"` + strings.Repeat("x", 8<<20) + `"}`)
	if _, cliErr := app.readInput("-"); cliErr == nil || cliErr.Code != "INPUT_TOO_LARGE" {
		t.Fatalf("oversized input error = %#v", cliErr)
	}
}

func TestCapabilityFlagsFailInsteadOfBeingIgnored(t *testing.T) {
	tests := [][]string{
		{"auth", "status", "--wait-for", "environment=sandbox"},
		{"auth", "status", "--expand", "order"},
		{"mcp", "serve", "--field", "data"},
		{"customers", "list", "--input", "-"},
		{"customers", "list", "--confirm"},
		{"customers", "list", "--open=false"},
		{"customers", "list", "--paginate=false"},
		{"customers", "list", "--field="},
		{"version", "--progress", "plain"},
	}
	registry := NewRegistry()
	for _, argv := range tests {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			_, _, _, cliErr := parseInvocation(registry, argv)
			if cliErr == nil || cliErr.ExitCode != ExitUsage {
				t.Fatalf("parseInvocation(%v) error = %#v", argv, cliErr)
			}
		})
	}
}

func TestEveryCommandHelpRendersWithoutAuthentication(t *testing.T) {
	for _, cmd := range NewRegistry().Commands {
		t.Run(cmd.CanonicalName, func(t *testing.T) {
			app, stdout, stderr := testApp(t, "")
			argv := append([]string(nil), cmd.Path[1:]...)
			argv = append(argv, "--help")
			if exit := app.Run(argv); exit != ExitOK || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			if !strings.Contains(stdout.String(), "Usage:") || !strings.Contains(stdout.String(), strings.Join(cmd.Path, " ")) {
				t.Fatalf("incomplete help for %s: %s", cmd.CanonicalName, stdout)
			}
		})
	}

	app, stdout, stderr := testApp(t, "")
	if exit := app.Run([]string{"help"}); exit != ExitOK || stderr.Len() != 0 || !strings.Contains(stdout.String(), "flint <command>") {
		t.Fatalf("root help exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

func TestLocalConfigAndSchemaCommands(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	t.Setenv("FLINT_API_KEY", "")

	for _, test := range []struct {
		argv []string
		want string
	}{
		{argv: []string{"config", "set", "profile", "qa", "--output", "json"}, want: `"value":"qa"`},
		{argv: []string{"config", "set", "merchant", "mer_guard", "--output", "json"}, want: `"value":"mer_guard"`},
		{argv: []string{"config", "get", "--output", "json"}, want: `"merchant_guard":"mer_guard"`},
		{argv: []string{"config", "validate", "--output", "json"}, want: `"status":"not_configured"`},
		{argv: []string{"schema", "commands"}, want: `"name":"customers.list"`},
		{argv: []string{"schema", "command", "customers.list"}, want: `"canonical_name":"customers.list"`},
		{argv: []string{"schema", "input", "customers.list"}, want: `"$schema":"https://json-schema.org/draft/2020-12/schema"`},
		{argv: []string{"schema", "output", "customers.list"}, want: `"$schema":"https://json-schema.org/draft/2020-12/schema"`},
		{argv: []string{"schema", "errors"}, want: `"wait_condition_unmet"`},
		{argv: []string{"schema", "events"}, want: `"webhooks"`},
	} {
		stdout.Reset()
		stderr.Reset()
		if exit := app.Run(test.argv); exit != ExitOK || stderr.Len() != 0 || !strings.Contains(stdout.String(), test.want) {
			t.Fatalf("argv=%v exit=%d stdout=%s stderr=%s", test.argv, exit, stdout, stderr)
		}
	}
}

func TestOfflineHelpDoctorFixAndHumanRemediation(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	for _, topic := range []string{"exit-codes", "errors", "events", "test-cards"} {
		stdout.Reset()
		stderr.Reset()
		if exit := app.Run([]string{"help", topic}); exit != ExitOK || stderr.Len() != 0 || stdout.Len() == 0 {
			t.Fatalf("topic=%s exit=%d stdout=%s stderr=%s", topic, exit, stdout, stderr)
		}
	}

	if err := app.saveConfig(Config{DefaultProfile: "stale", Profiles: map[string]Profile{"default": {}}}); err != nil {
		t.Fatal(err)
	}
	changes, err := app.applyDoctorFixes()
	if err != nil || len(changes) != 1 {
		t.Fatalf("changes=%v err=%v", changes, err)
	}
	cfg, err := app.loadConfig()
	if err != nil || cfg.DefaultProfile != "default" {
		t.Fatalf("config=%#v err=%v", cfg, err)
	}

	t.Setenv("FLINT_API_KEY", "")
	stdout.Reset()
	stderr.Reset()
	if exit := app.Run([]string{"orders", "get", "ord_123"}); exit != ExitAuth || !strings.Contains(stderr.String(), "Next: flint auth import") || !strings.Contains(stderr.String(), "Next: flint signup") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}

	var human strings.Builder
	renderHuman(&human, "ready", nil, time.Now())
	if human.String() != "ready\n" {
		t.Fatalf("human scalar output = %q", human.String())
	}
	if got := (&CLIError{Message: "failure"}).Error(); got != "failure" {
		t.Fatalf("error string = %q", got)
	}
}

func TestHistoryClearAcceptsExplicitConfirmation(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	if err := app.saveHistory(History{Entries: []HistoryEntry{{ID: "pi_123", Profile: "default", Environment: "sandbox"}}}); err != nil {
		t.Fatal(err)
	}
	exit := app.Run([]string{"history", "--clear", "--confirm", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"data":[]`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

func TestConfigPrecedenceAndSandboxGuard(t *testing.T) {
	app, _, _ := testApp(t, "")
	if err := app.saveConfig(Config{
		DefaultProfile: "global",
		Profiles: map[string]Profile{
			"global":  {MerchantGuard: "mer_global"},
			"project": {MerchantGuard: "mer_profile"},
			"env":     {MerchantGuard: "mer_env_profile"},
			"flag":    {MerchantGuard: "mer_flag_profile"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	projectRoot := t.TempDir()
	projectDir := filepath.Join(projectRoot, ".flint")
	if err := os.MkdirAll(projectDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "config.json"), []byte(`{"profile":"project","merchant":"mer_project","sandbox":"test_project"}`), 0600); err != nil {
		t.Fatal(err)
	}
	app.WorkingDir = filepath.Join(projectRoot, "nested")
	if err := os.MkdirAll(app.WorkingDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLINT_PROFILE", "env")
	t.Setenv("FLINT_MERCHANT", "mer_environment")

	resolved, _, err := app.resolveConfig(Options{Profile: "flag", Merchant: "mer_flag"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProfileName != "flag" || resolved.MerchantGuard != "mer_flag" || resolved.SandboxGuard != "test_project" {
		t.Fatalf("resolved = %#v", resolved)
	}
	if resolved.Sources["profile"] != "flag" || resolved.Sources["merchant_guard"] != "flag" || resolved.Sources["sandbox_guard"] != "project" {
		t.Fatalf("sources = %#v", resolved.Sources)
	}
	if cliErr := validateCredentialIntent(AuthContext{Environment: "sandbox", MerchantID: "mer_flag", SandboxID: "test_other"}, Options{}, resolved.MerchantGuard, resolved.SandboxGuard); cliErr == nil || cliErr.Code != "SANDBOX_GUARD_MISMATCH" {
		t.Fatalf("sandbox guard error = %#v", cliErr)
	}
}

func TestLogoutRefusesToClaimEnvironmentCredentialWasRemoved(t *testing.T) {
	app, stdout, _ := testApp(t, "")
	t.Setenv("FLINT_API_KEY", "flint_test_environment")
	deleted := false
	app.DeleteCredential = func(string) error { deleted = true; return nil }
	exit := app.Run([]string{"auth", "logout", "--confirm", "--output", "json"})
	if exit != ExitAuth || deleted || !strings.Contains(stdout.String(), "ENVIRONMENT_CREDENTIAL_ACTIVE") {
		t.Fatalf("exit=%d deleted=%t stdout=%s", exit, deleted, stdout)
	}
}

func TestAuthImportRollsBackCredentialWhenConfigWriteFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, authContextJSON("sandbox"))
	}))
	defer server.Close()

	app, stdout, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	app.Stdin = strings.NewReader("flint_test_new\n")
	app.LoadCredential = func(string) (string, error) { return "flint_test_previous", nil }
	var stored []string
	app.StoreCredential = func(_ string, secret string) error {
		stored = append(stored, secret)
		if len(stored) == 1 {
			path, err := app.configPath()
			if err != nil {
				return err
			}
			return os.Mkdir(path, 0700)
		}
		return nil
	}
	app.DeleteCredential = func(string) error { return errors.New("unexpected delete") }
	exit := app.Run([]string{"auth", "import", "--stdin", "--output", "json"})
	if exit != ExitAuth || len(stored) != 2 || stored[0] != "flint_test_new" || stored[1] != "flint_test_previous" {
		t.Fatalf("exit=%d stored=%v stdout=%s", exit, stored, stdout)
	}
}

func TestAllPaginationUsesTimeoutPerRequest(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		pages++
		// Keep each request comfortably inside its timeout while making the
		// aggregate runtime exceed that timeout. This proves --timeout is scoped
		// to each page without relying on a scheduler-sensitive deadline.
		time.Sleep(50 * time.Millisecond)
		next := ""
		if pages < 5 {
			next = fmt.Sprintf("page_%d", pages+1)
		}
		fmt.Fprintf(w, `{"data":[{"customer_id":"cus_%d"}],"next_page_token":%q}`, pages, next)
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"customers", "list", "--all", "--timeout", "200ms", "--output", "json"})
	if exit != ExitOK || pages != 5 || stderr.Len() != 0 {
		t.Fatalf("exit=%d pages=%d stdout=%s stderr=%s", exit, pages, stdout, stderr)
	}
}

func TestRawAPIPaginationUsesDocumentedRouteAndWritesEveryPage(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.URL.Path != "/v1/customers" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		pages++
		if pages == 1 && r.URL.Query().Get("page_token") != "" {
			t.Fatalf("first page token = %q", r.URL.Query().Get("page_token"))
		}
		if pages == 2 && r.URL.Query().Get("page_token") != "next" {
			t.Fatalf("second page token = %q", r.URL.Query().Get("page_token"))
		}
		next := ""
		if pages == 1 {
			next = "next"
		}
		fmt.Fprintf(w, `{"data":[{"customer_id":"cus_%d"}],"next_page_token":%q}`, pages, next)
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	if exit := app.Run([]string{"api", "get", "/v1/customers", "--paginate", "--output", "json"}); exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if pages != 2 || strings.Count(strings.TrimSpace(stdout.String()), "\n") != 1 || !strings.Contains(stdout.String(), `"customer_id":"cus_2"`) {
		t.Fatalf("pages=%d stdout=%s", pages, stdout)
	}
	if cliErr := validateRawPublicRoute(http.MethodGet, "/v1/private"); cliErr == nil || cliErr.Code != "UNDOCUMENTED_API_OPERATION" {
		t.Fatalf("undocumented route error = %#v", cliErr)
	}
}

func TestNestedPaymentLinkFlagsBuildOneLineItem(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	exit := app.Run([]string{
		"payment-links", "create", "--name", "T-shirt", "--item-name", "T-shirt",
		"--amount", "2500", "--currency", "USD", "--dry-run", "client", "--output", "json",
	})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	lineItems, ok := envelope["data"].(map[string]any)["body"].(map[string]any)["line_items"].([]any)
	if !ok || len(lineItems) != 1 {
		t.Fatalf("line items = %#v", lineItems)
	}
	item := lineItems[0].(map[string]any)
	money := item["unit_price_money"].(map[string]any)
	if item["name"] != "T-shirt" || money["amount"] != float64(2500) || money["currency"] != "USD" {
		t.Fatalf("item = %#v", item)
	}
}

func TestOrderPaymentBareTokenResolvesSingleAwaitingIntent(t *testing.T) {
	var paid map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/developer/auth-context":
			fmt.Fprint(w, authContextJSON("sandbox"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/orders/ord_123":
			if r.URL.Query().Get("expand") != "payment_intents" {
				t.Fatalf("expand = %q", r.URL.Query().Get("expand"))
			}
			fmt.Fprint(w, `{"data":{"payment_intents":[{"payment_intent_id":"pi_ready","status":"requires_payment_method"},{"payment_intent_id":"pi_done","status":"succeeded"}]}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/orders/ord_123/pay":
			if err := json.NewDecoder(r.Body).Decode(&paid); err != nil {
				t.Fatal(err)
			}
			fmt.Fprint(w, `{"data":{"order":{"order_id":"ord_123"}}}`)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"orders", "pay", "ord_123", "--payment-source-token", "pm_card_visa", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	intents, ok := paid["payment_intents"].([]any)
	if !ok || len(intents) != 1 {
		t.Fatalf("paid body = %#v", paid)
	}
	selection := intents[0].(map[string]any)
	if selection["payment_intent_id"] != "pi_ready" || selection["token"] != "pm_card_visa" {
		t.Fatalf("selection = %#v", selection)
	}
}

func TestRelativeTimesFlagSuggestionsAndDebugMetadata(t *testing.T) {
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	for value, want := range map[string]string{
		"yesterday":                 "2026-07-20T12:00:00Z",
		"2h":                        "2026-07-21T10:00:00Z",
		"2026-07-21T08:00:00-04:00": "2026-07-21T12:00:00Z",
	} {
		got, err := resolveTime(value, now)
		if err != nil || got != want {
			t.Fatalf("resolveTime(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	if _, err := resolveTime("soon", now); err == nil {
		t.Fatal("invalid relative time unexpectedly succeeded")
	}
	_, _, _, cliErr := parseInvocation(NewRegistry(), []string{"customers", "list", "--outpt", "json"})
	if cliErr == nil || !strings.Contains(fmt.Sprint(cliErr.Details), "--output") {
		t.Fatalf("flag suggestion error = %#v", cliErr)
	}
	if !isSensitivePath("/v1/api-keys") || !isSensitivePath("/v1/developer/sandboxes/test_123/test-key") || isSensitivePath("/v1/customers") {
		t.Fatal("sensitive path classification is incorrect")
	}
	var debug strings.Builder
	debugResponse(&debug, &apiResponse{Status: http.StatusOK, Value: map[string]any{"meta": map[string]any{"api_version": "2026-02-01", "trace_id": "trace_123"}}})
	if !strings.Contains(debug.String(), "api_version=2026-02-01 trace_id=trace_123") {
		t.Fatalf("debug output = %q", debug.String())
	}
	if err := openBrowser("file:///tmp/secret"); err == nil {
		t.Fatal("unsafe browser URL unexpectedly succeeded")
	}
}

func TestPaginationRejectsRepeatedTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		fmt.Fprint(w, `{"data":[],"next_page_token":"repeat"}`)
	}))
	defer server.Close()

	app, stdout, _ := testApp(t, server.URL)
	exit := app.Run([]string{"customers", "list", "--all", "--output", "json"})
	if exit != ExitAPI || !strings.Contains(stdout.String(), "PAGINATION_TOKEN_LOOP") {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
}

func TestDurationBoundedListenLimitsInFlightRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	start := time.Now()
	exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--for", "25ms", "--timeout", "1s", "--output", "json"})
	elapsed := time.Since(start)
	if exit != ExitOK || elapsed >= 125*time.Millisecond || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"type":"checkpoint"`) {
		t.Fatalf("exit=%d elapsed=%s stdout=%s stderr=%s", exit, elapsed, stdout, stderr)
	}
}

func TestListenIdleTimeoutJoinsBlockedForwardBeforeReturning(t *testing.T) {
	forwardStarted := make(chan struct{})
	forwardStopped := make(chan struct{})

	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		_, _ = fmt.Fprint(writer, "event: webhook.event\nid: whev_01ABCDEFGHIJKLMNOPQRSTUVWX\ndata: {\"webhook_event_id\":\"whev_01ABCDEFGHIJKLMNOPQRSTUVWX\",\"event_type\":\"order.created\",\"payload\":{\"order_id\":\"ord_1\"}}\n\n")
	}()

	app, stdout, _ := testApp(t, "")
	app.ForwardHTTPClient = &http.Client{Transport: listenRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(forwardStarted)
		<-request.Context().Done()
		close(forwardStopped)
		return nil, request.Context().Err()
	})}
	opts := defaultOptions()
	opts.Output = "ndjson"
	opts.Timeout = 25 * time.Millisecond
	state := &listenStreamState{}
	streamErr := app.consumeListenStreamWithIdleTimeout(context.Background(), reader, opts, "http://127.0.0.1/webhooks", "whsec_dGVzdA==", state)
	if streamErr == nil || streamErr.Code != "REQUEST_TIMEOUT" {
		t.Fatalf("stream error = %#v", streamErr)
	}
	select {
	case <-forwardStarted:
	default:
		t.Fatal("local forward did not start")
	}
	select {
	case <-forwardStopped:
	default:
		t.Fatal("local forward was still running after timeout returned")
	}
	outputAtReturn := stdout.String()
	time.Sleep(20 * time.Millisecond)
	if stdout.String() != outputAtReturn {
		t.Fatalf("listen wrote output after returning: before=%q after=%q", outputAtReturn, stdout.String())
	}
}

func TestDialLoopbackAddressesTriesEveryValidatedAddress(t *testing.T) {
	addresses := []netip.Addr{netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.1")}
	var attempts []string
	peer, remote := net.Pipe()
	defer remote.Close()
	connection, err := dialLoopbackAddresses(context.Background(), "tcp", "8080", addresses, func(_ context.Context, _, address string) (net.Conn, error) {
		attempts = append(attempts, address)
		if strings.Contains(address, "127.0.0.1") {
			return peer, nil
		}
		return nil, errors.New("address family unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if len(attempts) != 2 || !strings.Contains(attempts[0], "::1") || !strings.Contains(attempts[1], "127.0.0.1") {
		t.Fatalf("dial attempts = %#v", attempts)
	}
}

func TestHistoryRecordingIsDeterministicAndDeduplicated(t *testing.T) {
	app, _, _ := testApp(t, "")
	instant := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	app.Now = func() time.Time { return instant }
	scope := historyScope{Profile: "default", Environment: "sandbox", MerchantID: "mer_123", SandboxID: "test_123"}
	value := map[string]any{"data": map[string]any{"payment_intent_id": "pi_z", "related_payment_intent_id": "pi_a"}}
	if err := app.recordHistory(value, "payment-intents.get", scope); err != nil {
		t.Fatal(err)
	}
	resolved, resolveErr := app.resolveHistoryRef("@last.pi", "", scope)
	if resolveErr != nil || resolved != "pi_z" {
		t.Fatalf("primary history reference = %q, %v", resolved, resolveErr)
	}
	instant = instant.Add(time.Minute)
	if err := app.recordHistory(map[string]any{"data": map[string]any{"id": "pi_a"}}, "payment-intents.get", scope); err != nil {
		t.Fatal(err)
	}
	history, err := app.loadHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Entries) != 2 || history.Entries[0].ID != "pi_a" || history.Entries[1].ID != "pi_z" {
		t.Fatalf("history = %#v", history.Entries)
	}
}

func TestWebhookDeliveryHistoryUsesExactSandboxScope(t *testing.T) {
	app, _, _ := testApp(t, "")
	first := historyScope{Profile: "default", Environment: "sandbox", MerchantID: "mer_123", SandboxID: "test_first"}
	second := historyScope{Profile: "default", Environment: "sandbox", MerchantID: "mer_123", SandboxID: "test_second"}
	if err := app.recordHistory(
		map[string]any{"data": map[string]any{"webhook_delivery_id": "wdel_123"}},
		"webhook-deliveries.get",
		first,
	); err != nil {
		t.Fatal(err)
	}
	if resolved, resolveErr := app.resolveHistoryRef("@last.wdel", "", first); resolveErr != nil || resolved != "wdel_123" {
		t.Fatalf("delivery history reference = %q, %v", resolved, resolveErr)
	}
	if resolved, resolveErr := app.resolveHistoryRef("@last", "wdel_", second); resolveErr == nil || resolved != "" {
		t.Fatalf("cross-sandbox history reference = %q, %v", resolved, resolveErr)
	}
}

type failingResponseBody struct{}

func (failingResponseBody) Read([]byte) (int, error) {
	return 0, errors.New("connection reset while reading")
}
func (failingResponseBody) Close() error { return nil }

func TestMutationResponseReadRetryReusesIdempotencyKey(t *testing.T) {
	attempts := 0
	keys := []string{}
	app, _, _ := testApp(t, "https://api.example.com")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		keys = append(keys, req.Header.Get("Idempotency-Key"))
		body := io.NopCloser(strings.NewReader(`{"data":{"payment_intent_id":"pi_123"}}`))
		if attempts == 1 {
			body = failingResponseBody{}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
	})}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, cliErr := app.doRequest(ctx, "https://api.example.com", "flint_test_key", http.MethodPost, "/v1/payment-intents", []byte(`{}`), "idem_same", false)
	if cliErr != nil || response == nil || attempts != 2 || len(keys) != 2 || keys[0] != "idem_same" || keys[1] != "idem_same" {
		t.Fatalf("response=%#v err=%v attempts=%d keys=%v", response, cliErr, attempts, keys)
	}
}

func TestMutationNetworkErrorPreservesIdempotencyKey(t *testing.T) {
	app, _, _ := testApp(t, "https://api.example.com")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, cliErr := app.doRequest(ctx, "https://api.example.com", "flint_test_key", http.MethodPost, "/v1/payment-intents", []byte(`{}`), "idem_preserved", false)
	if cliErr == nil {
		t.Fatal("expected network error")
	}
	details, ok := cliErr.Details.(map[string]any)
	if !ok || details["idempotency_key"] != "idem_preserved" {
		t.Fatalf("network error details = %#v", cliErr.Details)
	}
}

func TestMutationFinalRetryableStatusPreservesIdempotencyKey(t *testing.T) {
	app, _, _ := testApp(t, "https://api.example.com")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"TEMPORARILY_UNAVAILABLE","message":"retry later"}}`)),
			Request:    req,
		}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, cliErr := app.doRequest(ctx, "https://api.example.com", "flint_test_key", http.MethodPost, "/v1/payment-intents", []byte(`{}`), "idem_retryable", false)
	if cliErr == nil {
		t.Fatal("expected retryable API error")
	}
	details, ok := cliErr.Details.(map[string]any)
	if !ok || details["idempotency_key"] != "idem_retryable" {
		t.Fatalf("retryable API error details = %#v", cliErr.Details)
	}
}

func TestMCPServerEnforcesLifecycle(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
	}, "\n") + "\n"
	app, stdout, stderr := testApp(t, "")
	app.Stdin = strings.NewReader(input)
	exit := app.Run([]string{"mcp", "serve"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("responses=%d: %s", len(lines), stdout)
	}
	var responses []map[string]any
	for _, line := range lines {
		var response map[string]any
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
	}
	if code, _ := lookupPath(responses[0], "error.code"); code != float64(-32002) {
		t.Fatalf("pre-initialize response = %#v", responses[0])
	}
	if version, _ := lookupPath(responses[1], "result.protocolVersion"); version != "2025-11-25" {
		t.Fatalf("initialize response = %#v", responses[1])
	}
	if _, ok := lookupPath(responses[2], "result.tools"); !ok {
		t.Fatalf("tools response = %#v", responses[2])
	}
	if code, _ := lookupPath(responses[3], "error.code"); code != float64(-32600) {
		t.Fatalf("reinitialize response = %#v", responses[3])
	}
}

func TestMCPFlagsSerializeBooleansAndInheritServerContext(t *testing.T) {
	argv := appendMCPFlag(nil, "--clear", false)
	if len(argv) != 1 || argv[0] != "--clear=false" {
		t.Fatalf("boolean argv = %#v", argv)
	}
	opts := defaultOptions()
	opts.Profile = "staging"
	opts.Merchant = "mer_guard"
	opts.Live = true
	opts.Timeout = 45 * time.Second
	opts.Raw["profile"] = []string{"staging"}
	opts.Raw["merchant"] = []string{"mer_guard"}
	opts.Raw["live"] = []string{"true"}
	opts.Raw["timeout"] = []string{"45s"}
	argv = appendInheritedMCPOptions(nil, opts)
	joined := strings.Join(argv, " ")
	for _, expected := range []string{"--profile staging", "--merchant mer_guard", "--live=true", "--timeout 45s"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("inherited argv %q is missing %q", joined, expected)
		}
	}
}

func TestMCPOutputTransformsRemainStructuredAndSchemaCompatible(t *testing.T) {
	app, _, _ := testApp(t, "")
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{
		"name": "version",
		"arguments": map[string]any{
			"_flint": map[string]any{"field": "data.cli_version"},
		},
	}}, defaultOptions())
	result := response["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("MCP transform failed: %#v", result)
	}
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok || structured["value"] != "test" {
		t.Fatalf("structured transform result = %#v", result)
	}
	content := result["content"].([]map[string]any)
	if len(content) != 1 || content[0]["text"] != `{"value":"test"}` {
		t.Fatalf("text transform result = %#v", content)
	}

	malformed := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{
		"name":      "version",
		"arguments": map[string]any{"_flint": map[string]any{"select": []any{true}}},
	}}, defaultOptions())
	malformedResult := malformed["result"].(map[string]any)
	if malformedResult["isError"] != true || strings.Contains(fmt.Sprint(malformedResult), "structuredContent") {
		t.Fatalf("malformed transform result = %#v", malformedResult)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestBoundedListenReportsOutputWriteFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()

	app, _, _ := testApp(t, server.URL)
	app.Stdout = failingWriter{}
	exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--for", "1ms", "--output", "json"})
	if exit != ExitNetwork {
		t.Fatalf("exit=%d", exit)
	}
}

func TestSignupTransportTimeoutDoesNotIncludeHumanInputDelay(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		requests++
		switch r.URL.Path {
		case "/v1/onboarding/start":
			fmt.Fprint(w, `{"data":{"verification_token":"devver_123"}}`)
		case "/v1/onboarding/verify-email":
			fmt.Fprint(w, `{"data":{"onboarding_session_token":"devsess_123"}}`)
		case "/v1/onboarding/state":
			fmt.Fprint(w, `{"data":{"can_issue_api_key":false,"next_step":{"code":"refresh_account","machine_completable":true,"submit_method":"POST","submit_endpoint":"/v1/onboarding/advance"}}}`)
		case "/v1/onboarding/advance":
			fmt.Fprint(w, `{"data":{"can_issue_api_key":true,"default_sandbox_id":"test_123"}}`)
		case "/v1/onboarding/api-key":
			fmt.Fprint(w, `{"data":{"api_key_id":"key_123","merchant_id":"mer_123","sandbox_id":"test_123","secret_key":"flint_test_new"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	app.IsTTY = func() bool { return true }
	app.Stdin = &delayedReader{delay: 40 * time.Millisecond, reader: strings.NewReader("482193\n")}
	exit := app.Run([]string{"signup", "--email", "dev@example.com", "--first-name", "Ada", "--last-name", "Lovelace", "--timeout", "20ms", "--output", "human"})
	if exit != ExitOK || requests != 5 || !strings.Contains(stdout.String(), "Credential saved") {
		t.Fatalf("exit=%d requests=%d stdout=%s stderr=%s", exit, requests, stdout, stderr)
	}
}

func TestSignupHumanActionBeforeInitialKeyFailsWithActionableState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/onboarding/start":
			fmt.Fprint(w, `{"data":{"verification_token":"devver_123"}}`)
		case "/v1/onboarding/verify-email":
			fmt.Fprint(w, `{"data":{"onboarding_session_token":"devsess_123"}}`)
		case "/v1/onboarding/state":
			fmt.Fprint(w, `{"data":{"can_issue_api_key":false,"next_step":{"code":"complete_verification_step","owner":"human","machine_completable":false,"launch":{"endpoint":"/v1/merchant-account-sessions","component":"account_onboarding"}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	exit := app.Run([]string{
		"signup", "--email", "dev@example.com", "--first-name", "Ada", "--last-name", "Lovelace",
		"--verification-code", "482193", "--no-input", "--output", "json",
	})
	if exit != ExitAuth || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"code":"SIGNUP_HUMAN_ACTION_REQUIRED"`) || !strings.Contains(stdout.String(), `"complete_verification_step"`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

func TestSignupRevokesIssuedKeyWhenKeychainWriteFails(t *testing.T) {
	revoked := false
	server := signupCompensationTestServer(t, &revoked)
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	credentialDeleted := false
	app.StoreCredential = func(string, string) error { return errors.New("keychain locked") }
	app.DeleteCredential = func(string) error {
		credentialDeleted = true
		return nil
	}
	exit := app.Run([]string{
		"signup", "--email", "dev@example.com", "--first-name", "Ada", "--last-name", "Lovelace",
		"--verification-code", "482193", "--no-input", "--output", "json",
	})
	if exit != ExitAuth || stderr.Len() != 0 || !revoked || !credentialDeleted || !strings.Contains(stdout.String(), `"code":"KEYCHAIN_WRITE_FAILED"`) {
		t.Fatalf("exit=%d revoked=%t credential_deleted=%t stdout=%s stderr=%s", exit, revoked, credentialDeleted, stdout, stderr)
	}
}

func TestSignupRevokesIssuedKeyAndRestoresCredentialWhenConfigWriteFails(t *testing.T) {
	revoked := false
	server := signupCompensationTestServer(t, &revoked)
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	app.LoadCredential = func(string) (string, error) { return "flint_test_previous", nil }
	var stored []string
	app.StoreCredential = func(_ string, secret string) error {
		stored = append(stored, secret)
		if len(stored) == 1 {
			path, err := app.configPath()
			if err != nil {
				return err
			}
			return os.Mkdir(path, 0700)
		}
		return nil
	}
	exit := app.Run([]string{
		"signup", "--email", "dev@example.com", "--first-name", "Ada", "--last-name", "Lovelace",
		"--verification-code", "482193", "--no-input", "--output", "json",
	})
	if exit != ExitAuth || stderr.Len() != 0 || !revoked || len(stored) != 2 || stored[1] != "flint_test_previous" || !strings.Contains(stdout.String(), `"code":"CONFIG_WRITE_FAILED"`) {
		t.Fatalf("exit=%d revoked=%t stored=%v stdout=%s stderr=%s", exit, revoked, stored, stdout, stderr)
	}
}

func signupCompensationTestServer(t *testing.T, revoked *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/onboarding/start":
			fmt.Fprint(w, `{"data":{"verification_token":"devver_123"}}`)
		case "/v1/onboarding/verify-email":
			fmt.Fprint(w, `{"data":{"onboarding_session_token":"devsess_123"}}`)
		case "/v1/onboarding/state":
			fmt.Fprint(w, `{"data":{"can_issue_api_key":true,"default_sandbox_id":"test_123"}}`)
		case "/v1/onboarding/api-key":
			fmt.Fprint(w, `{"data":{"api_key_id":"key_123","merchant_id":"mer_123","sandbox_id":"test_123","secret_key":"flint_test_new"}}`)
		case "/v1/api-keys/key_123/revoke":
			if r.Header.Get("Authorization") != "Bearer flint_test_new" || r.Header.Get("Idempotency-Key") == "" {
				t.Errorf("invalid compensation request headers: %#v", r.Header)
			}
			*revoked = true
			fmt.Fprint(w, `{"data":{"api_key_id":"key_123","status":"revoked"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

type delayedReader struct {
	delay  time.Duration
	reader *strings.Reader
	done   bool
}

func (r *delayedReader) Read(p []byte) (int, error) {
	if !r.done {
		time.Sleep(r.delay)
		r.done = true
	}
	return r.reader.Read(p)
}

func TestMCPToolErrorsDoNotClaimToMatchSuccessOutputSchema(t *testing.T) {
	result := mcpToolError("failed", map[string]any{"error": "details"})
	if _, ok := result["structuredContent"]; ok {
		t.Fatalf("tool error unexpectedly contains structuredContent: %#v", result)
	}
	if result["isError"] != true {
		t.Fatalf("tool error = %#v", result)
	}
}

func TestMCPRejectsMalformedToolCallsAsProtocolErrors(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, req := range []jsonRPCRequest{
		{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{}},
		{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{"name": "missing"}},
		{JSONRPC: "2.0", ID: 3, Method: "tools/call", Params: map[string]any{"name": "version", "arguments": "invalid"}},
	} {
		response := app.handleMCP(req, defaultOptions())
		if code, _ := lookupPath(response, "error.code"); code != -32602 {
			t.Fatalf("response = %#v", response)
		}
	}
}

func TestMCPOlderProtocolOmitsNewStructuredOutputFields(t *testing.T) {
	app, _, _ := testApp(t, "")
	listed := app.handleMCPVersion(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}, defaultOptions(), "2025-03-26")
	tools, ok := listed["result"].(map[string]any)["tools"].([]map[string]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools/list response = %#v", listed)
	}
	if _, exists := tools[0]["outputSchema"]; exists {
		t.Fatalf("2025-03-26 tool unexpectedly includes outputSchema: %#v", tools[0])
	}
	called := app.handleMCPVersion(jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{"name": "version", "arguments": map[string]any{}}}, defaultOptions(), "2025-03-26")
	if _, exists := called["result"].(map[string]any)["structuredContent"]; exists {
		t.Fatalf("2025-03-26 result unexpectedly includes structuredContent: %#v", called)
	}
}

func TestCustomerAPIRouting(t *testing.T) {
	t.Setenv("FLINT_BASE_URL", "")
	app := &App{}
	for _, key := range []string{"flint_test_fixture", "flint_live_fixture"} {
		got, err := app.baseURLForCredential(key)
		if err != nil || got != "https://api.withflintpay.com" {
			t.Fatalf("default endpoint = %q, %v", got, err)
		}
	}
	t.Setenv("FLINT_BASE_URL", "https://api.example.com")
	if got, err := app.baseURLForCredential("flint_test_fixture"); err != nil || got != "https://api.example.com" {
		t.Fatalf("explicit endpoint = %q, %v", got, err)
	}
}

func TestSignupDefaultsToCustomerAPI(t *testing.T) {
	app, _, _ := testApp(t, "")
	called := false
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if req.URL.Scheme != "https" || req.URL.Host != "api.withflintpay.com" || req.URL.Path != "/v1/onboarding/start" {
			t.Errorf("signup destination = %s", req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"TEST_STOP","message":"synthetic response"}}`)),
		}, nil
	})}
	if exit := app.Run([]string{"signup", "--email", "dev@example.com", "--first-name", "Ada", "--last-name", "Lovelace", "--no-input", "--output", "json"}); exit != ExitAPI || !called {
		t.Fatalf("signup exit = %d, transport called = %t", exit, called)
	}
}
