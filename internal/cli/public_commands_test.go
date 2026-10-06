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

func TestPublicVerificationArgumentsUseOperationIDPrefixes(t *testing.T) {
	for _, scenario := range []struct {
		operationID string
		prefix      string
	}{
		{"confirmCustomerVerification", "cver_"},
		{"confirmCheckoutSessionCustomerVerification", "cscv_"},
	} {
		t.Run(scenario.operationID, func(t *testing.T) {
			operation, ok := openAPIOperationByID(scenario.operationID)
			if !ok {
				t.Fatal("verification operation missing")
			}
			for _, argument := range publicAPIArguments("", operation) {
				if argument.Name == "customer_verification_id" {
					if argument.IDPrefix != scenario.prefix || !argument.AcceptsHistoryRef {
						t.Fatalf("verification argument = %#v", argument)
					}
					return
				}
			}
			t.Fatal("verification ID argument missing")
		})
	}
}

func TestVerificationCreateResponsesRecordHistoryAndResolveReferences(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		createOperation  string
		confirmOperation string
		id               string
		resourceType     string
		createBody       string
		response         string
		checkout         bool
	}{
		{
			name: "customer", createOperation: "createCustomerVerification", confirmOperation: "confirmCustomerVerification",
			id: "cver_created", resourceType: "customer_verification",
			createBody: `{"customer_id":"cus_test","email":"buyer@example.com","purpose":"link_guest_purchases"}`,
			response:   `{"data":{"customer_verification_id":"cver_created","customer_id":"cus_test","channel":"email","email":"buyer@example.com","purpose":"link_guest_purchases","status":"pending","created_at":"2026-03-17T14:30:00Z","expires_at":"2026-03-17T14:45:00Z"}}`,
		},
		{
			name: "checkout", createOperation: "createCheckoutSessionCustomerVerification", confirmOperation: "confirmCheckoutSessionCustomerVerification",
			id: "cscv_created", resourceType: "checkout_session_customer_verification", checkout: true,
			createBody: `{"email":"buyer@example.com","purpose":"save_payment_method"}`,
			response:   `{"data":{"customer_verification_id":"cscv_created","checkout_session_id":"cs_test","channel":"email","email":"buyer@example.com","purpose":"save_payment_method","created_at":"2026-03-17T14:30:00Z","expires_at":"2026-03-17T14:45:00Z"}}`,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			registry := NewRegistry()
			var create, confirm *Command
			for _, command := range registry.Commands {
				if command.OperationID == scenario.createOperation {
					create = command
				}
				if command.OperationID == scenario.confirmOperation {
					confirm = command
				}
			}
			if create == nil || confirm == nil {
				t.Fatal("verification commands missing")
			}
			createPath := strings.ReplaceAll(create.APIPath, "{checkout_session_id}", "cs_test")
			confirmPath := strings.ReplaceAll(confirm.APIPath, "{checkout_session_id}", "cs_test")
			confirmPath = strings.ReplaceAll(confirmPath, "{customer_verification_id}", scenario.id)
			confirmCalls, linkCalls := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/developer/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				if r.Method != http.MethodPost {
					t.Errorf("method = %s", r.Method)
				}
				switch r.URL.Path {
				case createPath:
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, scenario.response)
				case confirmPath:
					confirmCalls++
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["code"] != "123456" {
						t.Errorf("confirm body = %#v, error = %v", body, err)
					}
					fmt.Fprint(w, `{"data":{}}`)
				case "/v1/customers/cus_test/link-guest-purchases":
					linkCalls++
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["customer_verification_id"] != scenario.id {
						t.Errorf("link body = %#v, error = %v", body, err)
					}
					fmt.Fprint(w, `{"data":{}}`)
				default:
					t.Errorf("unexpected verification path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			app, stdout, stderr := testApp(t, server.URL)
			if scenario.checkout {
				t.Setenv("FLINT_API_KEY", "")
				t.Setenv("FLINT_CHECKOUT_SESSION_ID", "cs_test")
				t.Setenv("FLINT_CHECKOUT_SESSION_SECRET", "secret_test")
			}
			run := func(command *Command, args []string, body string) {
				t.Helper()
				argv := append([]string(nil), command.Path[1:]...)
				argv = append(argv, args...)
				if body != "" {
					argv = append(argv, "--input", "-")
				}
				argv = append(argv, "--output", "json")
				app.Stdin = strings.NewReader(body)
				stdout.Reset()
				stderr.Reset()
				if exit := app.Run(argv); exit != ExitOK {
					t.Fatalf("argv = %v, exit = %d, stdout = %s, stderr = %s", argv, exit, stdout, stderr)
				}
			}
			var positionals []string
			if scenario.checkout {
				positionals = []string{"cs_test"}
			}
			run(create, positionals, scenario.createBody)
			history, err := app.loadHistory()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range history.Entries {
				if entry.ID == scenario.id {
					found = true
					if entry.ResourceType != scenario.resourceType || entry.Command != create.CanonicalName {
						t.Fatalf("verification history entry = %#v", entry)
					}
				}
			}
			if !found {
				t.Fatalf("create response verification ID missing from history: %#v", history)
			}
			for _, ref := range []string{"@last", "@last." + strings.SplitN(scenario.id, "_", 2)[0]} {
				args := append(append([]string(nil), positionals...), ref)
				run(confirm, args, `{"code":"123456"}`)
				if !scenario.checkout {
					link, _ := registry.ByName("customers.link-guest-purchases")
					run(link, []string{"cus_test", "--customer-verification", ref}, "")
				}
			}
			if confirmCalls != 2 || (!scenario.checkout && linkCalls != 2) {
				t.Fatalf("confirm calls = %d, link calls = %d", confirmCalls, linkCalls)
			}
		})
	}
}

func TestNoArgumentsShowsRootHelpWithoutAuthentication(t *testing.T) {
	app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
	app.LoadCredential = func(string) (string, error) {
		t.Fatal("root help must not read credentials")
		return "", nil
	}
	if code := app.Run(nil); code != ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr)
	}
	output := stdout.String()
	for _, text := range []string{"Usage:", "Start here:", "flint auth login", "flint <command> --help"} {
		if !strings.Contains(output, text) {
			t.Errorf("help omitted %q: %s", text, output)
		}
	}
	for _, args := range [][]string{{"--help"}, {"help"}} {
		stdout.Reset()
		if code := app.Run(args); code != ExitOK || stdout.String() != output || stderr.Len() != 0 {
			t.Errorf("%v differs from bare flint: exit=%d stdout=%s stderr=%s", args, code, stdout, stderr)
		}
	}
}

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

func TestCustomerFulfillmentEventsRequireOrderBeforeRequest(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		flags     []string
		withOrder bool
	}{
		{name: "missing order"},
		{name: "other filter", flags: []string{"--fulfillment-id", "ful_test"}},
		{name: "order scoped", flags: []string{"--order-id", "ord_test", "--fulfillment-id", "ful_test"}, withOrder: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/v1/me/fulfillment-events" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer flint_cses_test" {
					t.Error("customer bearer authentication missing")
				}
				if got := r.URL.Query().Get("order_id"); got != "ord_test" {
					t.Errorf("order_id=%q", got)
				}
				if got := r.URL.Query().Get("fulfillment_id"); got != "ful_test" {
					t.Errorf("fulfillment_id=%q", got)
				}
				fmt.Fprint(w, `{"data":[],"has_more":false}`)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_ACCESS_TOKEN", "")
			app.LoadCredential = func(string) (string, error) {
				t.Error("unexpected keychain lookup")
				return "", nil
			}
			if scenario.withOrder {
				t.Setenv("FLINT_ACCESS_TOKEN", "flint_cses_test")
			}
			args := append([]string{"me", "fulfillment-events", "list"}, scenario.flags...)
			exit := app.Run(append(args, "--output", "json"))
			if scenario.withOrder {
				if exit != ExitOK || calls != 1 || stderr.Len() != 0 {
					t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, calls, out, stderr)
				}
				return
			}
			var result struct {
				Error struct {
					Code  string `json:"code"`
					Param string `json:"param"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if exit != ExitUsage || calls != 0 || result.Error.Code != "MISSING_REQUIRED_ARGUMENT" || result.Error.Param != "order_id" {
				t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, calls, out, stderr)
			}
		})
	}
}

func TestFulfillmentEventInputSchemasPreserveCustomerOrderScope(t *testing.T) {
	for _, scenario := range []struct {
		command   string
		arguments map[string]any
		valid     bool
	}{
		{"me.fulfillment-events.list", map[string]any{}, false},
		{"me.fulfillment-events.list", map[string]any{"fulfillment_id": "ful_test"}, false},
		{"me.fulfillment-events.list", map[string]any{"order_id": "ord_test"}, true},
		{"fulfillment-events.list", map[string]any{}, true},
	} {
		command, ok := NewRegistry().ByName(scenario.command)
		if !ok {
			t.Fatalf("command %s missing", scenario.command)
		}
		if err := validateMCPArguments(command, scenario.arguments); (err == nil) != scenario.valid {
			t.Errorf("%s arguments=%v valid=%t: %v", scenario.command, scenario.arguments, scenario.valid, err)
		}
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
