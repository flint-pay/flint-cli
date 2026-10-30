package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPErrorsRawAPIObjectIsImmutable(t *testing.T) {
	raw := map[string]any{
		"type": "unavailable_error", "code": "SERVICE_UNAVAILABLE", "message": "Wait <a&b>.", "error_source": "future_service",
		"details":     []any{map[string]any{"reason": "fixture", "facts": map[string]any{"values": []any{"original"}}}},
		"remediation": map[string]any{"retry": "same_request", "next_steps": "Keep --confirm unchanged."},
		"extension":   map[string]any{"nested": []any{"original"}},
	}
	want := cloneErrorJSON(raw)
	e := apiErrorFromResponse(&apiResponse{Status: 503, Headers: http.Header{"X-Request-Id": []string{"header-only"}}, Value: map[string]any{"error": raw}})
	withIdempotencyRetryContext(e, "immutable-key")
	// Exercise the terminal writer too: its status, key and retry diagnostics must
	// never enter the retained API object.
	var terminal bytes.Buffer
	(&App{Stdout: &terminal}).writeError(e, Options{Output: "json"})
	raw["details"].([]any)[0].(map[string]any)["facts"].(map[string]any)["values"].([]any)[0] = "changed"
	raw["extension"].(map[string]any)["nested"].([]any)[0] = "changed"
	cmd, _ := NewRegistry().ByName("customers.create")
	result := (&App{}).mcpCommandError(cmd, nil, Options{}, mcpCallResult{cliError: e, exit: e.ExitCode, environment: "sandbox"}, mcpPolicy{}, "A local override.")
	envelope := mcpErrorJSON(t, result)
	if !reflect.DeepEqual(envelope["error"], want) {
		t.Fatalf("raw API object changed: %#v", envelope)
	}
	if lookupPathValue(envelope, "cli.origin") != "api" || lookupPathValue(envelope, "cli.operation") != "customers.create" || lookupPathValue(envelope, "cli.environment") != "sandbox" || lookupPathValue(envelope, "cli.idempotency_key") != "immutable-key" {
		t.Fatalf("metadata=%#v", envelope["cli"])
	}
	text := mcpResultText(result)
	if strings.Contains(text, `\u003c`) || strings.Contains(text, "\n") || result["structuredContent"] != nil {
		t.Fatalf("noncompact result: %#v", result)
	}
	// Returned maps are independent too, so callers cannot corrupt a later result.
	e.apiError["extension"].(map[string]any)["nested"].([]any)[0] = "later"
	if !reflect.DeepEqual(envelope["error"], want) {
		t.Fatal("result shares API maps")
	}
}

func TestMCPErrorsCaptureOriginalChildError(t *testing.T) {
	const response = `{"error":{"type":"validation_error","code":"INVALID_REQUEST","message":"Two fields are invalid.","error_source":"merchant_endpoint","details":[{"code":"REQUIRED","param":"email"},{"code":"INVALID_VALUE","param":"name"}],"remediation":{"retry":"none","next_steps":"Correct the fields."},"future_fact":{"keep":[1,2]}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		w.Header().Set("X-Request-Id", "req_header")
		w.WriteHeader(400)
		fmt.Fprint(w, response)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	cmd, _ := app.Registry.ByName("customers.create")
	args := map[string]any{"name": "Fixture", "_flint": map[string]any{"idempotency_key": "capture-key"}}
	outcome := app.runMCPCommand(context.Background(), cmd, args, defaultOptions(), mcpPolicy{}, nil)
	if outcome.cliError == nil || outcome.cliError.apiError == nil {
		t.Fatalf("error was reconstructed: %#v", outcome)
	}
	result := app.mcpCommandError(cmd, args, defaultOptions(), outcome, mcpPolicy{}, "")
	got := mcpErrorJSON(t, result)
	var want map[string]any
	if err := json.Unmarshal([]byte(response), &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got["error"])
	wantJSON, _ := json.Marshal(want["error"])
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("API error changed: %s != %s", gotJSON, wantJSON)
	}
	if lookupPathValue(got, "cli.idempotency_key") != "capture-key" || lookupPathValue(got, "cli.origin") != "api" {
		t.Fatal(got)
	}
}

func TestMCPErrorsWriteOutcomeRequiresTargetDispatch(t *testing.T) {
	for _, scenario := range []string{"authentication timeout", "authentication unavailable", "target unavailable", "target dial failure"} {
		t.Run(scenario, func(t *testing.T) {
			var reads, writes, dialAttempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/auth-context" {
					reads.Add(1)
					if scenario == "authentication timeout" {
						<-r.Context().Done()
						return
					}
					if strings.HasPrefix(scenario, "target ") {
						fmt.Fprint(w, authContextJSON("sandbox"))
						return
					}
				} else if r.Method == http.MethodPost && r.URL.Path == "/v1/refunds" {
					writes.Add(1)
				} else {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"error":{"type":"unavailable_error","code":"SERVICE_UNAVAILABLE","message":"Flint is unavailable.","error_source":"flint","details":[],"remediation":{"retry":"same_request","next_steps":"Wait for Flint to recover."}}}`)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			if scenario == "target dial failure" {
				app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodPost && r.URL.Path == "/v1/refunds" {
						dialAttempts.Add(1)
						return nil, &net.OpError{Op: "dial", Err: errors.New("test connection refused before sending")}
					}
					return http.DefaultTransport.RoundTrip(r)
				})}
			}
			cmd, _ := app.Registry.ByName("refunds.create")
			args := map[string]any{"payment_intent_id": "pi_synthetic", "refund_type": "full", "_flint": map[string]any{"idempotency_key": "dispatch-key", "confirm": true}}
			opts := mcpServeOptions(t, "--timeout", "40ms")
			outcome := app.runMCPCommand(context.Background(), cmd, args, opts, mcpPolicy{allowDestructive: true}, nil)
			if outcome.cliError == nil || reads.Load() == 0 {
				t.Fatalf("missing failure or auth request: %#v", outcome)
			}
			envelope := mcpErrorJSON(t, app.mcpCommandError(cmd, args, opts, outcome, mcpPolicy{allowDestructive: true}, ""))
			if scenario == "target unavailable" {
				if writes.Load() == 0 || !outcome.cliError.requestSent || lookupPathValue(envelope, "cli.outcome") != "unknown" || lookupPathValue(envelope, "cli.idempotency_key") != "dispatch-key" {
					t.Fatalf("target write lost dispatch facts: writes=%d result=%#v", writes.Load(), envelope)
				}
				return
			}
			if writes.Load() != 0 || outcome.cliError.requestSent || lookupPathValue(envelope, "cli.outcome") != nil || lookupPathValue(envelope, "cli.idempotency_key") != nil {
				t.Fatalf("auth failure acquired write facts: writes=%d result=%#v", writes.Load(), envelope)
			}
			if scenario == "target dial failure" && dialAttempts.Load() == 0 {
				t.Fatal("target dial failure did not reach the transport")
			}
			actions, _ := json.Marshal(lookupPathValue(envelope, "cli.next_actions"))
			if strings.Contains(string(actions), "refunds.list") || strings.Contains(string(actions), "flint_write") || strings.Contains(string(actions), "refunds_create") || strings.Contains(string(actions), "idempotency_key") || strings.Contains(fmt.Sprint(lookupPathValue(envelope, "error.remediation.next_steps")), "idempotency_key") {
				t.Fatalf("auth failure acquired write recovery advice: %#v", envelope)
			}
		})
	}
}

func TestMCPErrorsObserverExcludesPreparationMutations(t *testing.T) {
	cmd, _ := NewRegistry().ByName("refunds.create")
	for _, local := range []bool{false, true} {
		observer := &mcpEnvironmentObserver{targetOperationID: cmd.OperationID, localMutations: local, base: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}}, nil
		})}
		request, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/oauth/token", nil)
		_, _ = observer.RoundTrip(request)
		if _, _, observed := observer.state(); observed {
			t.Fatal("credential exchange counted as target dispatch")
		}
		if !local {
			request, _ = http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/customers", nil)
			_, _ = observer.RoundTrip(request)
			if _, _, observed := observer.state(); observed {
				t.Fatal("unrelated mutation counted as target dispatch")
			}
		}
		request, _ = http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/refunds", nil)
		_, _ = observer.RoundTrip(request)
		if _, _, observed := observer.state(); !observed {
			t.Fatal("target mutation was not observed")
		}
	}
}

func TestMCPErrorsUnknownOutcomeRetainsAPIObject(t *testing.T) {
	for _, tc := range []struct {
		code, retry string
		status      int
	}{{"IDEMPOTENCY_KEY_IN_PROGRESS", "same_request", 409}, {"SERVICE_UNAVAILABLE", "none", 503}} {
		t.Run(tc.code, func(t *testing.T) {
			raw := map[string]any{"type": "unavailable_error", "code": tc.code, "message": "The write has no definite result.", "error_source": "flint", "details": []any{map[string]any{"reason": "outcome_unknown"}}, "remediation": map[string]any{"retry": tc.retry, "next_steps": "Keep the API remedy."}}
			e := apiErrorFromResponse(&apiResponse{Status: tc.status, Headers: http.Header{}, Value: map[string]any{"error": raw}})
			history := requestHistory{sent: true, started: time.Now(), attempts: []*requestAttempt{{HTTPStatus: tc.status, ErrorCode: tc.code}}, uncertain: e}
			resultError := history.finalError(context.Background(), e, "POST", "/v1/refunds", "unknown-key")
			resultError.requestSent = true
			withIdempotencyRetryContext(resultError, "unknown-key")
			cmd, _ := NewRegistry().ByName("refunds.create")
			result := (&App{}).mcpCommandError(cmd, nil, Options{}, mcpCallResult{cliError: resultError, exit: resultError.ExitCode}, mcpPolicy{}, "")
			envelope := mcpErrorJSON(t, result)
			if !reflect.DeepEqual(envelope["error"], raw) {
				t.Fatalf("raw API error lost: %#v", envelope)
			}
			if lookupPathValue(envelope, "cli.outcome") != "unknown" || lookupPathValue(envelope, "cli.idempotency_key") != "unknown-key" || lookupPathValue(envelope, "cli.origin") != "api" {
				t.Fatalf("missing recovery metadata: %#v", envelope)
			}
		})
	}
}

func TestMCPErrorsClassNoneWriteReportsUnknownOutcome(t *testing.T) {
	const apiError = `{"error":{"type":"unavailable_error","code":"SERVICE_UNAVAILABLE","message":"Flint is unavailable.","error_source":"flint","details":[],"remediation":{"retry":"none","next_steps":"Inspect the affected resources."}}}`
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/delivery-previews" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("class-none write sent an idempotency key")
		}
		writes.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, apiError)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	cmd, _ := app.Registry.ByName("delivery-previews.create")
	if commandIdempotencyClass(cmd) != "none" {
		t.Fatal("fixture requires a class-none operation")
	}
	args := map[string]any{
		"mode": "delivery_options", "currency": "USD",
		"delivery_method_ids": []any{"dmet_01K1P6G4M7H2N8Q9R3S5T6V7WX"},
		"destination_address": map[string]any{"country": "US", "postal_code": "11249"},
		"line_items":          []any{map[string]any{"type": "variant", "variant_id": "var_01K1P6G4M7H2N8Q9R3S5T6V7WX", "quantity": 2}},
	}
	outcome := app.runMCPCommand(context.Background(), cmd, args, defaultOptions(), mcpPolicy{}, nil)
	if writes.Load() != 1 || outcome.cliError == nil || !outcome.cliError.requestSent {
		t.Fatalf("write did not dispatch: writes=%d outcome=%#v", writes.Load(), outcome)
	}
	if mcpErrorFact(outcome.cliError.Details, "outcome") != nil {
		t.Fatal("fixture unexpectedly acquired keyed history outcome")
	}
	envelope := mcpErrorJSON(t, app.mcpCommandError(cmd, args, defaultOptions(), outcome, mcpPolicy{}, ""))
	if lookupPathValue(envelope, "cli.outcome") != "unknown" || lookupPathValue(envelope, "cli.idempotency_key") != nil {
		t.Fatalf("class-none outcome metadata=%#v", envelope)
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(apiError), &want)
	if !reflect.DeepEqual(envelope["error"], want["error"]) {
		t.Fatalf("API error changed: %#v", envelope)
	}
	if strings.Contains(mcpResultText(app.mcpCommandError(cmd, args, defaultOptions(), outcome, mcpPolicy{}, "")), "idempotency_key") {
		t.Fatalf("class-none result acquired key advice: %#v", envelope)
	}
}

func TestMCPErrorsValidationFallbackUsesDetailVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		details       any
		want          []string
	}{
		{name: "required operation", message: "operation is required.", want: []string{"REQUIRED"}},
		{name: "invalid type", message: "arguments must be an object.", want: []string{"INVALID_VALUE"}},
		{name: "invalid JSON", message: "arguments must be a JSON object.", want: []string{"INVALID_VALUE"}},
		{name: "missing fact", message: "An input is missing.", details: map[string]any{"reason": "missing_required"}, want: []string{"REQUIRED"}},
		{name: "existing generic code", message: "Invalid arguments.", details: []any{map[string]any{"code": "INVALID_REQUEST", "message": "arguments must be an object."}}, want: []string{"INVALID_VALUE"}},
		{name: "translator codes", message: "4 arguments are invalid.", details: []map[string]any{{"code": "REQUIRED"}, {"code": "INVALID_VALUE"}, {"code": "UNKNOWN_FIELD"}, {"code": "NOT_ALLOWED"}}, want: []string{"REQUIRED", "INVALID_VALUE", "UNKNOWN_FIELD", "NOT_ALLOWED"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := usageError("INVALID_REQUEST", tc.message, "arguments")
			e.Details = tc.details
			before := cloneErrorJSON(tc.details)
			envelope := mcpCLIErrorEnvelope(e)
			details, ok := lookupPathValue(envelope, "error.details").([]any)
			if !ok || len(details) != len(tc.want) {
				t.Fatalf("invalid details: %#v", envelope)
			}
			for i, code := range tc.want {
				if detail, ok := details[i].(map[string]any); !ok || detail["code"] != code {
					t.Fatalf("detail %d = %#v, want %s", i, details[i], code)
				}
			}
			if !reflect.DeepEqual(tc.details, before) {
				t.Fatal("formatter mutated validation details")
			}
		})
	}
}

func TestMCPErrorsLocalShapeAndDispatchFacts(t *testing.T) {
	for _, tc := range []struct {
		name, operation string
		sent            bool
		wantKey         bool
	}{{"before send", "refunds.create", false, false}, {"write may be sent", "refunds.create", true, true}, {"read", "refunds.list", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := networkError("CLIENT_TIMEOUT", "The request timed out.", context.DeadlineExceeded)
			e.RequestID = "omit-local-request-id"
			e.idempotencyKey, e.requestSent = "local-key", tc.sent
			cmd, _ := NewRegistry().ByName(tc.operation)
			result := (&App{}).mcpLocalError(cmd, nil, Options{}, mcpPolicy{}, e, "sandbox")
			envelope := mcpErrorJSON(t, result)
			if lookupPathValue(envelope, "error.type") != "timeout_error" || lookupPathValue(envelope, "error.error_source") != "caller" || lookupPathValue(envelope, "error.remediation.retry") != "none" || lookupPathValue(envelope, "error.request_id") != nil {
				t.Fatal(envelope)
			}
			if (lookupPathValue(envelope, "cli.idempotency_key") != nil) != tc.wantKey {
				t.Fatal(envelope)
			}
			details, ok := lookupPathValue(envelope, "error.details").([]any)
			if !ok || len(details) != 1 || details[0].(map[string]any)["code"] != "CLIENT_TIMEOUT" {
				t.Fatal(envelope)
			}
		})
	}
}

func TestMCPErrorsEarlyParseFailureIsCaptured(t *testing.T) {
	app, _, _ := testApp(t, "")
	var captured *CLIError
	app.mcpErrorObserver = func(e *CLIError) { captured = e }
	exit := app.Run([]string{"customers", "list", "--not-a-flag"})
	if captured == nil || captured.ExitCode != exit || exit != ExitUsage {
		t.Fatalf("exit=%d captured=%#v", exit, captured)
	}
}

func TestMCPErrorsLocalCatalogDoesNotReuseAPICodes(t *testing.T) {
	document, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	properties := document.Components.Schemas["ErrorObject"]["properties"].(map[string]any)
	enum := properties["code"].(map[string]any)["enum"].([]any)
	apiCodes := map[string]bool{}
	for _, raw := range enum {
		apiCodes[raw.(string)] = true
	}
	// Some emitters choose these codes dynamically, so cover the complete MCP
	// catalog as well as literal emitters found in the source below.
	for _, code := range []string{
		"UNKNOWN_OPERATION", "OPERATION_NOT_AVAILABLE", "CONFIRMATION_REQUIRED", "CONFIRMATION_NEEDS_USER", "CONFIRMATION_DECLINED",
		"LIVE_ACCESS_DISABLED", "LIVE_ACKNOWLEDGEMENT_REQUIRED", "RESULT_TOO_LARGE", "API_KEY_REQUIRED", "CREDENTIAL_LOOKUP_FAILED",
		"CREDENTIAL_FORMAT_INVALID", "CLIENT_TIMEOUT", "NETWORK_ERROR", "CLIENT_CANCELED", "MCP_COMMAND_FAILED",
	} {
		if apiCodes[code] {
			t.Errorf("MCP local catalog reuses API code %s", code)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			var value ast.Expr
			switch n := node.(type) {
			case *ast.CallExpr:
				if f, ok := n.Fun.(*ast.Ident); ok {
					position := -1
					switch f.Name {
					case "usageError", "configError", "networkError", "credentialStoreError", "invalidResponseError":
						position = 0
					case "cliError":
						position = 2
					}
					if position >= 0 && position < len(n.Args) {
						value = n.Args[position]
					}
				}
			case *ast.CompositeLit:
				if typ, ok := n.Type.(*ast.Ident); ok && typ.Name == "CLIError" {
					for _, element := range n.Elts {
						if pair, ok := element.(*ast.KeyValueExpr); ok {
							if key, ok := pair.Key.(*ast.Ident); ok && key.Name == "Code" {
								value = pair.Value
							}
						}
					}
				}
			}
			if literal, ok := value.(*ast.BasicLit); ok && literal.Kind == token.STRING {
				code, err := strconv.Unquote(literal.Value)
				if err == nil {
					found[code] = true
					if code != "INVALID_REQUEST" && apiCodes[code] {
						t.Errorf("local emitter in %s reuses API code %s", path, code)
					}
				}
			}
			return true
		})
	}
	for _, code := range []string{"INVALID_REQUEST", "UNKNOWN_OPERATION", "OPERATION_NOT_AVAILABLE", "RESULT_TOO_LARGE", "MCP_COMMAND_FAILED"} {
		if !found[code] {
			t.Errorf("missing local emitter coverage for %s", code)
		}
	}
}
