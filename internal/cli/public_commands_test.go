package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryCommandNamespaceHasHelp(t *testing.T) {
	registry := NewRegistry()
	seen := map[string]bool{}
	for _, command := range registry.Commands {
		for size := 2; size < len(command.Path); size++ {
			prefix := command.Path[1:size]
			name := strings.Join(prefix, " ")
			if seen[name] {
				continue
			}
			seen[name] = true
			_, _, help, err := parseInvocation(registry, append(append([]string{}, prefix...), "--help"))
			if err != nil || !help {
				t.Errorf("%s --help: help=%v error=%v", name, help, err)
			}
		}
	}
}

func TestNamespaceHelpListsCommandsWithoutAuthentication(t *testing.T) {
	app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
	app.LoadCredential = func(string) (string, error) {
		t.Fatal("namespace help must not read credentials")
		return "", nil
	}
	if code := app.Run([]string{"sandboxes", "--help"}); code != ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	for _, text := range []string{"flint sandboxes <command>", "get", "delete", "create", "list"} {
		if !strings.Contains(stdout.String(), text) {
			t.Errorf("help omitted %q: %s", text, stdout)
		}
	}
	if _, _, _, err := parseInvocation(app.Registry, []string{"developer", "not-a-command", "--help"}); err == nil {
		t.Fatal("unknown namespaces must still fail")
	}
}

func TestGeneratedCommandsUseExistingResourceGroups(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"sandboxes.get", "sandboxes.delete"} {
		if _, ok := r.ByName(name); !ok {
			t.Errorf("missing command %s", name)
		}
	}
}

func TestEveryPublicOperationHasCompleteCommandContract(t *testing.T) {
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	byOperation := map[string]*Command{}
	for _, command := range registry.Commands {
		if command.OperationID != "" {
			byOperation[command.OperationID] = command
		}
	}
	for path, methods := range doc.Paths {
		if path == "/v1/feedback-reports" || strings.HasPrefix(path, "/v1/feedback-reports/") {
			for _, operation := range methods {
				if command := byOperation[operation.OperationID]; command != nil {
					t.Errorf("removed feedback operation still exposes %s", command.Name)
				}
			}
			continue
		}
		for method, operation := range methods {
			if operation.OperationID == "" {
				continue
			}
			command := byOperation[operation.OperationID]
			if command == nil {
				t.Errorf("%s %s has no command", method, path)
				continue
			}
			if command.AuthRequired != (len(operation.Security) > 0) {
				t.Errorf("%s authentication differs from OpenAPI", command.Name)
			}
			if command.OutputSchema != responseSchemaName(operation.Responses) {
				t.Errorf("%s output schema differs from OpenAPI", command.Name)
			}
			if command.InputSchema != requestSchemaName(operation.RequestBody) {
				t.Errorf("%s input schema differs from OpenAPI", command.Name)
			}
			for _, parameter := range operation.Parameters {
				location, _ := parameter["in"].(string)
				name, _ := parameter["name"].(string)
				if location != "query" || name == "expand" {
					continue
				}
				found := false
				for _, argument := range command.Arguments {
					if argument.Query == name {
						found = true
					}
				}
				if !found {
					t.Errorf("%s omits query parameter %s", command.Name, name)
				}
			}
		}
	}
}

func TestPublicAndSessionCommandsUseTheirOwnAuthentication(t *testing.T) {
	for _, scenario := range []struct {
		name              string
		args              []string
		token             string
		wantAuthorization string
	}{
		{"public", []string{"openapi", "get"}, "", ""},
		{"customer", []string{"me", "get"}, "flint_cses_test", "Bearer flint_cses_test"},
		{"onboarding", []string{"onboarding", "state", "get"}, "onboarding_test", "Bearer onboarding_test"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/v1/developer/auth-context" {
					t.Error("session/public request attempted API-key introspection")
				}
				if got := r.Header.Get("Authorization"); got != scenario.wantAuthorization {
					t.Errorf("unexpected authorization header")
				}
				fmt.Fprint(w, `{"data":{"id":"test"}}`)
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_ACCESS_TOKEN", scenario.token)
			t.Setenv("FLINT_API_KEY", "")
			app.LoadCredential = func(string) (string, error) { t.Error("unexpected keychain lookup"); return "", nil }
			if exit := app.Run(append(scenario.args, "--output", "json")); exit != 0 {
				t.Fatalf("exit %d: %s", exit, out)
			}
			if calls != 1 {
				t.Errorf("calls=%d", calls)
			}
		})
	}
}

func TestPDFDownloadSavesExactBytesAndNeverOverwrites(t *testing.T) {
	pdf := []byte("%PDF-1.7\n\x00\xff test file\n%%EOF")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.Header.Get("Accept") != "application/pdf" {
			t.Error("PDF accept header missing")
		}
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdf)
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "invoice.pdf")
	app, out, _ := testApp(t, server.URL)
	args := []string{"invoices", "pdf", "get", "inv_test", "--save-to", destination, "--output", "json"}
	if exit := app.Run(args); exit != 0 {
		t.Fatalf("exit %d: %s", exit, out)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != string(pdf) {
		t.Fatalf("download differs: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "content_base64") {
		t.Error("saved download duplicated file in output")
	}
	out.Reset()
	if exit := app.Run(args); exit == 0 {
		t.Fatal("overwrote an existing file")
	}
}

func TestRequiredQueryCannotBeSuppliedByInputFile(t *testing.T) {
	command := apiCommand("example.get", "Get example", "POST", "/v1/example", "", "", "", []Arg{flag("token", "string", "", "token", "Token", true)}, true, false, false, false, "flint example get --token value")
	registry := &Registry{byName: map[string]*Command{}, byPath: map[string]*Command{}}
	registry.add(command)
	_, _, _, err := parseInvocation(registry, []string{"example", "get", "--input", "-"})
	if err == nil || err.Code != "MISSING_REQUIRED_ARGUMENT" {
		t.Fatalf("required query accepted input file: %v", err)
	}
}

func TestReportRedirectDownloadsWithoutForwardingCredentials(t *testing.T) {
	storageCalls := 0
	storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageCalls++
		for _, header := range []string{"Authorization", "X-API-Key", "Idempotency-Key", "Flint-Version"} {
			if r.Header.Get(header) != "" {
				t.Errorf("forwarded %s to file storage", header)
			}
		}
		w.Header().Set("Content-Type", "text/csv")
		fmt.Fprint(w, "id,amount\nord_test,100\n")
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		w.Header().Set("Location", storage.URL+"/report.csv?signature=test")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer api.Close()
	app, out, _ := testApp(t, api.URL)
	app.HTTPClient = storage.Client()
	if exit := app.Run([]string{"report-downloads", "get", "rdl_test", "--output", "json"}); exit != 0 {
		t.Fatalf("exit=%d: %s", exit, out)
	}
	if storageCalls != 1 || !strings.Contains(out.String(), "content_base64") {
		t.Fatalf("download calls=%d: %s", storageCalls, out)
	}
}

func TestOperationSpecificSchemasPreserveAPIAlternatives(t *testing.T) {
	invoice, _ := NewRegistry().ByName("invoices.create")
	for _, test := range []struct {
		body  map[string]any
		valid bool
	}{
		{map[string]any{}, false},
		{map[string]any{"order_id": "ord_test"}, true},
		{map[string]any{"order_id": "ord_test", "quick_pay": map[string]any{}}, false},
	} {
		if err := validateMCPArguments(invoice, test.body); (err == nil) != test.valid {
			t.Errorf("invoice schema validity=%t: %v", test.valid, err)
		}
	}
	retry, _ := NewRegistry().ByName("subscriptions.payment-retries.create")
	if err := validateMCPArguments(retry, map[string]any{"subscription_id": "sub_test", "_flint": map[string]any{"dry_run": "client"}}); err != nil {
		t.Fatalf("empty request body rejected CLI arguments: %v", err)
	}
	if err := validateMCPArguments(retry, map[string]any{"subscription_id": "sub_test", "unexpected": true}); err == nil {
		t.Fatal("empty request schema admitted unknown body field")
	}
}

func TestSessionHistoryNeverMatchesAnotherCredential(t *testing.T) {
	entry := HistoryEntry{Profile: "default", Environment: "sandbox", CredentialScope: "session-a"}
	for _, credential := range []string{"", "session-b"} {
		if historyEntryMatchesScope(entry, historyScope{Profile: "default", Environment: "sandbox", CredentialScope: credential}) {
			t.Fatalf("session history matched credential %q", credential)
		}
	}
	if !historyEntryMatchesScope(entry, historyScope{Profile: "default", Environment: "sandbox", CredentialScope: "session-a"}) {
		t.Fatal("session cannot resolve its own history")
	}
}

func TestAccessTokenCannotBypassAPIKeyIntrospection(t *testing.T) {
	app, out, _ := testApp(t, "")
	t.Setenv("FLINT_ACCESS_TOKEN", "flint_live_test")
	if exit := app.Run([]string{"customers", "list", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), "API_KEY_IN_ACCESS_TOKEN") {
		t.Fatalf("exit=%d: %s", exit, out)
	}
}

func TestCheckoutSessionHeadersSkipAPIKeyAuthentication(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/checkout-sessions/cs_test" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Checkout-Session-ID") != "cs_test" || r.Header.Get("X-Checkout-Session-Secret") != "secret_test" {
			t.Error("incorrect checkout authentication")
		}
		fmt.Fprint(w, `{"data":{"checkout_session_id":"cs_test"}}`)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	t.Setenv("FLINT_CHECKOUT_SESSION_ID", "cs_test")
	t.Setenv("FLINT_CHECKOUT_SESSION_SECRET", "secret_test")
	t.Setenv("FLINT_API_KEY", "")
	if err := app.updateConfig(func(cfg *Config) error {
		cfg.Profiles["default"] = Profile{ContextID: "ctx_saved"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if exit := app.Run([]string{"checkout", "get", "cs_test", "--output", "json"}); exit != 0 {
		t.Fatalf("exit=%d: %s", exit, out)
	}
	if calls != 1 {
		t.Errorf("made %d requests", calls)
	}
	out.Reset()
	if exit := app.Run([]string{"checkout", "get", "cs_test", "--context", "ctx_explicit", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), "CONTEXT_SESSION_REQUIRED") {
		t.Fatalf("explicit context accepted: exit=%d: %s", exit, out)
	}
}

func TestOAuthTokenErrorPreservesProtocolDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/oauth/token" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		w.Header().Set("X-Request-Id", "req_oauth")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_client","error_description":"Client authentication failed."}`)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	app.Stdin = strings.NewReader(`{}`)
	if exit := app.Run([]string{"oauth", "token", "--input", "-", "--output", "json"}); exit != ExitAuth {
		t.Fatalf("exit=%d: %s", exit, out)
	}
	for _, value := range []string{"invalid_client", "Client authentication failed.", "req_oauth"} {
		if !strings.Contains(out.String(), value) {
			t.Errorf("missing %s: %s", value, out)
		}
	}
}

func TestOpenAPISpecDoesNotRecordExampleIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"openapi":"3.1.2","example":{"product_id":"prod_example"}}`)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	if exit := app.Run([]string{"openapi", "get", "--output", "json"}); exit != 0 {
		t.Fatalf("exit=%d: %s", exit, out)
	}
	history, err := app.loadHistory()
	if err != nil || len(history.Entries) != 0 {
		t.Fatalf("contract examples entered resource history: %v", err)
	}
}

func TestHistoryDoesNotPersistCredentialShapedStrings(t *testing.T) {
	ids := map[string]bool{}
	collectIDs(map[string]any{"data": map[string]any{"payment_intent_id": "pi_resource", "client_secret": "pi_resource_secret_private", "description": "pi_not_a_resource", "metadata": map[string]any{"id": "pi_private_metadata"}}}, ids)
	if len(ids) != 1 || !ids["pi_resource"] {
		t.Fatalf("unexpected history IDs: %v", ids)
	}
}

func TestRawOperationMatchingPrefersLiteralSubresources(t *testing.T) {
	for i := 0; i < 100; i++ {
		operation, ok := matchPublicOperation("GET", "/v1/checkout-sessions/cs_test/delivery-selections/current")
		if !ok || operation.OperationID != "getCheckoutSessionCurrentDeliverySelection" {
			t.Fatalf("matched %s", operation.OperationID)
		}
	}
	operation, ok := matchPublicOperation("GET", "/v1/checkout-sessions/cs_test/delivery-selections/dsel_test")
	if !ok || operation.OperationID != "getCheckoutSessionDeliverySelectionHistory" {
		t.Fatalf("matched history as %s", operation.OperationID)
	}
}
