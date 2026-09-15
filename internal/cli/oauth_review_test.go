package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthReviewRefreshSurvivesContextOutage(t *testing.T) {
	var unavailable atomic.Bool
	unavailable.Store(true)
	var refreshes, revocations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loginTokenPath:
			refreshes.Add(1)
			writeTestTokens(w, "new-access", "new-refresh")
		case oauthRevokePath:
			revocations.Add(1)
		case "/v1/developer/auth-context":
			if unavailable.Load() {
				w.WriteHeader(500)
				return
			}
			fmt.Fprint(w, oauthAuthJSON("sandbox"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	stored := installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(-time.Minute)))
	if _, e := app.oauthAccess(context.Background(), "default"); e == nil || e.ExitCode != ExitAPI {
		t.Errorf("want server failure, got %v", e)
	}
	c, e := decodeOAuthCredential(stored())
	if e != nil || c.RefreshToken != "new-refresh" || revocations.Load() != 0 {
		t.Fatalf("temporary outage discarded or revoked rotated session")
	}
	unavailable.Store(false)
	c, e = app.oauthAccess(context.Background(), "default")
	if e != nil || c.AccessToken != "new-access" || refreshes.Load() != 1 || revocations.Load() != 0 {
		t.Fatalf("recovery refreshed/revoked instead of validating saved tokens: %v", e)
	}
}

func TestOAuthReviewLogoutRequiresCompletedRevocation(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusNoContent} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			stored := installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(time.Hour)))
			if exit := app.Run([]string{"auth", "logout", "--confirm", "--output", "json"}); exit != ExitAuth || stored() == "" {
				t.Fatalf("logout accepted incomplete revocation: exit=%d out=%s", exit, out)
			}
		})
	}
}

func TestOAuthReviewDoctorDistinguishesLocalCredentialFailure(t *testing.T) {
	app, out, _ := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_API_KEY", "")
	app.LoadCredential = func(string) (string, error) { return `{"kind":"oauth","version":1}`, nil }
	if exit := app.Run([]string{"doctor", "--output", "json"}); exit != ExitAuth {
		t.Fatalf("exit=%d", exit)
	}
	if strings.Contains(out.String(), "API rejected") || !strings.Contains(out.String(), `"name":"credential","status":"fail"`) {
		t.Fatalf("local failure blamed on API: %s", out)
	}
}

func TestOAuthReviewChecksHonorAccessTokenOverride(t *testing.T) {
	for _, command := range [][]string{{"doctor"}, {"config", "validate"}, {"init"}} {
		t.Run(strings.Join(command, "-"), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/openapi.json" {
					fmt.Fprint(w, `{"x-flint-api-releases":{"current_version":"2026-02-01"}}`)
					return
				}
				if r.Header.Get("Authorization") == "Bearer expired-environment-token" {
					w.WriteHeader(401)
					fmt.Fprint(w, `{"error":{"message":"Invalid credential."}}`)
					return
				}
				fmt.Fprint(w, oauthAuthJSON("sandbox"))
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_ACCESS_TOKEN", "expired-environment-token")
			installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(time.Hour)))
			args := append(append([]string{}, command...), "--output", "json")
			if exit := app.Run(args); exit != ExitAuth || strings.Contains(out.String(), "expired-environment-token") {
				t.Fatalf("wrong credential checked: exit=%d out=%s", exit, out)
			}
		})
	}
}

func TestOAuthReviewPendingSessionCannotReachBusinessEndpoint(t *testing.T) {
	var businessRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.WriteHeader(500)
			return
		}
		businessRequests.Add(1)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	c := testOAuthCredential(t, server.URL, time.Now().Add(time.Hour))
	c.PendingValidation = true
	installTestOAuth(t, app, c)
	ctx := context.WithValue(context.Background(), oauthSessionKey{}, &oauthSession{Profile: "default", GrantID: "grant_test"})
	if _, e := app.doRequest(ctx, server.URL, c.AccessToken, http.MethodPost, "/v1/orders", []byte(`{}`), "test-review", false); e == nil || e.ExitCode != ExitAPI || businessRequests.Load() != 0 {
		t.Fatalf("unverified token reached business API: %v", e)
	}
}

func TestOAuthReviewVerificationStateSaveFailureIsRecoverable(t *testing.T) {
	var refreshes, revocations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loginTokenPath:
			refreshes.Add(1)
			writeTestTokens(w, "new-access", "new-refresh")
		case oauthRevokePath:
			revocations.Add(1)
		default:
			fmt.Fprint(w, oauthAuthJSON("sandbox"))
		}
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	stored := installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(-time.Minute)))
	save := app.StoreCredential
	app.StoreCredential = func(profile, raw string) error {
		c, e := decodeOAuthCredential(raw)
		if e != nil {
			return e
		}
		if !c.PendingValidation {
			return fmt.Errorf("keychain temporarily unavailable")
		}
		return save(profile, raw)
	}
	if _, e := app.oauthAccess(context.Background(), "default"); e == nil || e.Code != "KEYCHAIN_WRITE_FAILED" {
		t.Fatalf("want storage failure, got %v", e)
	}
	c, e := decodeOAuthCredential(stored())
	if e != nil || !c.PendingValidation || c.RefreshToken != "new-refresh" || revocations.Load() != 0 {
		t.Fatal("lost staged session")
	}
	app.StoreCredential = save
	c, e = app.oauthAccess(context.Background(), "default")
	if e != nil || c.PendingValidation || refreshes.Load() != 1 || revocations.Load() != 0 {
		t.Fatalf("could not retry verification: %v", e)
	}
}
