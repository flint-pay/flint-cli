package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Flint describes the onboarding next step as a readiness action
// (action_type and owner). These cases cover that shape and the legacy
// code/submit_endpoint shape, which servers send until they are upgraded.

func TestSignupDrivesReadinessActions(t *testing.T) {
	var paths []string
	var advanceBodies []map[string]any
	var keyBody map[string]any
	app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1/onboarding/state":
			if r.URL.RawQuery != "" {
				t.Errorf("state query = %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"data":{"can_issue_api_key":false,"default_sandbox_id":"test_new","next_step":{"action_type":"advance_onboarding","owner":"integrator"}}}`)
		case "/v1/onboarding/advance":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			advanceBodies = append(advanceBodies, body)
			if len(advanceBodies) == 1 {
				fmt.Fprint(w, `{"data":{"can_issue_api_key":false,"default_sandbox_id":"test_new","next_step":{"action_type":"advance_onboarding","owner":"integrator","required_fields":["website_url"]}}}`)
				return
			}
			// create_api_key ends the loop even while can_issue_api_key lags.
			fmt.Fprint(w, `{"data":{"can_issue_api_key":false,"default_sandbox_id":"test_new","next_step":{"action_type":"create_api_key","owner":"integrator","required_fields":["name"]}}}`)
		case "/v1/sandboxes/test_new/api-keys":
			if err := json.NewDecoder(r.Body).Decode(&keyBody); err != nil {
				t.Fatal(err)
			}
			signupFlowSuccess(t, w, r)
		default:
			signupFlowSuccess(t, w, r)
		}
	})
	args := append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--country", "us", "--website-url", "https://example.test", "--no-input", "--output", "json")
	if exit := app.Run(args); exit != ExitOK {
		t.Fatalf("signup exit=%d out=%s stderr=%s", exit, out, stderr)
	}
	wantPaths := []string{"/v1/onboarding/start", "/v1/onboarding/verify-email", "/v1/onboarding/state", "/v1/onboarding/advance", "/v1/onboarding/advance", "/v1/sandboxes/test_new/api-keys"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("paths=%v want %v", paths, wantPaths)
	}
	wantAdvance := []map[string]any{
		{"country": "US", "profile": map[string]any{"website_url": "https://example.test"}},
		{"country": "US", "profile": map[string]any{"website_url": "https://example.test"}},
	}
	if !reflect.DeepEqual(advanceBodies, wantAdvance) {
		t.Fatalf("advance bodies=%#v", advanceBodies)
	}
	if !reflect.DeepEqual(keyBody, map[string]any{"access": "full"}) {
		t.Fatalf("api-key body=%#v; the initial key explicitly requests full access", keyBody)
	}
}

func TestSignupStopsForReadinessActionsItCannotComplete(t *testing.T) {
	for _, test := range []struct {
		name, step, code string
	}{
		{"account session", `{"action_type":"create_merchant_account_session","owner":"integrator","merchant_account_session":{"components":["account_onboarding"]}}`, "SIGNUP_HUMAN_ACTION_REQUIRED"},
		{"review", `{"action_type":"wait_for_review","owner":"flint"}`, "SIGNUP_HUMAN_ACTION_REQUIRED"},
		{"merchant owner", `{"action_type":"advance_onboarding","owner":"merchant"}`, "SIGNUP_HUMAN_ACTION_REQUIRED"},
		{"flint owner", `{"action_type":"advance_onboarding","owner":"flint"}`, "SIGNUP_HUMAN_ACTION_REQUIRED"},
		{"unknown action", `{"action_type":"other","owner":"integrator"}`, "SIGNUP_HUMAN_ACTION_REQUIRED"},
		{"action wins over legacy fields", `{"action_type":"create_merchant_account_session","owner":"integrator","code":"start_onboarding","machine_completable":true,"submit_endpoint":"/v1/onboarding/advance"}`, "SIGNUP_HUMAN_ACTION_REQUIRED"},
		{"legacy human step", `{"code":"complete_verification_step","owner":"human","machine_completable":false}`, "SIGNUP_HUMAN_ACTION_REQUIRED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var paths []string
			app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/v1/onboarding/state" {
					fmt.Fprintf(w, `{"data":{"can_issue_api_key":false,"default_sandbox_id":"test_new","next_step":%s}}`, test.step)
					return
				}
				signupFlowSuccess(t, w, r)
			})
			exit, result := runJSON(t, app, out, append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input")...)
			if exit == ExitOK || lookupPathValue(result, "error.code") != test.code {
				t.Fatalf("exit=%d result=%v", exit, result)
			}
			if lookupPathValue(result, "error.details.next_step") == nil {
				t.Fatalf("error omits the next step: %v", result)
			}
			for _, path := range paths {
				if path == "/v1/onboarding/advance" || path == "/v1/sandboxes/test_new/api-keys" {
					t.Fatalf("signup submitted %s for a step it cannot complete", path)
				}
			}
		})
	}
}

func TestSignupStopsWhenAReadinessActionRepeats(t *testing.T) {
	advances := 0
	app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/onboarding/state", "/v1/onboarding/advance":
			if r.URL.Path == "/v1/onboarding/advance" {
				advances++
			}
			fmt.Fprint(w, `{"data":{"can_issue_api_key":false,"default_sandbox_id":"test_new","next_step":{"action_type":"advance_onboarding","owner":"integrator","required_fields":["website_url"]}}}`)
		default:
			signupFlowSuccess(t, w, r)
		}
	})
	exit, result := runJSON(t, app, out, append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--website-url", "https://example.test", "--no-input")...)
	if exit == ExitOK || lookupPathValue(result, "error.code") != "SIGNUP_ONBOARDING_STALLED" || advances != 1 {
		t.Fatalf("exit=%d advances=%d result=%v", exit, advances, result)
	}
}

func TestSignupLegacyStepShapes(t *testing.T) {
	t.Run("submit endpoint", func(t *testing.T) {
		var paths []string
		app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			switch r.URL.Path {
			case "/v1/onboarding/state":
				fmt.Fprint(w, `{"data":{"can_issue_api_key":false,"default_sandbox_id":"test_new","next_step":{"code":"start_onboarding","owner":"agent","machine_completable":true,"submit_method":"POST","submit_endpoint":"https://api.example.test/v1/onboarding/advance","required_fields":["country","sandbox_id"]}}}`)
			case "/v1/onboarding/advance":
				fmt.Fprint(w, `{"data":{"can_issue_api_key":true,"default_sandbox_id":"test_new","next_step":{"code":"create_api_key","owner":"agent","machine_completable":true}}}`)
			default:
				signupFlowSuccess(t, w, r)
			}
		})
		args := append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--country", "US", "--no-input", "--output", "json")
		if exit := app.Run(args); exit != ExitOK {
			t.Fatalf("signup exit=%d out=%s stderr=%s", exit, out, stderr)
		}
		if strings.Join(paths, ",") != "/v1/onboarding/start,/v1/onboarding/verify-email,/v1/onboarding/state,/v1/onboarding/advance,/v1/sandboxes/test_new/api-keys" {
			t.Fatalf("paths=%v", paths)
		}
	})
	t.Run("create api key code", func(t *testing.T) {
		issued := false
		app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/onboarding/state":
				fmt.Fprint(w, `{"data":{"can_issue_api_key":false,"default_sandbox_id":"test_new","next_step":{"code":"create_api_key","owner":"agent","machine_completable":true,"submit_endpoint":"/v1/onboarding/api-key","required_fields":["name","sandbox_id"]}}}`)
			case "/v1/sandboxes/test_new/api-keys":
				issued = true
				signupFlowSuccess(t, w, r)
			default:
				signupFlowSuccess(t, w, r)
			}
		})
		args := append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input", "--output", "json")
		if exit := app.Run(args); exit != ExitOK || !issued {
			t.Fatalf("signup exit=%d issued=%t out=%s stderr=%s", exit, issued, out, stderr)
		}
	})
}

const sandboxAPIKeyInput = `{"name":"ci","access":"restricted","scopes":["payments.payment_intents.read"]}`

func sandboxAPIKeyServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]string) {
	t.Helper()
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("live"))
			return
		}
		requests = append(requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Idempotency-Key"))
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestSandboxAPIKeysCreateRoutes(t *testing.T) {
	const created = `{"data":{"api_key_id":"key_sandbox","secret_key":"flint_test_synthetic_sandbox"}}`
	path := "/v1/sandboxes/test_123/api-keys"
	for _, test := range []struct {
		name    string
		argv    []string
		handler func(w http.ResponseWriter, r *http.Request)
		exit    int
		code    string
		want    []string
	}{
		{
			name: "api-keys create",
			argv: []string{"sandboxes", "api-keys", "create", "test_123"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, created)
			},
			exit: ExitOK,
			want: []string{"POST " + path + " idem_sandbox_key"},
		},
		{
			name: "test-key alias",
			argv: []string{"sandboxes", "api-keys", "create", "test_123"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, created)
			},
			exit: ExitOK,
			want: []string{"POST " + path + " idem_sandbox_key"},
		},
		{
			name: "missing sandbox",
			argv: []string{"sandboxes", "api-keys", "create", "test_123"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":"SANDBOX_NOT_FOUND","message":"Sandbox not found."}}`)
			},
			exit: ExitAPI,
			code: "SANDBOX_NOT_FOUND",
			want: []string{"POST " + path + " idem_sandbox_key"},
		},
		{
			name: "missing route is not retried",
			argv: []string{"sandboxes", "api-keys", "create", "test_123"},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"type":"request_error","code":"ROUTE_NOT_FOUND","message":"No public API route matches this request."}}`)
			},
			exit: ExitAPI,
			code: "ROUTE_NOT_FOUND",
			want: []string{"POST " + path + " idem_sandbox_key"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, requests := sandboxAPIKeyServer(t, test.handler)
			app, out, _ := testApp(t, server.URL)
			argv := append(append([]string{}, test.argv...), "--input", sandboxAPIKeyInput, "--idempotency-key", "idem_sandbox_key", "--live", "--confirm")
			exit, result := runJSON(t, app, out, argv...)
			if exit != test.exit || (test.code != "" && lookupPathValue(result, "error.code") != test.code) {
				t.Fatalf("exit=%d result=%v", exit, result)
			}
			if !reflect.DeepEqual(*requests, test.want) {
				t.Fatalf("requests=%v want %v", *requests, test.want)
			}
		})
	}
}

func TestSandboxAPIKeysCreateDryRunShowsRoute(t *testing.T) {
	server, requests := sandboxAPIKeyServer(t, func(w http.ResponseWriter, r *http.Request) {})
	app, out, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "flint_live_synthetic")
	exit, result := runJSON(t, app, out, "sandboxes", "api-keys", "create", "test_123", "--input", sandboxAPIKeyInput, "--live", "--dry-run", "client")
	if exit != ExitOK || lookupPathValue(result, "data.path") != "/v1/sandboxes/test_123/api-keys" || len(*requests) != 0 {
		t.Fatalf("exit=%d requests=%v result=%v", exit, *requests, result)
	}
}
