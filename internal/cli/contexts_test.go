package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func contextTestAuth(id string) AuthContext {
	auth := AuthContext{AuthType: "oauth", OAuthSessionID: "session_one", ContextID: id, OAuthGrantID: "grant_" + id, Environment: "sandbox", MerchantID: "mer_" + id, SandboxID: "test_" + id, Scopes: []string{"payments.payment_intents.read"}}
	if id == "ctx_live" {
		auth.Environment = "live"
		auth.SandboxID = ""
	}
	return auth
}

type contextTestServer struct {
	mu                  sync.Mutex
	tokens              map[string]string
	refreshes           int
	requested           []string
	deny                string
	outage              bool
	unauthorizedContext string
	revokedOriginal     bool
	contextListError    string
	wrongSession        bool
	devices             int
	revokes             int
}

func newContextTestApp(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer, *contextTestServer, func() string) {
	t.Helper()
	state := &contextTestServer{tokens: map[string]string{testOAuthAccess: "ctx_a"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case oauthContextsPath:
			if state.revokedOriginal || state.contextListError != "" {
				code := state.contextListError
				if state.revokedOriginal {
					code = "invalid_grant"
				}
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
				return
			}
			_ = r.ParseForm()
			if r.Form.Get("refresh_token") == "" || r.Form.Get("client_id") != oauthClientID {
				t.Error("missing session authentication")
			}
			list := contextList{SessionID: "session_one", Contexts: []authorizedContext{}}
			for _, id := range []string{"ctx_a", "ctx_b", "ctx_live"} {
				auth := contextTestAuth(id)
				list.Contexts = append(list.Contexts, authorizedContext{ID: id, Name: id, MerchantID: auth.MerchantID, Environment: auth.Environment, SandboxID: auth.SandboxID})
			}
			_ = json.NewEncoder(w).Encode(list)
		case loginDevicePath, oauthReauthorizePath:
			state.devices++
			if r.URL.Path == oauthReauthorizePath {
				_ = r.ParseForm()
				if r.Form.Get("refresh_token") == "" {
					t.Error("reauth has no credential")
				}
			}
			writeTestDevice(w)
		case loginTokenPath:
			_ = r.ParseForm()
			if state.revokedOriginal && r.Form.Get("refresh_token") == testOAuthRefresh {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
			id := r.Form.Get("context_id")
			if id == "" {
				id = "ctx_a"
			}
			if id == state.deny {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"invalid_context"}`)
				return
			}
			state.refreshes++
			state.requested = append(state.requested, id)
			access := fmt.Sprintf("access-%d", state.refreshes)
			state.tokens[access] = id
			session := "session_one"
			if state.wrongSession {
				session = "session_other"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": fmt.Sprintf("refresh-%d", state.refreshes), "token_type": "Bearer", "expires_in": 3600, "oauth_session_id": session, "context_id": id})
		case "/v1/developer/auth-context":
			if state.revokedOriginal && r.Header.Get("Authorization") == "Bearer "+testOAuthAccess {
				w.WriteHeader(401)
				return
			}
			if state.outage {
				w.WriteHeader(503)
				return
			}
			id := state.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			if id == "" || id == state.unauthorizedContext {
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": contextTestAuth(id)})
		case oauthRevokePath:
			state.revokes++
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	app, out, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	auth := contextTestAuth("ctx_a")
	credential := oauthCredential{Kind: "oauth", Version: 2, SessionID: auth.OAuthSessionID, ContextID: auth.ContextID, BaseURL: server.URL, AccessToken: testOAuthAccess, RefreshToken: testOAuthRefresh, ExpiresAt: time.Now().Add(time.Hour), Auth: auth}
	stored := installTestOAuth(t, app, credential)
	if err := app.updateConfig(func(cfg *Config) error { cfg.Profiles["default"] = Profile{ContextID: "ctx_a"}; return nil }); err != nil {
		t.Fatal(err)
	}
	return app, out, stderr, state, stored
}

func TestContextListAndSwitch(t *testing.T) {
	app, out, stderr, state, _ := newContextTestApp(t)
	if exit := app.Run([]string{"context", "list"}); exit != ExitOK || !strings.Contains(out.String(), "* ctx_a") {
		t.Fatalf("list: %d %s %s", exit, out, stderr)
	}
	out.Reset()
	if exit := app.Run([]string{"context", "switch", "ctx_b", "--output", "json"}); exit != ExitOK {
		t.Fatalf("switch: %d %s %s", exit, out, stderr)
	}
	cfg, _ := app.loadConfig()
	if cfg.Profiles["default"].ContextID != "ctx_b" {
		t.Fatal("selection not saved")
	}
	if state.refreshes != 1 || state.requested[0] != "ctx_b" {
		t.Fatalf("refreshes=%v", state.requested)
	}
	out.Reset()
	if exit := app.Run([]string{"auth", "status", "--output", "json"}); exit != ExitOK || !strings.Contains(out.String(), `"context_id":"ctx_b"`) {
		t.Fatalf("status: %d %s", exit, out)
	}
}
func TestContextSwitchLiveRequiresExplicitChoice(t *testing.T) {
	app, out, stderr, _, _ := newContextTestApp(t)
	if exit := app.Run([]string{"context", "switch", "ctx_live", "--output", "json"}); exit != ExitUsage {
		t.Fatalf("live switch silently allowed: %d %s", exit, out)
	}
	out.Reset()
	if exit := app.Run([]string{"context", "switch", "ctx_live", "--live", "--output", "json"}); exit != ExitOK {
		t.Fatalf("switch: %d %s", exit, out)
	}
	out.Reset()
	if exit := app.Run([]string{"auth", "status", "--output", "json"}); exit != ExitOK || !strings.Contains(stderr.String(), "LIVE context:") {
		t.Fatalf("live status: %d %s %s", exit, out, stderr)
	}
	// Existing destructive confirmations still apply to a selected live context.
	auth := contextTestAuth("ctx_live")
	auth.SelectedContext = true
	if e := app.confirmCommand(&Command{Destructive: true}, auth, Options{NoInput: true}); e == nil || e.Code != "PRODUCTION_CONFIRMATION_REQUIRED" {
		t.Fatalf("confirmation=%v", e)
	}
}
func TestContextOverrideDoesNotChangeDefault(t *testing.T) {
	app, out, _, _, _ := newContextTestApp(t)
	if exit := app.Run([]string{"auth", "status", "--context", "ctx_b", "--output", "json"}); exit != ExitOK || !strings.Contains(out.String(), `"context_id":"ctx_b"`) {
		t.Fatalf("%d %s", exit, out)
	}
	cfg, _ := app.loadConfig()
	if cfg.Profiles["default"].ContextID != "ctx_a" {
		t.Fatal("one-shot override changed default")
	}
	out.Reset()
	if exit := app.Run([]string{"auth", "status", "--output", "json"}); exit != ExitOK || !strings.Contains(out.String(), `"context_id":"ctx_a"`) {
		t.Fatalf("%d %s", exit, out)
	}
}
func TestContextFrozenDuringRunningCommand(t *testing.T) {
	app, out, _, _, _ := newContextTestApp(t)
	auth := contextTestAuth("ctx_a")
	ctx := context.WithValue(context.Background(), oauthSessionKey{}, &oauthSession{Profile: "default", GrantID: auth.OAuthGrantID, SessionID: auth.OAuthSessionID, ContextID: auth.ContextID, MerchantID: auth.MerchantID, Environment: auth.Environment, SandboxID: auth.SandboxID})
	if exit := app.Run([]string{"context", "switch", "ctx_b", "--output", "json"}); exit != ExitOK {
		t.Fatalf("%d %s", exit, out)
	}
	key, e := app.oauthRequestToken(ctx, testOAuthAccess, appBase(t, app))
	if e != nil {
		t.Fatal(e)
	}
	got, e := app.fetchAuthContext(withoutOAuthSession(ctx), appBase(t, app), key, false)
	if e != nil || got.ContextID != "ctx_a" {
		t.Fatalf("running command changed context: %+v %v", got, e)
	}
	cfg, _ := app.loadConfig()
	if cfg.Profiles["default"].ContextID != "ctx_b" {
		t.Fatal("running command changed selected default")
	}
}
func appBase(t *testing.T, a *App) string {
	t.Helper()
	raw, _ := a.LoadCredential("default")
	c, e := decodeOAuthCredential(raw)
	if e != nil {
		t.Fatal(e)
	}
	return c.BaseURL
}
func TestContextDeniedPreservesSelection(t *testing.T) {
	app, out, _, state, stored := newContextTestApp(t)
	before := stored()
	state.deny = "ctx_b"
	if exit := app.Run([]string{"context", "switch", "ctx_b", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), "CONTEXT_ACCESS_DENIED") {
		t.Fatalf("%d %s", exit, out)
	}
	if stored() != before {
		t.Fatal("denied refresh changed credential")
	}
	cfg, _ := app.loadConfig()
	if cfg.Profiles["default"].ContextID != "ctx_a" {
		t.Fatal("denied switch changed selection")
	}
}
func TestContextRefreshOutageRetainsRotatedToken(t *testing.T) {
	app, _, _, state, stored := newContextTestApp(t)
	state.outage = true
	_, e := app.oauthAccess(withOAuthContext(context.Background(), "ctx_b"), "default")
	if e == nil {
		t.Fatal("expected unavailable context endpoint")
	}
	c, e := decodeOAuthCredential(stored())
	if e != nil || !c.PendingValidation || c.ContextID != "ctx_b" || state.revokes != 0 {
		t.Fatalf("staging failed: %+v %v", c, e)
	}
	state.outage = false
	c, e = app.oauthAccess(withOAuthContext(context.Background(), "ctx_b"), "default")
	if e != nil || c.Auth.ContextID != "ctx_b" || c.PendingValidation || state.refreshes != 1 {
		t.Fatalf("recovery failed: %+v %v", c, e)
	}
}
func TestContextRejectsSessionSubstitution(t *testing.T) {
	app, _, _, state, _ := newContextTestApp(t)
	state.wrongSession = true
	if _, e := app.oauthAccess(withOAuthContext(context.Background(), "ctx_b"), "default"); e == nil || e.Code != "OAUTH_CONTEXT_MISMATCH" {
		t.Fatalf("mismatch=%v", e)
	}
}
func TestContextInteractiveAndNoInput(t *testing.T) {
	app, out, _, _, _ := newContextTestApp(t)
	if exit := app.Run([]string{"context", "switch", "--output", "json"}); exit != ExitUsage {
		t.Fatalf("%d %s", exit, out)
	}
	app.IsTTY = func() bool { return true }
	app.Stdin = strings.NewReader("2\n")
	if exit := app.Run([]string{"context", "switch"}); exit != ExitOK {
		t.Fatalf("%d %s", exit, out)
	}
}
func TestContextLoginReusesSession(t *testing.T) {
	app, out, _, state, _ := newContextTestApp(t)
	if exit := app.Run([]string{"login", "--output", "json"}); exit != ExitOK || !strings.Contains(out.String(), "already_authenticated") || state.devices != 0 {
		t.Fatalf("%d %s", exit, out)
	}
}
func TestContextReauthorization(t *testing.T) {
	app, out, _, state, stored := newContextTestApp(t)
	if exit := app.Run([]string{"reauth", "--no-open", "--output", "json"}); exit != ExitOK {
		t.Fatalf("%d %s", exit, out)
	}
	c, e := decodeOAuthCredential(stored())
	if e != nil || c.Version != 2 || c.SessionID != "session_one" || state.devices != 1 || state.refreshes != 1 || state.revokes != 0 {
		t.Fatalf("reauth session: %+v %v", c, e)
	}
}
func TestContextLegacySessionStillRequiresLiveFlag(t *testing.T) {
	auth := contextTestAuth("ctx_live")
	auth.OAuthSessionID = ""
	auth.ContextID = ""
	if e := validateCredentialIntent(auth, Options{}, "", ""); e == nil || e.Code != "LIVE_ACKNOWLEDGEMENT_REQUIRED" {
		t.Fatalf("legacy check=%v", e)
	}
}

func TestContextProjectSelectionAndGuard(t *testing.T) {
	app, out, _, _, _ := newContextTestApp(t)
	project := filepath.Join(app.WorkingDir, ".flint")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "config.json")
	if err := os.WriteFile(path, []byte(`{"context":"ctx_b"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if exit := app.Run([]string{"auth", "status", "--output", "json"}); exit != ExitOK || !strings.Contains(out.String(), `"context_id":"ctx_b"`) {
		t.Fatalf("project selection: %d %s", exit, out)
	}
	out.Reset()
	if exit := app.Run([]string{"auth", "status", "--context", "ctx_a", "--output", "json"}); exit != ExitOK || !strings.Contains(out.String(), `"context_id":"ctx_a"`) {
		t.Fatalf("override: %d %s", exit, out)
	}
	if err := os.WriteFile(path, []byte(`{"context":"ctx_b","merchant":"mer_other"}`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if exit := app.Run([]string{"auth", "status", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), "MERCHANT_GUARD_MISMATCH") {
		t.Fatalf("guard: %d %s", exit, out)
	}
}
func TestContextConcurrentCommandsRemainIsolated(t *testing.T) {
	app, _, _, _, _ := newContextTestApp(t)
	var wg sync.WaitGroup
	for _, id := range []string{"ctx_a", "ctx_b", "ctx_live", "ctx_a", "ctx_b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			c, e := app.oauthAccess(withOAuthContext(context.Background(), id), "default")
			if e != nil || c.Auth.ContextID != id {
				t.Errorf("context %s got %s, %v", id, c.Auth.ContextID, e)
			}
		}(id)
	}
	wg.Wait()
	cfg, _ := app.loadConfig()
	if cfg.Profiles["default"].ContextID != "ctx_a" {
		t.Fatal("concurrent tokens changed default")
	}
}
func TestContextNewLoginAndReauthorizationOutage(t *testing.T) {
	for _, scenario := range []string{"new", "reauth-outage"} {
		t.Run(scenario, func(t *testing.T) {
			app, out, stderr, state, stored := newContextTestApp(t)
			args := []string{"login", "--new-session", "--no-open", "--output", "json"}
			if scenario == "new" {
				app.LoadCredential = func(string) (string, error) { return "", nil }
			} else {
				state.outage = true
				args = []string{"reauth", "--no-open", "--output", "json"}
			}
			exit := app.Run(args)
			if scenario == "new" && exit != ExitOK {
				t.Fatalf("%d %s %s", exit, out, stderr)
			}
			if scenario == "reauth-outage" {
				c, e := decodeOAuthCredential(stored())
				if exit == ExitOK || e != nil || !c.PendingValidation || c.RefreshToken == testOAuthRefresh || state.revokes != 0 {
					t.Fatalf("lost reauth token: exit=%d pending=%v err=%v", exit, c.PendingValidation, e)
				}
				state.outage = false
				if _, e := app.oauthAccess(withOAuthContext(context.Background(), "ctx_a"), "default"); e != nil {
					t.Fatal(e)
				}
			}
			for _, secret := range []string{testOAuthAccess, testOAuthRefresh, testDeviceSecret, "access-1", "refresh-1"} {
				if strings.Contains(out.String()+stderr.String(), secret) {
					t.Fatalf("secret leaked: %s", secret)
				}
			}
		})
	}
}
func TestContextSelectedHistoryScope(t *testing.T) {
	a := contextTestAuth("ctx_a")
	b := contextTestAuth("ctx_b")
	if oauthHistoryScope(a) == oauthHistoryScope(b) {
		t.Fatal("contexts share history")
	}
	b.OAuthGrantID = a.OAuthGrantID
	if oauthHistoryScope(a) == oauthHistoryScope(b) {
		t.Fatal("shared session grant conflates contexts")
	}
}

func TestContextNewLoginDoesNotReuseMalformedCredential(t *testing.T) {
	app, out, _, state, stored := newContextTestApp(t)
	if err := app.StoreCredential("default", `{"kind":"oauth","refresh_token":"unvalidated-token","base_url":"https://untrusted.example"}`); err != nil {
		t.Fatal(err)
	}
	if exit := app.Run([]string{"login", "--new-session", "--no-open", "--output", "json"}); exit != ExitOK {
		t.Fatalf("%d %s", exit, out)
	}
	c, e := decodeOAuthCredential(stored())
	if e != nil || c.Version != 2 || state.revokes != 0 {
		t.Fatalf("invalid replacement: %v revocations=%d", e, state.revokes)
	}
}
func TestContextStagingVerificationOrigin(t *testing.T) {
	link := "https://app.staging.withflintpay.com/cli/activate"
	if _, ok := loginVerificationURL(link, "", "https://api.staging.withflintpay.com", "ABCD-EFGH"); !ok {
		t.Fatal("staging website rejected for staging API")
	}
	if _, ok := loginVerificationURL(link, "", "https://api.withflintpay.com", "ABCD-EFGH"); ok {
		t.Fatal("production API accepted staging website")
	}
}
