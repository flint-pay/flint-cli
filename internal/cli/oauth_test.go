package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testOAuthCredential(t *testing.T, base string, expiry time.Time) oauthCredential {
	t.Helper()
	var envelope authContextEnvelope
	if err := json.Unmarshal([]byte(oauthAuthJSON("sandbox")), &envelope); err != nil {
		t.Fatal(err)
	}
	return oauthCredential{Kind: "oauth", Version: 1, BaseURL: base, AccessToken: testOAuthAccess, RefreshToken: testOAuthRefresh, ExpiresAt: expiry, Auth: envelope.Data}
}
func installTestOAuth(t *testing.T, a *App, credential oauthCredential) func() string {
	t.Helper()
	var mu sync.Mutex
	raw, _ := json.Marshal(credential)
	stored := string(raw)
	a.LoadCredential = func(string) (string, error) { mu.Lock(); defer mu.Unlock(); return stored, nil }
	a.StoreCredential = func(_ string, value string) error { mu.Lock(); defer mu.Unlock(); stored = value; return nil }
	a.DeleteCredential = func(string) error { mu.Lock(); defer mu.Unlock(); stored = ""; return nil }
	return func() string { mu.Lock(); defer mu.Unlock(); return stored }
}

func TestOAuthManagedCommandsRefresh(t *testing.T) {
	for _, command := range [][]string{{"auth", "status"}, {"doctor"}, {"config", "validate"}, {"api", "get", "/v1/developer/auth-context"}} {
		t.Run(strings.Join(command, "-"), func(t *testing.T) {
			var refreshes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case loginTokenPath:
					checkOAuthForm(t, r)
					if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != testOAuthRefresh {
						t.Error("wrong refresh request")
					}
					refreshes.Add(1)
					writeTestTokens(w, "new-access", "new-refresh")
				case "/v1/developer/auth-context":
					if r.Header.Get("Authorization") != "Bearer new-access" {
						t.Error("expired token sent to API")
					}
					fmt.Fprint(w, oauthAuthJSON("sandbox"))
				case "/v1/openapi.json":
					fmt.Fprint(w, `{"x-flint-api-releases":{"current_version":"2026-02-01"}}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			stored := installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(-time.Minute)))
			argv := append(append([]string{}, command...), "--output", "json", "--debug")
			if exit := app.Run(argv); exit != ExitOK {
				t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
			}
			c, e := decodeOAuthCredential(stored())
			if e != nil || c.RefreshToken != "new-refresh" || refreshes.Load() != 1 {
				t.Fatal("refresh not persisted exactly once")
			}
			for _, secret := range []string{testOAuthAccess, testOAuthRefresh, "new-access", "new-refresh"} {
				if strings.Contains(out.String()+stderr.String(), secret) {
					t.Fatal("OAuth token exposed")
				}
			}
		})
	}
}

func TestOAuthConcurrentRefresh(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == loginTokenPath {
			refreshes.Add(1)
			time.Sleep(40 * time.Millisecond)
			writeTestTokens(w, "new-access", "new-refresh")
			return
		}
		fmt.Fprint(w, oauthAuthJSON("sandbox"))
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(-time.Minute)))
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, e := app.oauthAccess(context.Background(), "default")
			if e != nil || c.AccessToken != "new-access" {
				t.Error("concurrent refresh failed")
			}
		}()
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("rotated %d times", refreshes.Load())
	}
}

func TestOAuthRefreshFailures(t *testing.T) {
	for _, scenario := range []string{"invalid_grant", "changed_grant", "keychain", "malformed", "no_rotation"} {
		t.Run(scenario, func(t *testing.T) {
			var revoked atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case loginTokenPath:
					if scenario == "invalid_grant" {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":"invalid_grant","error_description":"synthetic-refresh-token"}`)
						return
					}
					if scenario == "malformed" {
						fmt.Fprint(w, `{"access_token":"new-access","token_type":"Bearer","expires_in":3600}`)
						return
					}
					refresh := "new-refresh"
					if scenario == "no_rotation" {
						refresh = testOAuthRefresh
					}
					writeTestTokens(w, "new-access", refresh)
				case oauthRevokePath:
					revoked.Store(true)
				default:
					body := oauthAuthJSON("sandbox")
					if scenario == "changed_grant" {
						body = strings.ReplaceAll(body, "grant_test", "grant_other")
					}
					fmt.Fprint(w, body)
				}
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			stored := installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(-time.Minute)))
			before := stored()
			if scenario == "keychain" {
				app.StoreCredential = func(string, string) error { return errors.New("storage failed") }
			}
			if exit := app.Run([]string{"auth", "status", "--output", "json"}); exit != ExitAuth {
				t.Fatalf("exit=%d out=%s", exit, out)
			}
			if (scenario != "changed_grant" && stored() != before) || (scenario != "invalid_grant" && !revoked.Load()) || strings.Contains(out.String(), testOAuthRefresh) {
				t.Fatal("unsafe refresh failure handling")
			}
			if scenario == "changed_grant" {
				c, e := decodeOAuthCredential(stored())
				if e != nil || !c.PendingValidation || c.Auth.OAuthGrantID != "grant_test" {
					t.Fatal("changed identity was trusted")
				}
			}
		})
	}
}

func TestOAuthLogoutRevokesBeforeDeleting(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var revoked atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != oauthRevokePath {
					t.Error("logout tried to refresh or call API")
				}
				checkOAuthForm(t, r)
				if r.Form.Get("token") != testOAuthRefresh || r.Form.Get("token_type_hint") != "refresh_token" {
					t.Error("wrong token revoked")
				}
				if fail {
					w.WriteHeader(503)
					return
				}
				revoked.Store(true)
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			stored := installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(-time.Minute)))
			previousDelete := app.DeleteCredential
			app.DeleteCredential = func(profile string) error {
				if !revoked.Load() {
					t.Error("deleted before revocation")
				}
				return previousDelete(profile)
			}
			exit := app.Run([]string{"auth", "logout", "--confirm", "--output", "json"})
			if fail {
				if exit != ExitAuth || stored() == "" {
					t.Fatalf("failed revoke lost credential: %s", out)
				}
			} else if exit != ExitOK || stored() != "" {
				t.Fatalf("logout failed: %s", out)
			}
		})
	}
}

func TestOAuthIssuerMismatchAndRedirects(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	installTestOAuth(t, app, testOAuthCredential(t, target.URL, time.Now().Add(-time.Minute)))
	if exit := app.Run([]string{"auth", "status", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), "OAUTH_SERVER_MISMATCH") {
		t.Fatalf("out=%s", out)
	}
	app.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	_, e := app.oauthRequest(context.Background(), server.URL, loginTokenPath, url.Values{"client_id": {oauthClientID}, "refresh_token": {testOAuthRefresh}})
	if e == nil || leaked.Load() {
		t.Fatal("redirect exposed credentials")
	}
}

func TestOAuthOfflineDryRunAndHistory(t *testing.T) {
	app, out, stderr := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_API_KEY", "")
	c := testOAuthCredential(t, "http://127.0.0.1:1", time.Now().Add(-time.Minute))
	installTestOAuth(t, app, c)
	if err := app.updateConfig(func(cfg *Config) error {
		cfg.Profiles["default"] = Profile{Environment: "sandbox", MerchantID: "mer_123", SandboxID: "test_123"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if exit := app.Run([]string{"customers", "create", "--dry-run", "client", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
	}
	if strings.Contains(out.String(), testOAuthAccess) || strings.Contains(out.String(), testOAuthRefresh) {
		t.Fatal("dry-run exposed tokens")
	}
	if err := app.updateHistory(func(h *History) error {
		h.Entries = []HistoryEntry{{ID: "ord_same", Profile: "default", Environment: "sandbox", MerchantID: "mer_123", SandboxID: "test_123", CredentialScope: "oauth:grant_test"}, {ID: "ord_other", Profile: "default", Environment: "sandbox", MerchantID: "mer_123", SandboxID: "test_123", CredentialScope: "oauth:grant_other"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if exit := app.Run([]string{"history", "--output", "json"}); exit != ExitOK || !strings.Contains(out.String(), "ord_same") || strings.Contains(out.String(), "ord_other") {
		t.Fatalf("out=%s", out)
	}
}

func TestOAuthRefreshDuringLongCommandAndNoMutationReplay(t *testing.T) {
	var refreshed, writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loginTokenPath:
			refreshed.Add(1)
			writeTestTokens(w, "new-access", "new-refresh")
		case "/v1/developer/auth-context":
			fmt.Fprint(w, oauthAuthJSON("sandbox"))
		default:
			writes.Add(1)
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Error("old token used after expiry")
			}
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"Expired"}}`)
		}
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(-time.Minute)))
	ctx := context.WithValue(context.Background(), oauthSessionKey{}, &oauthSession{Profile: "default", GrantID: "grant_test"})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, e := app.doRequest(ctx, server.URL, testOAuthAccess, "POST", "/v1/orders", []byte(`{}`), "test-idempotency", false)
	if e == nil || e.ExitCode != ExitAuth || writes.Load() != 1 || refreshed.Load() != 1 {
		t.Fatalf("writes=%d refresh=%d error=%v", writes.Load(), refreshed.Load(), e)
	}
}

func TestOAuthAPICredentialOverrideStillWins(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer flint_test_test" {
			t.Error("saved OAuth session overrode API key")
		}
		fmt.Fprint(w, authContextJSON("sandbox"))
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(-time.Minute)))
	if exit := app.Run([]string{"auth", "status", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d", exit)
	}
}

func TestOAuthTokenResponseRejectsUnsupportedCredentials(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, raw := range []string{`{"data":{"secret_key":"flint_test_old"}}`, `{"access_token":"access","refresh_token":"refresh","token_type":"mac","expires_in":3600}`, `{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","expires_in":0}`} {
		if _, e := app.decodeOAuthTokens([]byte(raw), defaultAPIBaseURL); e == nil {
			t.Fatal("accepted invalid token response")
		}
	}
}

func TestOAuthContextServerErrorIsNotAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"synthetic-access-token"}}`)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(time.Hour)))
	if exit := app.Run([]string{"doctor", "--output", "json"}); exit != ExitAPI || !strings.Contains(out.String(), `"name":"connectivity"`) || strings.Contains(out.String(), testOAuthAccess) {
		t.Fatalf("exit=%d out=%s", exit, out)
	}
}
