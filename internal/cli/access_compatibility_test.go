package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAccessCompatibilityAuthContextFields(t *testing.T) {
	for _, tc := range []struct{ raw, environment, mode, authType, access string }{
		{`{"environment":"sandbox","auth_type":"oauth"}`, "sandbox", "", "oauth", ""},
		{`{"mode":"live","environment":"sandbox","auth_type":"cli_login","access":"full"}`, "live", "live", "cli_login", "full"},
		{`{"mode":"test","auth_type":"api_key","access":"restricted"}`, "sandbox", "test", "api_key", "restricted"},
		{`{"mode":"","environment":"live","auth_type":"workload_identity","access":"read_only"}`, "live", "", "workload_identity", "read_only"},
		{`{"mode":"future","environment":"sandbox","auth_type":"future","access":"future"}`, "future", "future", "future", "future"},
	} {
		var auth AuthContext
		if err := json.Unmarshal([]byte(tc.raw), &auth); err != nil {
			t.Fatal(err)
		}
		if auth.Environment != tc.environment || auth.Mode != tc.mode || auth.AuthType != tc.authType || auth.Access != tc.access {
			t.Fatalf("raw=%s auth=%+v", tc.raw, auth)
		}
	}
}

func TestAccessCompatibilityCLILoginSession(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	credential := testOAuthCredential(t, "http://127.0.0.1:9", time.Now().Add(time.Hour))
	credential.Auth.AuthType = "cli_login"
	credential.Auth.Mode = "test"
	credential.Auth.Access = "full"
	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	decoded, e := decodeOAuthCredential(string(raw))
	if e != nil || !matchesOAuthCredential(decoded, decoded.Auth) {
		t.Fatal("cli_login session rejected", e)
	}
	cmd, _ := app.Registry.ByName("subscriptions.list")
	decoded.Auth.Scopes = []string{"customers.read"}
	if e := commandPermissionError(cmd, decoded.Auth, "default"); e == nil || !strings.Contains(fmt.Sprint(e.Details), "flint login --new-session") {
		t.Fatal("cli_login scope guidance missing", e)
	}
	check := doctorScopeCheck(credential.Auth)
	if !strings.Contains(fmt.Sprint(check), "browser session") {
		t.Fatal(check)
	}
}

func TestAccessCompatibilityLocalGuardsUseMode(t *testing.T) {
	for _, tc := range []struct {
		prefix, mode, legacy string
		want                 int
	}{
		{"flint_live_", "test", "live", ExitOK},
		{"flint_test_", "live", "sandbox", ExitAuth},
	} {
		for _, command := range []string{"init", "auth import"} {
			t.Run(tc.prefix+command, func(t *testing.T) {
				app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/openapi.json" {
						w.Header().Set("Flint-Version", "2026-02-01")
						return
					}
					if r.URL.Path != "/v1/auth-context" {
						t.Fatalf("unexpected request %s", r.URL.Path)
					}
					if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"auth_type": "api_key", "mode": tc.mode, "environment": tc.legacy, "access": "full", "merchant_id": "mer_123", "sandbox_id": "test_123", "scopes": allTestScopes()}}); err != nil {
						t.Fatal(err)
					}
				})
				args := []string{"init"}
				if command == "auth import" {
					app.Stdin = strings.NewReader(tc.prefix + "synthetic\n")
					args = []string{"auth", "import", "--stdin"}
				} else {
					t.Setenv("FLINT_API_KEY", tc.prefix+"synthetic")
				}
				exit, _ := runJSON(t, app, out, args...)
				refusal := "LIVE_ACKNOWLEDGEMENT_REQUIRED"
				if command == "init" {
					refusal = "This credential is live."
				}
				if exit != tc.want || tc.want != ExitOK && !strings.Contains(out.String(), refusal) {
					t.Fatalf("exit=%d out=%s", exit, out)
				}
			})
		}
	}
}

func TestAccessCompatibilitySignupFallbackOnlyOn404(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 404, 409, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var paths, keys []string
			var bodies []map[string]any
			app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/sandboxes/test_new/api-keys" && r.URL.Path != "/v1/api-keys" {
					signupFlowSuccess(t, w, r)
					return
				}
				if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer synthetic-session" {
					t.Error("wrong key issuance method or credential")
				}
				paths = append(paths, r.URL.Path)
				keys = append(keys, r.Header.Get("Idempotency-Key"))
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				bodies = append(bodies, body)
				if r.URL.Path == "/v1/sandboxes/test_new/api-keys" && status != 200 {
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":{"code":"ISSUANCE_REFUSED","message":"refused","remediation":{"retry":"none"}}}`)
					return
				}
				fmt.Fprint(w, `{"data":{"api_key_id":"key_new","merchant_id":"mer_new","secret_key":"flint_test_synthetic_new"}}`)
			})
			exit, _ := runJSON(t, app, out, append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input")...)
			wantPaths := []string{"/v1/sandboxes/test_new/api-keys"}
			wantBodies := []map[string]any{{"access": "full"}}
			if status == 404 {
				wantPaths = append(wantPaths, "/v1/api-keys")
				wantBodies = append(wantBodies, map[string]any{"access": "full", "sandbox_id": "test_new"})
			}
			if !reflect.DeepEqual(paths, wantPaths) || !reflect.DeepEqual(bodies, wantBodies) {
				t.Fatalf("paths=%v bodies=%v", paths, bodies)
			}
			if keys[0] == "" || len(keys) == 2 && keys[1] != keys[0] {
				t.Fatal("missing or changed issuance idempotency key")
			}
			if (status == 200 || status == 404) != (exit == ExitOK) {
				t.Fatalf("status=%d exit=%d out=%s", status, exit, out)
			}
		})
	}
}

func TestAccessCompatibilitySignupWithoutRetiredReadiness(t *testing.T) {
	for _, sandboxInVerify := range []bool{true, false} {
		t.Run(fmt.Sprint(sandboxInVerify), func(t *testing.T) {
			var paths []string
			app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/v1/onboarding/verify-email":
					if sandboxInVerify {
						fmt.Fprint(w, `{"data":{"onboarding_session_token":"synthetic-session","default_sandbox_id":"test_new"}}`)
					} else {
						signupFlowSuccess(t, w, r)
					}
				case "/v1/onboarding/state":
					fmt.Fprint(w, `{"data":{"default_sandbox_id":"test_new","status":"complete"}}`)
				default:
					signupFlowSuccess(t, w, r)
				}
			})
			exit, _ := runJSON(t, app, out, append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input")...)
			if exit != ExitOK {
				t.Fatal(out)
			}
			want := []string{"/v1/onboarding/start", "/v1/onboarding/verify-email"}
			if !sandboxInVerify {
				want = append(want, "/v1/onboarding/state")
			}
			want = append(want, "/v1/sandboxes/test_new/api-keys")
			if !reflect.DeepEqual(paths, want) {
				t.Fatalf("paths=%v want=%v", paths, want)
			}
		})
	}
}

func TestAccessCompatibilitySignupFallbackFailureStops(t *testing.T) {
	var paths []string
	app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/test_new/api-keys" && r.URL.Path != "/v1/api-keys" {
			signupFlowSuccess(t, w, r)
			return
		}
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"NOT_FOUND","message":"no such route"}}`)
	})
	exit, _ := runJSON(t, app, out, append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input")...)
	if exit == ExitOK || !reflect.DeepEqual(paths, []string{"/v1/sandboxes/test_new/api-keys", "/v1/api-keys"}) {
		t.Fatalf("paths=%v exit=%d output=%s", paths, exit, out)
	}
}

func TestAccessCompatibilityCLILoginTokenExport(t *testing.T) {
	app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth-context" {
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
		body := strings.Replace(oauthAuthJSON("sandbox"), `"auth_type":"oauth"`, `"auth_type":"cli_login","access":"restricted"`, 1)
		body = strings.Replace(body, `"environment":"sandbox"`, `"mode":"test"`, 1)
		fmt.Fprint(w, body)
	})
	t.Setenv("FLINT_BASE_URL", "http://127.0.0.1:9")
	credential := testOAuthCredential(t, "http://127.0.0.1:9", time.Now().Add(time.Hour))
	credential.Auth.AuthType = "cli_login"
	credential.Auth.Mode = "test"
	installTestOAuth(t, app, credential)
	exit, result := runJSON(t, app, out, "auth", "token")
	if exit != ExitOK || result["access_token"] != testOAuthAccess {
		t.Fatalf("exit=%d token export failed", exit)
	}
}
