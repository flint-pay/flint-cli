package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reviewReauthToken(t *testing.T, state *contextTestServer, id string) []byte {
	t.Helper()
	state.mu.Lock()
	state.tokens["review-reauth-access"] = id
	state.mu.Unlock()
	data, err := json.Marshal(map[string]any{"access_token": "review-reauth-access", "refresh_token": "review-reauth-refresh", "token_type": "Bearer", "expires_in": 3600, "oauth_session_id": "session_one", "context_id": id})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func TestContextReviewReusedLoginHonorsIntent(t *testing.T) {
	for _, args := range [][]string{{"login", "--live", "--output", "json"}, {"login", "--merchant", "mer_other", "--output", "json"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			app, out, _, state, _ := newContextTestApp(t)
			if exit := app.Run(args); exit != ExitAuth {
				t.Fatalf("login ignored explicit intent: exit=%d out=%s", exit, out)
			}
			if state.devices != 0 {
				t.Fatal("intent mismatch created a new browser session")
			}
		})
	}
}
func TestContextReviewReauthRetainsLiveSelectionWithSandboxCache(t *testing.T) {
	app, out, _, state, stored := newContextTestApp(t)
	if exit := app.Run([]string{"context", "switch", "ctx_live", "--live", "--output", "json"}); exit != ExitOK {
		t.Fatalf("%d %s", exit, out)
	}
	if exit := app.Run([]string{"auth", "status", "--context", "ctx_a", "--output", "json"}); exit != ExitOK {
		t.Fatalf("%d %s", exit, out)
	}
	cmd, opts, _, _ := parseInvocation(app.Registry, []string{"reauth", "--output", "json"})
	resolved, _, _ := app.resolveConfig(opts)
	previous, e := decodeOAuthCredential(stored())
	if e != nil {
		t.Fatal(e)
	}
	out.Reset()
	exit := app.finishBrowserLogin(context.Background(), cmd, opts, resolved, previous.BaseURL, reviewReauthToken(t, state, "ctx_live"), previous, true)
	if exit != ExitOK || state.revokes != 0 {
		t.Fatalf("reauth revoked valid live session: exit=%d revokes=%d out=%s", exit, state.revokes, out)
	}
	cfg, _ := app.loadConfig()
	if cfg.Profiles["default"].ContextID != "ctx_live" {
		t.Fatal("lost live default")
	}
}
func TestContextReviewReauthPreservesConcurrentDefaultSwitch(t *testing.T) {
	app, out, _, state, stored := newContextTestApp(t)
	cmd, opts, _, _ := parseInvocation(app.Registry, []string{"reauth", "--output", "json"})
	resolved, _, _ := app.resolveConfig(opts)
	if exit := app.Run([]string{"context", "switch", "ctx_b", "--output", "json"}); exit != ExitOK {
		t.Fatalf("%d %s", exit, out)
	}
	previous, e := decodeOAuthCredential(stored())
	if e != nil {
		t.Fatal(e)
	}
	exit := app.finishBrowserLogin(context.Background(), cmd, opts, resolved, previous.BaseURL, reviewReauthToken(t, state, "ctx_a"), previous, true)
	if exit != ExitOK {
		t.Fatalf("%d %s", exit, out)
	}
	cfg, _ := app.loadConfig()
	if cfg.Profiles["default"].ContextID != "ctx_b" {
		t.Fatalf("reauth overwrote another terminal's selection: %s", cfg.Profiles["default"].ContextID)
	}
}
func TestContextReviewPendingHistoryDoesNotUsePreviousContext(t *testing.T) {
	app, out, _, _, stored := newContextTestApp(t)
	c, e := decodeOAuthCredential(stored())
	if e != nil {
		t.Fatal(e)
	}
	c.ContextID = "ctx_b"
	c.PendingValidation = true
	raw, _ := json.Marshal(c)
	if err := app.StoreCredential("default", string(raw)); err != nil {
		t.Fatal(err)
	}
	if exit := app.Run([]string{"history", "--context", "ctx_b", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), "OAUTH_CONTEXT_UNAVAILABLE") {
		t.Fatalf("pending history used old context: exit=%d out=%s", exit, out)
	}
}

func TestContextReviewReauthProjectPinPreservesProfile(t *testing.T) {
	app, out, _, state, stored := newContextTestApp(t)
	project := filepath.Join(app.WorkingDir, ".flint")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "config.json"), []byte(`{"context":"ctx_b"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd, opts, _, _ := parseInvocation(app.Registry, []string{"reauth", "--output", "json"})
	resolved, before, err := app.resolveConfig(opts)
	if err != nil {
		t.Fatal(err)
	}
	previous, e := decodeOAuthCredential(stored())
	if e != nil {
		t.Fatal(e)
	}
	if exit := app.finishBrowserLogin(context.Background(), cmd, opts, resolved, previous.BaseURL, reviewReauthToken(t, state, "ctx_b"), previous, true); exit != ExitOK {
		t.Fatalf("%d %s", exit, out)
	}
	after, err := app.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if after.Profiles["default"] != before.Profiles["default"] {
		t.Fatal("reauthorizing a project changed the profile default")
	}
	c, e := decodeOAuthCredential(stored())
	if e != nil || c.ContextID != "ctx_b" || c.PendingValidation {
		t.Fatalf("reauthorized project credential not saved: %v", e)
	}
}

func TestContextReviewRemovedContextDoesNotRevokeSession(t *testing.T) {
	app, out, _, state, stored := newContextTestApp(t)
	c, e := decodeOAuthCredential(stored())
	if e != nil {
		t.Fatal(e)
	}
	c.ExpiresAt = time.Now().Add(-time.Minute)
	raw, _ := json.Marshal(c)
	if err := app.StoreCredential("default", string(raw)); err != nil {
		t.Fatal(err)
	}
	state.unauthorizedContext = "ctx_a"
	_, e = app.oauthAccess(withOAuthContext(context.Background(), "ctx_a"), "default")
	if e == nil || e.Code != "CONTEXT_ACCESS_DENIED" || state.revokes != 0 {
		t.Fatalf("one removed context revoked session: error=%v revokes=%d", e, state.revokes)
	}
	next, e := decodeOAuthCredential(stored())
	if e != nil || !next.PendingValidation || next.RefreshToken == testOAuthRefresh {
		t.Fatalf("rotated session lost: pending=%v err=%v", next.PendingValidation, e)
	}
	if exit := app.Run([]string{"context", "switch", "ctx_b", "--output", "json"}); exit != ExitOK {
		t.Fatalf("cannot recover by switching: %d %s", exit, out)
	}
	if state.revokes != 0 {
		t.Fatal("switching revoked the session")
	}
}
func TestContextReviewOldCommandDoesNotRotateReplacementSession(t *testing.T) {
	app, _, _, state, stored := newContextTestApp(t)
	c, e := decodeOAuthCredential(stored())
	if e != nil {
		t.Fatal(e)
	}
	original := c
	c.SessionID = "session_replacement"
	c.Auth.OAuthSessionID = c.SessionID
	c.Auth.OAuthGrantID = "grant_replacement"
	c.ExpiresAt = time.Now().Add(-time.Minute)
	raw, _ := json.Marshal(c)
	if err := app.StoreCredential("default", string(raw)); err != nil {
		t.Fatal(err)
	}
	auth := original.Auth
	ctx := context.WithValue(context.Background(), oauthSessionKey{}, &oauthSession{Profile: "default", GrantID: auth.OAuthGrantID, SessionID: auth.OAuthSessionID, ContextID: auth.ContextID, MerchantID: auth.MerchantID, Environment: auth.Environment, SandboxID: auth.SandboxID})
	_, e = app.oauthRequestToken(ctx, original.AccessToken, original.BaseURL)
	if e == nil || e.Code != "OAUTH_SESSION_CHANGED" || state.refreshes != 0 || state.revokes != 0 {
		t.Fatalf("old command touched new session: error=%v refreshes=%d revokes=%d", e, state.refreshes, state.revokes)
	}
	if stored() != string(raw) {
		t.Fatal("old command changed replacement credential")
	}
}

func TestContextLoginRecoversRevokedSession(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached-token", true: "refresh-token"}[expired], func(t *testing.T) {
			app, out, _, state, stored := newContextTestApp(t)
			state.revokedOriginal = true
			if expired {
				c, _ := decodeOAuthCredential(stored())
				c.ExpiresAt = time.Now().Add(-time.Minute)
				raw, _ := json.Marshal(c)
				if err := app.StoreCredential("default", string(raw)); err != nil {
					t.Fatal(err)
				}
			}
			if exit := app.Run([]string{"login", "--no-open", "--output", "json"}); exit != ExitOK {
				t.Fatalf("login did not recover: %d %s", exit, out)
			}
			c, e := decodeOAuthCredential(stored())
			if e != nil || c.RefreshToken == testOAuthRefresh || state.devices != 1 {
				t.Fatalf("new login was not saved: error=%v devices=%d", e, state.devices)
			}
		})
	}
}

func TestContextLoginDoesNotReplaceUnconfirmedSession(t *testing.T) {
	for _, kind := range []string{"removed-context", "api-outage", "session-check-failed", "no-input"} {
		t.Run(kind, func(t *testing.T) {
			app, out, _, state, stored := newContextTestApp(t)
			before := stored()
			args := []string{"login", "--no-open", "--output", "json"}
			switch kind {
			case "removed-context":
				state.unauthorizedContext = "ctx_a"
			case "api-outage":
				state.outage = true
			case "session-check-failed":
				state.unauthorizedContext = "ctx_a"
				state.contextListError = "invalid_client"
			case "no-input":
				state.revokedOriginal = true
				args = append(args, "--no-input")
			}
			if exit := app.Run(args); exit == ExitOK {
				t.Fatalf("unexpected success: %s", out)
			}
			if state.devices != 0 || state.revokes != 0 || stored() != before {
				t.Fatal("login replaced or revoked the saved session")
			}
		})
	}
}
