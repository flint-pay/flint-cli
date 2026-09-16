package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testDeviceSecret = "synthetic-device-secret"
const testOAuthAccess = "synthetic-access-token"
const testOAuthRefresh = "synthetic-refresh-token"

func oauthAuthJSON(environment string) string {
	return fmt.Sprintf(`{"data":{"auth_type":"oauth","oauth_grant_id":"grant_test","environment":%q,"merchant_id":"mer_123","sandbox_id":"test_123","scopes":["payments.payment_intents.read"]},"meta":{"api_version":"2026-02-01"}}`, environment)
}
func writeTestDevice(w http.ResponseWriter) {
	fmt.Fprint(w, `{"device_code":"synthetic-device-secret","user_code":"ABCD-EFGH","verification_uri":"https://app.withflintpay.com/cli/activate","verification_uri_complete":"https://app.withflintpay.com/cli/activate?user_code=ABCD-EFGH","expires_in":600,"interval":1}`)
}
func writeTestTokens(w http.ResponseWriter, access, refresh string) {
	fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"token_type":"Bearer","expires_in":3600,"scope":"payments.payment_intents.read"}`, access, refresh)
}
func checkOAuthForm(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != "POST" || r.Header.Get("Authorization") != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Errorf("invalid OAuth request headers")
	}
	if err := r.ParseForm(); err != nil {
		t.Error(err)
	}
	if r.Form.Get("client_id") != oauthClientID || r.Form.Get("client_secret") != "" {
		t.Error("invalid public client")
	}
}

func TestBrowserLoginSuccess(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case loginDevicePath:
			checkOAuthForm(t, r)
			if r.Form.Get("environment") != "sandbox" || r.Form.Get("scope") == "" {
				t.Error("missing requested context/scopes")
			}
			writeTestDevice(w)
		case loginTokenPath:
			checkOAuthForm(t, r)
			if r.Form.Get("grant_type") != deviceGrantType || r.Form.Get("device_code") != testDeviceSecret {
				t.Error("invalid device grant")
			}
			polls++
			if polls == 1 {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
				return
			}
			writeTestTokens(w, testOAuthAccess, testOAuthRefresh)
		case "/v1/developer/auth-context":
			if r.Header.Get("Authorization") != "Bearer "+testOAuthAccess {
				t.Error("wrong access token")
			}
			fmt.Fprint(w, oauthAuthJSON("sandbox"))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	if err := app.updateConfig(func(cfg *Config) error { cfg.Profiles["browser"] = Profile{}; return nil }); err != nil {
		t.Fatal(err)
	}
	var opened, stored string
	app.OpenBrowser = func(target string) error { opened = target; return nil }
	app.StoreCredential = func(profile, secret string) error {
		if profile != "browser" {
			t.Error("wrong profile")
		}
		stored = secret
		return nil
	}
	if exit := app.Run([]string{"auth", "login", "--profile", "browser", "--output", "json", "--debug"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
	}
	if opened != "https://app.withflintpay.com/cli/activate?user_code=ABCD-EFGH" {
		t.Fatalf("opened=%s", opened)
	}
	credential, e := decodeOAuthCredential(stored)
	if e != nil || credential.AccessToken != testOAuthAccess || credential.RefreshToken != testOAuthRefresh || credential.BaseURL != server.URL {
		t.Fatal("OAuth session not saved correctly")
	}
	cfg, err := app.loadConfig()
	if err != nil || cfg.Profiles["browser"].MerchantID != "mer_123" {
		t.Fatal("profile metadata missing")
	}
	raw, _ := json.Marshal(cfg)
	for _, secret := range []string{testDeviceSecret, testOAuthAccess, testOAuthRefresh} {
		if strings.Contains(out.String()+stderr.String()+string(raw), secret) {
			t.Fatal("credential exposed")
		}
	}
}

func TestBrowserLoginTerminalStates(t *testing.T) {
	for _, code := range []string{"access_denied", "expired_token", "slow_down"} {
		t.Run(code, func(t *testing.T) {
			polls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == loginDevicePath {
					writeTestDevice(w)
					return
				}
				polls++
				w.WriteHeader(400)
				fmt.Fprintf(w, `{"error":%q,"error_description":"synthetic-device-secret"}`, code)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			app.OpenBrowser = func(string) error { t.Error("--no-open opened browser"); return nil }
			app.StoreCredential = func(string, string) error { t.Error("unexpected save"); return nil }
			want := "LOGIN_DENIED"
			if code != "access_denied" {
				want = "LOGIN_EXPIRED"
			}
			if exit := app.Run([]string{"auth", "login", "--no-open", "--timeout", "2200ms", "--output", "json"}); exit != ExitAuth || polls != 1 || !strings.Contains(out.String(), want) {
				t.Fatalf("exit=%d polls=%d out=%s", exit, polls, out)
			}
			if strings.Contains(out.String()+stderr.String(), testDeviceSecret) {
				t.Fatal("secret exposed")
			}
		})
	}
}

func TestBrowserLoginUnavailableAndCanceled(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			want := "LOGIN_UNAVAILABLE"
			if canceled {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				app.Context = ctx
				want = "LOGIN_CANCELED"
			}
			if exit := app.Run([]string{"auth", "login", "--no-open", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), want) {
				t.Fatalf("exit=%d out=%s", exit, out)
			}
		})
	}
}

func TestBrowserLoginURLs(t *testing.T) {
	good := "https://app.withflintpay.com/cli"
	for _, uri := range []string{"http://app.withflintpay.com/cli", "https://evil.example/cli", "https://user:pass@app.withflintpay.com/cli", "file:///tmp/login"} {
		if _, ok := loginVerificationURL(uri, "", defaultAPIBaseURL, "ABCD-EFGH"); ok {
			t.Errorf("accepted %s", uri)
		}
		if _, ok := loginVerificationURL(good, uri, defaultAPIBaseURL, "ABCD-EFGH"); ok {
			t.Errorf("accepted complete %s", uri)
		}
	}
	if _, ok := loginVerificationURL(good, "", defaultAPIBaseURL, "BAD\nCODE"); ok {
		t.Error("accepted control character")
	}
	if target, ok := loginVerificationURL(good, "", defaultAPIBaseURL, "ABCD-EFGH"); !ok || target != good {
		t.Error("client invented a complete URI")
	}
}

func TestBrowserLoginRejectsOverridesAndAutomation(t *testing.T) {
	for _, variable := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_CHECKOUT_SESSION_SECRET", "no-input"} {
		t.Run(variable, func(t *testing.T) {
			app, out, _ := testApp(t, "http://127.0.0.1:1")
			t.Setenv("FLINT_API_KEY", "")
			argv := []string{"auth", "login", "--output", "json"}
			if variable == "no-input" {
				argv = append(argv, "--no-input")
			} else {
				t.Setenv(variable, "synthetic-override")
			}
			if exit := app.Run(argv); exit == ExitOK || strings.Contains(out.String(), "synthetic-override") {
				t.Fatalf("exit=%d out=%s", exit, out)
			}
		})
	}
}

func TestBrowserLoginFailureRevokesOAuthGrant(t *testing.T) {
	for _, scenario := range []string{"keychain", "environment"} {
		t.Run(scenario, func(t *testing.T) {
			revoked := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case loginDevicePath:
					writeTestDevice(w)
				case loginTokenPath:
					writeTestTokens(w, testOAuthAccess, testOAuthRefresh)
				case "/v1/developer/auth-context":
					env := "sandbox"
					if scenario == "environment" {
						env = "live"
					}
					fmt.Fprint(w, oauthAuthJSON(env))
				case oauthRevokePath:
					checkOAuthForm(t, r)
					if r.Form.Get("token") != testOAuthRefresh || r.Form.Get("token_type_hint") != "refresh_token" {
						t.Error("wrong revocation request")
					}
					revoked = true
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			app.OpenBrowser = func(string) error { return errors.New("no browser") }
			app.StoreCredential = func(string, string) error {
				if scenario == "environment" {
					t.Error("overwrote previous session")
				}
				return errors.New("keychain unavailable")
			}
			if exit := app.Run([]string{"auth", "login", "--output", "json"}); exit != ExitAuth || !revoked {
				t.Fatalf("exit=%d revoked=%t out=%s", exit, revoked, out)
			}
			if !strings.Contains(stderr.String(), "Open the link above") {
				t.Error("missing browser fallback")
			}
		})
	}
}

func TestBrowserLoginCancellationWhilePolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeTestDevice(w) }))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	app.Context = ctx
	app.OpenBrowser = func(string) error { cancel(); return nil }
	start := time.Now()
	if exit := app.Run([]string{"auth", "login", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), "LOGIN_CANCELED") || time.Since(start) > time.Second {
		t.Fatalf("exit=%d out=%s", exit, out)
	}
}

func TestBrowserLoginRejectsMalformedResponses(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":{"secret_key":"flint_test_legacy"}}`, `{"device_code":"secret","user_code":"ABCD-EFGH","verification_uri":"https://app.withflintpay.com/secret","expires_in":600,"interval":1}`, `{"device_code":"secret","user_code":"ABCD-EFGH","verification_uri":"https://app.withflintpay.com/cli","expires_in":0,"interval":1}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			app.OpenBrowser = func(string) error { t.Error("opened invalid URL"); return nil }
			if exit := app.Run([]string{"auth", "login", "--output", "json"}); exit == ExitOK || !strings.Contains(out.String(), "INVALID_LOGIN_RESPONSE") {
				t.Fatalf("exit=%d out=%s", exit, out)
			}
		})
	}
}

func TestSavedLoginOutput(t *testing.T) {
	for _, kind := range []string{"sandbox", "live", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			auth := contextTestAuth("ctx_a")
			if kind == "live" {
				auth = contextTestAuth("ctx_live")
			}
			auth.Name = "cedar-stone"
			if kind == "legacy" {
				auth.ContextID = ""
				auth.OAuthSessionID = ""
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/developer/auth-context" {
					t.Errorf("reused login made unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": auth})
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			credential := testOAuthCredential(t, server.URL, time.Now().Add(time.Hour))
			credential.Auth = auth
			if kind != "legacy" {
				credential.Version = 2
				credential.ContextID = auth.ContextID
				credential.SessionID = auth.OAuthSessionID
			}
			installTestOAuth(t, app, credential)
			if exit := app.Run([]string{"login"}); exit != ExitOK {
				t.Fatalf("login failed: exit=%d output=%s", exit, out)
			}
			for _, want := range []string{"Already authenticated.\n", "Context: cedar-stone\n", "Environment: " + strings.ToUpper(auth.Environment) + "\n", "Merchant: " + auth.MerchantID + "\n", "Next: flint "} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q in human output: %s", want, out)
				}
			}
			if auth.SandboxID != "" && !strings.Contains(out.String(), "Sandbox: "+auth.SandboxID+"\n") {
				t.Errorf("sandbox missing: %s", out)
			}
			if auth.ContextID != "" && !strings.Contains(out.String(), "Context ID: "+auth.ContextID+"\n") {
				t.Errorf("context ID missing: %s", out)
			}
			for _, unwanted := range []string{"{", "<nil>", "oauth:", "session_one", auth.OAuthGrantID, auth.Scopes[0]} {
				if strings.Contains(out.String(), unwanted) {
					t.Errorf("internal auth details %q in human summary: %s", unwanted, out)
				}
			}
			// Machine output must retain the full public context, including scopes.
			out.Reset()
			if exit := app.Run([]string{"auth", "login", "--output", "json"}); exit != ExitOK {
				t.Fatalf("JSON login failed: %d %s", exit, out)
			}
			var result struct {
				Data struct {
					AlreadyAuthenticated bool        `json:"already_authenticated"`
					ActiveContext        AuthContext `json:"active_context"`
				} `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Data.AlreadyAuthenticated || result.Data.ActiveContext.Name != auth.Name || result.Data.ActiveContext.OAuthGrantID != auth.OAuthGrantID || len(result.Data.ActiveContext.Scopes) != len(auth.Scopes) {
				t.Fatalf("machine output lost context: %s", out)
			}
			out.Reset()
			if exit := app.Run([]string{"login", "--field", "data.active_context.name"}); exit != ExitOK || out.String() != "cedar-stone\n" {
				t.Fatalf("field output changed: %d %s", exit, out)
			}
		})
	}
}
