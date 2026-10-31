package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

func TestCredentialClassMismatchBeforeLookupOrRequest(t *testing.T) {
	for _, tc := range []struct {
		name, command, key, token, workload, code, source string
	}{
		{"api_key_buyer", "me.orders.list", "flint_test_example", "", "", "BUYER_CREDENTIAL_REQUIRED", opindex.CredentialAPIKey},
		{"saved_login_buyer", "me.orders.list", "", "", "", "BUYER_CREDENTIAL_REQUIRED", opindex.CredentialLogin},
		{"api_key_beats_workload", "me.orders.list", "flint_test_example", "", "wid_example", "BUYER_CREDENTIAL_REQUIRED", opindex.CredentialAPIKey},
		{"workload_buyer", "me.orders.list", "", "", "wid_example", "BUYER_CREDENTIAL_REQUIRED", opindex.CredentialWorkload},
		{"merchant_access_buyer", "me.orders.list", "", "fcat_example", "", "BUYER_CREDENTIAL_REQUIRED", opindex.CredentialAccessToken},
		{"workload_access_buyer", "me.orders.list", "", "fwat_example", "", "BUYER_CREDENTIAL_REQUIRED", opindex.CredentialAccessToken},
		{"customer_session_merchant", "orders.list", "", "flint_cses_example", "", "MERCHANT_CREDENTIAL_REQUIRED", opindex.CredentialAccessToken},
		{"customer_session_beats_api_key", "orders.list", "flint_test_example", "flint_cses_example", "wid_example", "MERCHANT_CREDENTIAL_REQUIRED", opindex.CredentialAccessToken},
		{"customer_session_listen", "listen", "", "flint_cses_example", "", "MERCHANT_CREDENTIAL_REQUIRED", opindex.CredentialAccessToken},
		{"customer_session_status", "auth.status", "", "flint_cses_example", "", "MERCHANT_CREDENTIAL_REQUIRED", opindex.CredentialAccessToken},
		{"checkout_session_merchant", "orders.list", "", "ckat_v1.example", "", "MERCHANT_CREDENTIAL_REQUIRED", opindex.CredentialAccessToken},
		{"partner_unsupported", "customer-sessions.create", "", "fpat_v1.example", "", "PARTNER_ACCESS_NOT_ALLOWED", opindex.CredentialAccessToken},
	} {
		for _, dryRun := range []string{"", "client"} {
			t.Run(tc.name+"/"+dryRun, func(t *testing.T) {
				app, _, _ := testApp(t, "http://127.0.0.1:1")
				t.Setenv("FLINT_API_KEY", tc.key)
				t.Setenv("FLINT_ACCESS_TOKEN", tc.token)
				t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", tc.workload)
				lookups, requests := 0, 0
				app.LoadCredential = func(string) (string, error) { lookups++; return "", errors.New("must not load") }
				app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) { requests++; return nil, errors.New("must not request") })}
				command, ok := app.Registry.ByName(tc.command)
				if !ok {
					t.Fatal(tc.command)
				}
				opts := defaultOptions()
				opts.DryRun = dryRun
				_, _, _, e := app.authenticateCommand(context.Background(), command, opts, ResolvedConfig{ProfileName: "default"})
				if e == nil || e.ExitCode != ExitAuth || e.Code != tc.code || e.Type != errorTypeAuthentication {
					t.Fatalf("error: %#v", e)
				}
				if lookups != 0 || requests != 0 {
					t.Fatalf("lookups=%d requests=%d", lookups, requests)
				}
				details := e.Details.(map[string]any)
				if details["credential"] != tc.source {
					t.Fatal(details)
				}
				if details["remediation"].(map[string]any)["retry"] != "none" {
					t.Fatal(details)
				}
			})
		}
	}
}

func TestCredentialClassCompatibleAndUnknown(t *testing.T) {
	for _, tc := range []struct{ command, token string }{
		{"orders.list", "fcat_example"},
		{"orders.list", "fwat_example"},
		{"orders.list", "fpat_v1.example"},
		{"checkout-sessions.get", "ckat_v1.example"},
		{"checkout-sessions.get", "fcat_example"},
		{"me.orders.list", "flint_cses_example"},
		{"me.orders.list", "unknown_future_token"},
		{"orders.list", "unknown_future_token"},
		{"onboarding.start", "flint_cses_example"},
	} {
		t.Run(tc.command+"/"+tc.token, func(t *testing.T) {
			app, _, _ := testApp(t, "http://127.0.0.1:1")
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_ACCESS_TOKEN", tc.token)
			t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
			command, ok := app.Registry.ByName(tc.command)
			if !ok {
				t.Fatal(tc.command)
			}
			if e := app.credentialClassError(command); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestCredentialClassRawCommandUsesMatchedOperation(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_ACCESS_TOKEN", "flint_cses_example")
	command, _ := app.Registry.ByName("api")
	opts := defaultOptions()
	opts.Positionals = []string{"get", "/v1/orders"}
	if e := app.credentialClassError(resolveAPICommand(command, opts)); e == nil || e.Code != "MERCHANT_CREDENTIAL_REQUIRED" {
		t.Fatalf("error=%#v", e)
	}
	opts.Positionals = []string{"get", "/v1/future-resource"}
	if e := app.credentialClassError(resolveAPICommand(command, opts)); e != nil {
		t.Fatal(e)
	}
}

func TestCredentialClassCustomerRemediationParses(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_ACCESS_TOKEN", "")
	command, _ := app.Registry.ByName("me.orders.list")
	e := app.credentialClassError(command)
	if e == nil {
		t.Fatal("missing error")
	}
	details := e.Details.(map[string]any)
	if !reflect.DeepEqual(details["accepted_schemes"], []string{"CustomerSessionBearer"}) {
		t.Fatal(details)
	}
	action := details["remediation"].(map[string]any)["next_actions"].([]any)[0].(map[string]any)
	if !strings.Contains(action["command"].(string), `--input '{"customer_id":"CUSTOMER_ID"}'`) {
		t.Fatal(action)
	}
	parsed, opts, help, parseErr := parseInvocation(app.Registry, []string{"customer-sessions", "create", "--input", `{"customer_id":"cus_example"}`, "--field", "secret"})
	if parseErr != nil || help || parsed.OperationID != "createCustomerSession" {
		t.Fatalf("parsed=%+v help=%t error=%v", parsed, help, parseErr)
	}
	if e := validateOptions(parsed, &opts, false); e != nil {
		t.Fatal(e)
	}
}

func TestCredentialClassStopsJSON(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		args                               []string
		token, key, code, scheme, workload string
	}{
		{"merchant_key_buyer", []string{"me", "orders", "list"}, "", "flint_test_example", "BUYER_CREDENTIAL_REQUIRED", "CustomerSessionBearer", ""},
		{"saved_buyer", []string{"me", "orders", "list"}, "", "", "BUYER_CREDENTIAL_REQUIRED", "CustomerSessionBearer", ""},
		{"buyer_merchant", []string{"orders", "list"}, "flint_cses_example", "", "MERCHANT_CREDENTIAL_REQUIRED", "BearerAuth", ""},
		{"buyer_listen", []string{"listen"}, "flint_cses_example", "", "MERCHANT_CREDENTIAL_REQUIRED", "BearerAuth", ""},
		{"buyer_status", []string{"auth", "status"}, "flint_cses_example", "", "MERCHANT_CREDENTIAL_REQUIRED", "BearerAuth", ""},
		{"buyer_trigger", []string{"trigger", "customer.created"}, "flint_cses_example", "", "MERCHANT_CREDENTIAL_REQUIRED", "BearerAuth", ""},
		{"partner_unsupported", []string{"customer-sessions", "create", "--input", `{"customer_id":"cus_example"}`}, "fpat_v1.example", "", "PARTNER_ACCESS_NOT_ALLOWED", "BearerAuth", ""},
		{"checkout_required", []string{"checkout-sessions", "customer-verifications", "create", "cs_example", "--input", `{"email":"ada@example.com","purpose":"use_saved_payment_methods"}`}, "", "flint_test_example", "BUYER_CREDENTIAL_REQUIRED", "CheckoutSessionBearer", ""},
		{"buyer_client_dry_run", []string{"me", "addresses", "create", "--input", `{}`, "--dry-run=client"}, "", "flint_test_example", "BUYER_CREDENTIAL_REQUIRED", "CustomerSessionBearer", ""},
		{"workload_buyer", []string{"me", "orders", "list"}, "", "", "BUYER_CREDENTIAL_REQUIRED", "CustomerSessionBearer", "wid_example"},
		{"api_key_beats_workload", []string{"me", "orders", "list"}, "", "flint_test_example", "BUYER_CREDENTIAL_REQUIRED", "CustomerSessionBearer", "wid_example"},
		{"session_beats_api_key_and_workload", []string{"orders", "list"}, "flint_cses_example", "flint_test_example", "MERCHANT_CREDENTIAL_REQUIRED", "BearerAuth", "wid_example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, out, _ := testApp(t, "http://127.0.0.1:1")
			t.Setenv("FLINT_ACCESS_TOKEN", tc.token)
			t.Setenv("FLINT_API_KEY", tc.key)
			t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", tc.workload)
			lookups, requests := 0, 0
			app.LoadCredential = func(string) (string, error) { lookups++; return "", errors.New("unexpected lookup") }
			app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) { requests++; return nil, errors.New("unexpected request") })}
			exit, result := runJSON(t, app, out, tc.args...)
			if exit != ExitAuth || lookups != 0 || requests != 0 {
				t.Fatalf("exit=%d lookups=%d requests=%d output=%s", exit, lookups, requests, out)
			}
			for path, want := range map[string]any{"error.code": tc.code, "error.type": errorTypeAuthentication, "error.error_source": "caller", "cli.origin": "cli", "error.remediation.retry": "none"} {
				if got, _ := lookupPath(result, path); got != want {
					t.Errorf("%s=%v, want %v", path, got, want)
				}
			}
			accepted, _ := lookupPath(result, "error.details.accepted_schemes")
			if !reflect.DeepEqual(accepted, []any{tc.scheme}) {
				t.Errorf("accepted=%v", accepted)
			}
			if _, exists := lookupPath(result, "error.request_id"); exists {
				t.Error("local failure has request_id")
			}
		})
	}
}

func TestCredentialClassTransportPassThrough(t *testing.T) {
	for _, tc := range []struct {
		name            string
		args            []string
		token, wantAuth string
	}{
		{"customer", []string{"me", "orders", "list"}, "flint_cses_example", "Bearer flint_cses_example"},
		{"checkout", []string{"checkout-sessions", "customer-verifications", "create", "cs_example", "--input", `{"email":"ada@example.com","purpose":"use_saved_payment_methods"}`, "--confirm"}, "ckat_v1.example", "Bearer ckat_v1.example"},
		{"merchant", []string{"orders", "list"}, "fcat_example", "Bearer fcat_example"},
		{"unknown_buyer", []string{"me", "orders", "list"}, "future_token", "Bearer future_token"},
		{"unknown_merchant", []string{"orders", "list"}, "future_token", "Bearer future_token"},
		{"keyless", []string{"openapi", "get"}, "flint_cses_example", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/v1/auth-context" {
					t.Error("unexpected introspection")
				}
				if r.Header.Get("Authorization") != tc.wantAuth {
					t.Error("incorrect selected credential")
				}
				fmt.Fprint(w, `{"data":[],"request_id":"req_local"}`)
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_ACCESS_TOKEN", tc.token)
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
			app.LoadCredential = func(string) (string, error) { t.Error("unexpected lookup"); return "", nil }
			if exit := app.Run(append(tc.args, "--output", "json")); exit != ExitOK {
				t.Fatalf("exit=%d output=%s", exit, out)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
