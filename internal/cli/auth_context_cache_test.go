package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Drop the in-process entry so a new App must read the first App's disk cache.
func cachedContextTestApp(t *testing.T, baseURL string, first *App) *App {
	t.Helper()
	authContextCache.Delete(first.authContextCachePath(baseURL, "flint_test_test"))
	second, _, _ := testApp(t, baseURL)
	second.ConfigDir = first.ConfigDir
	second.WorkingDir = first.WorkingDir
	return second
}

func TestCachedScopeDenialRevalidatesOnce(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		change      func(*AuthContext)
		status      int
		invalidBody bool
		args        []string
		sandbox     string
		wantExit    int
		wantCode    string
	}{
		{name: "granted", wantExit: ExitOK},
		{name: "still denied", change: func(auth *AuthContext) { auth.Scopes = nil }, wantExit: ExitAuth, wantCode: "MISSING_REQUIRED_SCOPES"},
		{name: "revoked", status: http.StatusUnauthorized, wantExit: ExitAuth, wantCode: "INVALID_CREDENTIAL"},
		{name: "invalid response", invalidBody: true, wantExit: ExitAPI, wantCode: "INVALID_RESPONSE"},
		{name: "unknown environment", change: func(auth *AuthContext) { auth.Environment = "unknown" }, wantExit: ExitAuth, wantCode: "UNKNOWN_CREDENTIAL_ENVIRONMENT"},
		{name: "live acknowledgement", change: func(auth *AuthContext) { auth.Environment = "live" }, wantExit: ExitAuth, wantCode: "LIVE_ACKNOWLEDGEMENT_REQUIRED"},
		{name: "mode mismatch", args: []string{"--live"}, wantExit: ExitAuth, wantCode: "CREDENTIAL_MODE_MISMATCH"},
		{name: "merchant guard", args: []string{"--merchant", "mer_123"}, change: func(auth *AuthContext) { auth.MerchantID = "mer_other" }, wantExit: ExitAuth, wantCode: "MERCHANT_GUARD_MISMATCH"},
		{name: "sandbox guard", sandbox: "test_123", change: func(auth *AuthContext) { auth.SandboxID = "test_other" }, wantExit: ExitAuth, wantCode: "SANDBOX_GUARD_MISMATCH"},
		{name: "live raw ordinary write", args: []string{"--live"}, change: func(auth *AuthContext) { auth.Environment = "live" }, wantExit: ExitOK},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			auth := AuthContext{AuthType: "api_key", APIKeyID: "key_123", Environment: "sandbox", MerchantID: "mer_123", SandboxID: "test_123"}
			authCalls, targetCalls := 0, 0
			refresh := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/v1/auth-context" {
					targetCalls++
					if r.Method != http.MethodPost || r.URL.Path != "/v1/customers" {
						t.Errorf("unexpected target: %s %s", r.Method, r.URL.Path)
					}
					fmt.Fprint(w, `{"data":{"id":"cus_test"}}`)
					return
				}
				authCalls++
				if refresh && scenario.status != 0 {
					w.WriteHeader(scenario.status)
					fmt.Fprint(w, `{"error":{"type":"authentication_error","code":"INVALID_CREDENTIAL","message":"Credential revoked."}}`)
					return
				}
				if refresh && scenario.invalidBody {
					fmt.Fprint(w, `{"data":{}}`)
					return
				}
				_ = json.NewEncoder(w).Encode(authContextEnvelope{Data: auth})
			}))
			defer server.Close()
			first, stdout, stderr := testApp(t, server.URL)
			args := []string{"customers", "create", "--name", "Cache test", "--output", "json"}
			if exit := first.Run(args); exit != ExitAuth || !strings.Contains(stdout.String(), "MISSING_REQUIRED_SCOPES") {
				t.Fatalf("warm cache: exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			if authCalls != 1 || targetCalls != 0 {
				t.Fatalf("fresh denial retried: auth=%d target=%d", authCalls, targetCalls)
			}
			second := cachedContextTestApp(t, server.URL, first)
			auth.Scopes = allTestScopes()
			if scenario.change != nil {
				scenario.change(&auth)
			}
			if scenario.sandbox != "" {
				dir := filepath.Join(second.WorkingDir, ".flint")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(fmt.Sprintf(`{"sandbox_id":%q}`, scenario.sandbox)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			refresh = true
			if scenario.name == "live raw ordinary write" {
				// Known ordinary raw writes follow the shared ungated classification.
				args = []string{"api", "POST", "/v1/customers", "--input", `{"name":"Cache test"}`, "--output", "json"}
			}
			exit := second.Run(append(args, scenario.args...))
			output := fmt.Sprint(second.Stdout) + fmt.Sprint(second.Stderr)
			if exit != scenario.wantExit || (scenario.wantCode != "" && !strings.Contains(output, scenario.wantCode)) {
				t.Fatalf("exit=%d want=%d code=%s output=%s", exit, scenario.wantExit, scenario.wantCode, output)
			}
			wantTargets := 0
			if scenario.wantExit == ExitOK {
				wantTargets = 1
			}
			if authCalls != 2 || targetCalls != wantTargets {
				t.Fatalf("auth=%d want=2 target=%d want=%d", authCalls, targetCalls, wantTargets)
			}
		})
	}
}

func TestPositiveScopeCheckUsesDiskCache(t *testing.T) {
	authCalls, mutations := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			authCalls++
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		mutations++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/customers" {
			t.Errorf("unexpected target: %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"data":{"id":"cus_test"}}`)
	}))
	defer server.Close()
	first, _, stderr := testApp(t, server.URL)
	if exit := first.Run([]string{"auth", "status", "--output", "json"}); exit != ExitOK {
		t.Fatalf("warm cache: exit=%d stderr=%s", exit, stderr)
	}
	second := cachedContextTestApp(t, server.URL, first)
	if exit := second.Run([]string{"customers", "create", "--name", "Cache test", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d stderr=%s", exit, second.Stderr)
	}
	if authCalls != 1 || mutations != 1 {
		t.Fatalf("auth=%d mutations=%d", authCalls, mutations)
	}
}

func TestAuthStatusAlwaysFetchesCurrentContext(t *testing.T) {
	for _, command := range [][]string{{"auth", "status"}, {"whoami"}} {
		for _, change := range []string{"scopes and expiry", "expired", "revoked"} {
			t.Run(strings.Join(command, " ")+"/"+change, func(t *testing.T) {
				authCalls := 0
				status := http.StatusOK
				body := authContextJSON("sandbox")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/v1/auth-context" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					authCalls++
					w.WriteHeader(status)
					fmt.Fprint(w, body)
				}))
				defer server.Close()
				first, _, _ := testApp(t, server.URL)
				if _, e := first.fetchAuthContextResponse(context.Background(), server.URL, "flint_test_test", false); e != nil {
					t.Fatal(e)
				}
				second := cachedContextTestApp(t, server.URL, first)
				wantExit := ExitOK
				var current authContextEnvelope
				if change == "scopes and expiry" {
					_ = json.Unmarshal([]byte(body), &current)
					current.Data.Scopes = []string{"payments.payment_intents.read"}
					expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
					current.Data.ExpiresAt = &expiry
					current.RequestID = "req_current"
					raw, err := json.Marshal(current)
					if err != nil {
						t.Fatal(err)
					}
					body = string(raw)
				} else {
					status = http.StatusUnauthorized
					body = fmt.Sprintf(`{"error":{"type":"authentication_error","code":"INVALID_CREDENTIAL","message":%q}}`, "Credential "+change+".")
					wantExit = ExitAuth
				}
				exit := second.Run(append(command, "--output", "json"))
				if exit != wantExit || authCalls != 2 {
					t.Fatalf("exit=%d want=%d auth=%d stderr=%s", exit, wantExit, authCalls, second.Stderr)
				}
				if wantExit != ExitOK {
					if !strings.Contains(fmt.Sprint(second.Stdout), "Credential "+change) {
						t.Fatalf("missing current auth error: %s", second.Stdout)
					}
					if _, err := os.Stat(second.authContextCachePath(server.URL, "flint_test_test")); !os.IsNotExist(err) {
						t.Fatalf("cache not invalidated: %v", err)
					}
					return
				}
				var got authContextEnvelope
				if err := json.Unmarshal([]byte(fmt.Sprint(second.Stdout)), &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Data.Scopes) != 1 || got.Data.Scopes[0] != current.Data.Scopes[0] || got.Data.ExpiresAt == nil || *got.Data.ExpiresAt != *current.Data.ExpiresAt || got.RequestID != "req_current" {
					t.Fatalf("stale status: %+v", got)
				}
				cached, ok := second.cachedAuthContext(server.URL, "flint_test_test")
				if !ok || len(cached.Data.Scopes) != 1 || cached.Data.ExpiresAt == nil || *cached.Data.ExpiresAt != *current.Data.ExpiresAt {
					t.Fatalf("status did not update cache: %+v", cached)
				}
			})
		}
	}
}
