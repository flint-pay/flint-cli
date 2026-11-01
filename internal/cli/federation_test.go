package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// All tokens and identities in these tests are synthetic, created in process.
func subjectJWT(aud any, expiry time.Time, sub string) string {
	raw, _ := json.Marshal(map[string]any{"iss": "https://token.actions.githubusercontent.com", "sub": sub, "aud": aud, "exp": expiry.Unix()})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(raw) + ".synthetic"
}

func federationApp(t *testing.T, base string) *App {
	t.Helper()
	app, _, _ := testApp(t, base)
	for _, name := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_OIDC_TOKEN", "FLINT_OIDC_TOKEN_FILE", "FLINT_OIDC_SOURCE", "FLINT_OIDC_SCOPE", "FLINT_OIDC_AUDIENCE", "GITHUB_ACTIONS", "ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "GITLAB_CI", "KUBERNETES_SERVICE_HOST", "K_SERVICE", "FUNCTION_TARGET", "GCE_METADATA_HOST", "IDENTITY_ENDPOINT", "IDENTITY_HEADER", "FLINT_AZURE_CLIENT_ID", "AZURE_CLIENT_ID", "ECS_CONTAINER_METADATA_URI", "ECS_CONTAINER_METADATA_URI_V4", "AWS_EXECUTION_ENV", "AWS_LAMBDA_FUNCTION_NAME", "AWS_WEB_IDENTITY_TOKEN_FILE", "AZURE_FEDERATED_TOKEN_FILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "wid_synthetic")
	t.Setenv("FLINT_OIDC_TOKEN", subjectJWT(base, time.Now().Add(time.Hour), "repo:example/project:ref:refs/heads/main"))
	app.LoadCredential = func(string) (string, error) { t.Error("federation must not read keyring"); return "", nil }
	return app
}

func writeExchange(w http.ResponseWriter, n int) {
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("fwat_synthetic_%d", n), "token_type": "Bearer", "issued_token_type": accessTokenType, "expires_in": 3600, "workload_identity_id": "wid_synthetic", "merchant_id": "mer_123", "mode": "test", "sandbox_id": "test_123"})
}

func workloadAuthJSON(mode string) string {
	return strings.Replace(strings.Replace(authContextJSON(mode), `"auth_type":"api_key"`, `"auth_type":"workload_identity","workload_identity_id":"wid_synthetic"`, 1), `"api_key_id":"key_123"`, `"api_key_id":""`, 1)
}

func TestFederationExchangeConcurrency(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/oauth/token" || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Authorization") != "" {
			t.Error("invalid exchange headers")
		}
		_ = r.ParseForm()
		expected := map[string]string{"grant_type": exchangeGrant, "subject_token_type": jwtTokenType, "requested_token_type": accessTokenType, "workload_identity_id": "wid_synthetic", "scope": "payments.customers.read"}
		for key, value := range expected {
			if key == "scope" && r.Form.Get(key) == "payments.customers.write" {
				continue
			}
			if r.Form.Get(key) != value {
				t.Errorf("incorrect %s", key)
			}
		}
		if len(r.Form) != 6 || r.Form.Get("subject_token") == "" {
			t.Error("exchange has missing or extra parameters")
		}
		writeExchange(w, int(exchanges.Add(1)))
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	t.Setenv("FLINT_OIDC_SCOPE", "payments.customers.read")
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			copy := *app
			token, e := copy.oauthRequestToken(context.Background(), workloadCredential, server.URL)
			if e != nil || token != "fwat_synthetic_1" {
				t.Errorf("token=%q err=%v", token, e)
			}
		}()
	}
	close(start)
	wg.Wait()
	if exchanges.Load() != 1 {
		t.Fatalf("exchanges=%d", exchanges.Load())
	}
	// Audience and scope partition the process cache.
	t.Setenv("FLINT_OIDC_SCOPE", "payments.customers.write")
	_, _ = app.workloadToken(context.Background(), server.URL, "")
	if exchanges.Load() != 2 {
		t.Fatal("scope must partition cache")
	}
}

func TestFederationFileRotationAndRefresh(t *testing.T) {
	subjects := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		subjects <- r.Form.Get("subject_token")
		writeExchange(w, len(subjects))
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	now := time.Now()
	app.Now = func() time.Time { return now }
	path := filepath.Join(t.TempDir(), "token")
	t.Setenv("FLINT_OIDC_TOKEN", "")
	t.Setenv("FLINT_OIDC_TOKEN_FILE", path)
	first := subjectJWT(server.URL, now.Add(time.Hour), "first")
	if err := os.WriteFile(path, []byte(first), 0600); err != nil {
		t.Fatal(err)
	}
	if _, e := app.workloadToken(context.Background(), server.URL, ""); e != nil {
		t.Fatal(e)
	}
	now = now.Add(47 * time.Minute)
	if _, e := app.workloadToken(context.Background(), server.URL, ""); e != nil || len(subjects) != 1 {
		t.Fatal("refreshed too early", e)
	}
	second := subjectJWT(server.URL, now.Add(time.Hour), "rotated")
	if err := os.WriteFile(path, []byte(second), 0600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, e := app.workloadToken(context.Background(), server.URL, ""); e != nil {
		t.Fatal(e)
	}
	if len(subjects) != 2 || <-subjects != first || <-subjects != second {
		t.Fatal("file rotation not read at refresh")
	}
}

func TestFederationAudienceAndExpiryRefusal(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Error("invalid subject sent to exchange")
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	for _, test := range []struct {
		aud    any
		expiry time.Time
		code   string
	}{
		{"https://foreign.example", time.Now().Add(time.Hour), "OIDC_AUDIENCE_MISMATCH"},
		{[]string{server.URL, "https://foreign.example"}, time.Now().Add(time.Hour), "OIDC_AUDIENCE_MISMATCH"},
		{server.URL, time.Now().Add(-time.Second), "OIDC_TOKEN_REQUEST_FAILED"},
	} {
		t.Setenv("FLINT_OIDC_TOKEN", subjectJWT(test.aud, test.expiry, "expected-subject"))
		_, e := app.workloadToken(context.Background(), server.URL, "")
		if e == nil || e.Code != test.code || !strings.Contains(e.Message, "expected-subject") || !strings.Contains(e.Message, "https://token.actions.githubusercontent.com") {
			t.Fatalf("error=%v", e)
		}
	}
	for _, malformed := range []string{"invalid-jwt", "header.invalid.signature", strings.Repeat("x", 8193)} {
		t.Setenv("FLINT_OIDC_TOKEN", malformed)
		if _, e := app.workloadToken(context.Background(), server.URL, ""); e == nil || e.Code != "OIDC_TOKEN_REQUEST_FAILED" {
			t.Fatal("malformed JWT accepted", e)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("subject leaked to exchange")
	}
}

func TestFederationGitHubSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeExchange(w, 1) }))
	defer server.Close()
	audience := server.URL
	var oidcRequests atomic.Int32
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oidcRequests.Add(1)
		if r.Method != "GET" || r.URL.Query().Get("audience") != audience || r.URL.Query().Get("existing") != "yes" || r.Header.Get("Authorization") != "Bearer synthetic-runner-credential" {
			t.Error("invalid GitHub OIDC request")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": subjectJWT(audience, time.Now().Add(time.Hour), "github-subject")})
	}))
	defer github.Close()

	app := federationApp(t, server.URL)
	t.Setenv("FLINT_OIDC_TOKEN", "")
	t.Setenv("GITHUB_ACTIONS", "true")
	_, e := app.workloadToken(context.Background(), server.URL, "")
	if e == nil || e.Code != "FEDERATION_SOURCE_UNAVAILABLE" || !strings.Contains(e.Message, "permissions: id-token: write") {
		t.Fatal(e)
	}
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", github.URL+"?existing=yes")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "synthetic-runner-credential")
	token, e := app.workloadToken(context.Background(), server.URL, "")
	if e != nil || !strings.HasPrefix(token, "fwat_") || oidcRequests.Load() != 1 {
		t.Fatalf("token=%q error=%v", token, e)
	}
}

func TestFederationSources(t *testing.T) {
	app := federationApp(t, "https://api.example.test/path")
	t.Setenv("FLINT_OIDC_TOKEN", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", filepath.Join(t.TempDir(), "must-not-read"))
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", filepath.Join(t.TempDir(), "must-not-read"))
	c, e := app.federationConfig("https://api.example.test/path")
	if e == nil || c.Audience != "https://api.example.test" {
		t.Fatal("expected unavailable source and origin audience", e)
	}
	for _, source := range []string{"none", "invalid"} {
		t.Setenv("FLINT_OIDC_SOURCE", source)
		_, e := app.federationConfig("https://api.example.test")
		if e == nil || e.Code != "FEDERATION_SOURCE_UNAVAILABLE" {
			t.Fatal(source, e)
		}
	}
	t.Setenv("FLINT_OIDC_SOURCE", "auto")
	t.Setenv("GITLAB_CI", "true")
	c, e = app.federationConfig("https://api.example.test")
	if e != nil || c.Source != "gitlab_ci" {
		t.Fatal(c, e)
	}
	_, e = c.subjectToken(context.Background())
	if e == nil || !strings.Contains(e.Message, "id_tokens:") || !strings.Contains(e.Message, "FLINT_OIDC_TOKEN:") {
		t.Fatal(e)
	}
	t.Setenv("FLINT_OIDC_TOKEN", subjectJWT(c.Audience, time.Now().Add(time.Hour), "gitlab"))
	c.Source = "gitlab_ci"
	if _, e := c.subjectToken(context.Background()); e != nil {
		t.Fatal(e)
	}
}

func TestFederationKubernetesSelection(t *testing.T) {
	app := federationApp(t, "https://api.example.test")
	t.Setenv("FLINT_OIDC_TOKEN", "")
	t.Run("auto_without_marker", func(t *testing.T) {
		t.Setenv("FLINT_OIDC_SOURCE", "auto")
		c, e := app.federationConfig("https://api.example.test")
		if c.Source != "auto" || e == nil || e.Code != "FEDERATION_SOURCE_UNAVAILABLE" || !strings.Contains(e.Message, "No source is available") {
			t.Fatalf("auto selected a source without a marker: source=%s error=%v", c.Source, e)
		}
	})
	t.Run("explicit_without_marker", func(t *testing.T) {
		t.Setenv("FLINT_OIDC_SOURCE", "kubernetes")
		path := filepath.Join(t.TempDir(), "projected")
		token := subjectJWT("https://api.example.test", time.Now().Add(time.Hour), "synthetic-kubernetes-subject")
		if err := os.WriteFile(path, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("FLINT_OIDC_TOKEN_FILE", path)
		c, e := app.federationConfig("https://api.example.test")
		if e != nil || c.Source != "kubernetes" {
			t.Fatalf("explicit Kubernetes selection failed: source=%s error=%v", c.Source, e)
		}
		if got, e := c.subjectToken(context.Background()); e != nil || got != token {
			t.Fatal("explicit Kubernetes did not read its projected token without a marker", e)
		}
	})
}

func TestFederationPrecedenceAndContexts(t *testing.T) {
	var exchanges atomic.Int32
	var seenBearer atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			exchanges.Add(1)
			writeExchange(w, 1)
			return
		}
		seenBearer.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, workloadAuthJSON("sandbox"))
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	if err := app.updateConfig(func(cfg *Config) error { cfg.Profiles["environment-only"] = profileWithoutCredential(); return nil }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLINT_PROFILE", "environment-only")
	t.Setenv("FLINT_ACCESS_TOKEN", "workload_identity")
	t.Setenv("FLINT_API_KEY", "flint_test_synthetic")
	if code := app.Run([]string{"auth", "status", "--output", "json"}); code != 0 {
		t.Fatal("access token lost precedence", code)
	}
	if seenBearer.Load().(string) != "Bearer workload_identity" || exchanges.Load() != 0 {
		t.Fatal("wrong chain precedence")
	}
	if !strings.Contains(app.Stderr.(*bytes.Buffer).String(), "FLINT_ACCESS_TOKEN takes precedence") {
		t.Fatal("missing precedence warning")
	}
	t.Setenv("FLINT_ACCESS_TOKEN", "")
	if code := app.Run([]string{"auth", "status", "--output", "json"}); code != 0 || seenBearer.Load().(string) != "Bearer flint_test_synthetic" || exchanges.Load() != 0 {
		t.Fatal("API key lost precedence")
	}
	t.Setenv("FLINT_API_KEY", "")
	if code := app.Run([]string{"auth", "status", "--output", "json"}); code != 0 || seenBearer.Load().(string) != "Bearer fwat_synthetic_1" || exchanges.Load() != 1 {
		t.Fatal("workload did not override keyring", code)
	}
	if code := app.Run([]string{"auth", "status", "--context", "ctx_explicit", "--output", "json"}); code != ExitAuth {
		t.Fatal("--context accepted", code)
	}
	t.Setenv("FLINT_PROFILE", "")
	if err := app.saveConfig(Config{DefaultProfile: "default", Profiles: map[string]Profile{"default": {ContextID: "ctx_saved"}}}); err != nil {
		t.Fatal(err)
	}
	resolved, _, err := app.resolveConfig(defaultOptions())
	if err != nil || resolved.ContextID != "" {
		t.Fatal("saved context not overridden", err)
	}
	t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
	app.LoadCredential = func(string) (string, error) { return "flint_test_saved", nil }
	if key, source, err := app.resolveCheckCredential("default"); err != nil || key != "flint_test_saved" || source != "keychain" {
		t.Fatal("saved credential fallback failed", err)
	}
	resolved, _, err = app.resolveConfig(defaultOptions())
	if err != nil || resolved.ContextID != "ctx_saved" {
		t.Fatal("saved context not restored", err)
	}
}

func TestFederationGuardsAnd401(t *testing.T) {
	var exchanges atomic.Int32
	var apiRequests atomic.Int32
	var mode atomic.Value
	mode.Store("live")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			writeExchange(w, int(exchanges.Add(1)))
			return
		}
		if r.URL.Path == "/v1/auth-context" {
			_, _ = io.WriteString(w, workloadAuthJSON(mode.Load().(string)))
			return
		}
		apiRequests.Add(1)
		if r.Header.Get("Authorization") == "Bearer fwat_synthetic_1" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"code":"TEST","remediation":{"retry":"same_request"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"auth", "status"}, ExitAuth},
		{[]string{"auth", "status", "--live"}, 0},
		{[]string{"auth", "status", "--live", "--merchant", "mer_wrong"}, ExitAuth},
	} {
		if code := app.Run(append(test.args, "--output", "json")); code != test.code {
			t.Fatalf("%v: %d", test.args, code)
		}
	}
	mode.Store("sandbox")
	key, base, env, e := app.authenticateCommand(context.Background(), &Command{AuthRequired: true}, defaultOptions(), ResolvedConfig{ProfileName: "default"})
	if e != nil {
		t.Fatal(e)
	}
	if e := validateCredentialIntent(env.Data, defaultOptions(), "mer_123", "test_wrong"); e == nil || e.Code != "SANDBOX_GUARD_MISMATCH" {
		t.Fatal(e)
	}
	if _, e := app.doRequest(context.Background(), base, key, "GET", "/v1/customers", nil, "", true); e != nil {
		t.Fatal(e)
	}
	if exchanges.Load() != 2 || apiRequests.Load() != 2 {
		t.Fatalf("exchanges %d API requests %d", exchanges.Load(), apiRequests.Load())
	}
	// A second 401 is returned without another refresh.
	always401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			writeExchange(w, int(exchanges.Add(1)))
			return
		}
		apiRequests.Add(1)
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"code":"TEST","remediation":{"retry":"same_request"}}}`)
	}))
	defer always401.Close()
	t.Setenv("FLINT_OIDC_TOKEN", subjectJWT(always401.URL, time.Now().Add(time.Hour), "second-server"))
	if _, e := app.doRequest(context.Background(), always401.URL, workloadCredential, "POST", "/v1/customers", []byte(`{}`), "saved-action", false); e == nil || e.HTTPStatus != 401 {
		t.Fatal(e)
	}
	if exchanges.Load() != 4 || apiRequests.Load() != 4 {
		t.Fatal("401 retry not bounded")
	}
}

func TestFederationExchangeRetryRedactionAndRedirects(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		token := r.Form.Get("subject_token")
		switch attempts.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		case 2:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Trust rejected " + token})
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	started := time.Now()
	_, e := app.workloadToken(context.Background(), server.URL, "")
	if time.Since(started) < time.Second {
		t.Fatal("Retry-After ignored")
	}
	if e == nil || e.Code != "FEDERATION_EXCHANGE_FAILED" || attempts.Load() != 3 || !strings.Contains(e.Message, "Trust rejected") || strings.Contains(e.Message, os.Getenv("FLINT_OIDC_TOKEN")) {
		t.Fatal(e)
	}
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	t.Setenv("FLINT_OIDC_TOKEN", subjectJWT(redirect.URL, time.Now().Add(time.Hour), "redirect"))
	if _, e := app.workloadToken(context.Background(), redirect.URL, ""); e == nil {
		t.Fatal("exchange redirect accepted")
	}
	t.Setenv("FLINT_OIDC_SOURCE", "github_actions")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", redirect.URL)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "synthetic-runner-credential")
	c, e := app.federationConfig(redirect.URL)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := c.subjectToken(context.Background()); e == nil {
		t.Fatal("GitHub redirect accepted")
	}
	if leaked.Load() != 0 {
		t.Fatal("redirect leaked credentials")
	}
	transport := federationHTTPClient.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("credential client uses proxies")
	}
}

func TestFederationRefreshMinimumAndAudienceOverride(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeExchange(w, int(exchanges.Add(1))) }))
	defer server.Close()
	app := federationApp(t, server.URL)
	now := time.Now().Truncate(time.Second)
	app.Now = func() time.Time { return now }
	t.Setenv("FLINT_OIDC_AUDIENCE", "https://merchant.example/oidc")
	t.Setenv("FLINT_OIDC_TOKEN", subjectJWT([]string{"https://merchant.example/oidc"}, now.Add(2*time.Minute), "short-lived"))
	if _, e := app.workloadToken(context.Background(), server.URL, ""); e != nil {
		t.Fatal(e)
	}
	now = now.Add(59 * time.Second)
	if _, e := app.workloadToken(context.Background(), server.URL, ""); e != nil || exchanges.Load() != 1 {
		t.Fatal("refreshed before 60s window", e)
	}
	now = now.Add(time.Second)
	if _, e := app.workloadToken(context.Background(), server.URL, ""); e != nil || exchanges.Load() != 2 {
		t.Fatal("did not refresh at 60s", e)
	}
	for _, base := range []string{"http://public.example", "https://api.example?unexpected=query", "https://user:pass@api.example"} {
		if _, e := app.workloadToken(context.Background(), base, ""); e == nil || e.Code != "INVALID_BASE_URL" {
			t.Fatal("unsafe exchange URL accepted", e)
		}
	}
}

func TestFederationFileAndAPIRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			_ = r.ParseForm()
			subject := r.Form.Get("subject_token")
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "File token rejected: " + subject})
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(403)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "Rejected " + bearer, "code": "INVALID_CREDENTIAL", "details": map[string]string{"echo": bearer}}, "meta": map[string]string{"trace_id": bearer}})
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	subject := subjectJWT(server.URL, time.Now().Add(time.Hour), "file-subject")
	file := filepath.Join(t.TempDir(), "subject")
	if err := os.WriteFile(file, []byte(subject), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLINT_OIDC_TOKEN", "")
	t.Setenv("FLINT_OIDC_TOKEN_FILE", file)
	_, e := app.workloadToken(context.Background(), server.URL, "")
	if e == nil || strings.Contains(e.Message, subject) || !strings.Contains(e.Message, "File token rejected") || !strings.Contains(e.Message, "file-subject") {
		t.Fatal(e)
	}
	// Seed a synthetic token through the cache; the API must redact echoed
	// credentials from its error envelope and debug metadata as well.
	c, e := app.federationConfig(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	federationCache.Lock()
	federationCache.tokens[c] = federationToken{Value: "fwat_synthetic_echo", Expires: time.Now().Add(time.Hour), Refresh: time.Now().Add(45 * time.Minute)}
	federationCache.Unlock()
	_, e = app.doRequest(context.Background(), server.URL, workloadCredential, "GET", "/v1/customers", nil, "", true)
	if e == nil {
		t.Fatal("expected API rejection")
	}
	if code := app.fail(e, Options{Output: "json"}); code != ExitAuth {
		t.Fatal(code)
	}
	if strings.Contains(app.Stdout.(*bytes.Buffer).String()+app.Stderr.(*bytes.Buffer).String(), "fwat_synthetic_echo") {
		t.Fatal("API token leaked")
	}
}

func TestFederationListen401(t *testing.T) {
	var exchanges atomic.Int32
	var streams atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			writeExchange(w, int(exchanges.Add(1)))
			return
		}
		streams.Add(1)
		if r.Header.Get("Authorization") == "Bearer fwat_synthetic_1" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: ready\ndata: {}\n\n")
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	response, _, e := app.openListenStream(context.Background(), server.URL, workloadCredential, "/v1/webhooks/events/stream", "", time.Second, false)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if exchanges.Load() != 2 || streams.Load() != 2 {
		t.Fatal("stream did not recover from 401")
	}
}

func TestFederationAuthContextIdentityAndHelp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			writeExchange(w, 1)
			return
		}
		_, _ = io.WriteString(w, strings.Replace(workloadAuthJSON("sandbox"), "wid_synthetic", "wid_other", 1))
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	if code := app.Run([]string{"auth", "status", "--output", "json"}); code != ExitAuth || !strings.Contains(app.Stdout.(*bytes.Buffer).String(), "FEDERATION_EXCHANGE_FAILED") {
		t.Fatal("mismatched workload identity accepted", code)
	}
	app.Stdout.(*bytes.Buffer).Reset()
	if code := app.Run([]string{"--help"}); code != 0 {
		t.Fatal(code)
	}
	for _, name := range []string{"FLINT_WORKLOAD_IDENTITY_ID", "FLINT_OIDC_SOURCE", "FLINT_OIDC_AUDIENCE", "FLINT_OIDC_TOKEN", "FLINT_OIDC_TOKEN_FILE", "FLINT_OIDC_SCOPE"} {
		if !strings.Contains(app.Stdout.(*bytes.Buffer).String(), name) {
			t.Error("missing help", name)
		}
	}
}

func TestFederationClientDryRunIsOffline(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Error("client dry-run made a network request")
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	if code := app.Run([]string{"customers", "create", "--name", "Ada", "--dry-run=client", "--output", "json"}); code != 0 {
		t.Fatal(code, app.Stdout.(*bytes.Buffer).String())
	}
	if requests.Load() != 0 {
		t.Fatal("client dry-run requested credentials")
	}
	if code := app.Run([]string{"signup", "--email", "synthetic@example.test", "--no-input", "--output", "json"}); code != ExitAuth || requests.Load() != 0 {
		t.Fatal("signup ignored workload identity override", code)
	}
}

func TestFederationShortLifetimeCache(t *testing.T) {
	for _, lifetime := range []time.Duration{31 * time.Second, 45 * time.Second, 60 * time.Second} {
		for _, limitedBy := range []string{"access", "subject"} {
			t.Run(fmt.Sprintf("%s/%s", lifetime, limitedBy), func(t *testing.T) {
				var exchanges atomic.Int32
				now := time.Now().Truncate(time.Second)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					exchanges.Add(1)
					expiresIn := int64(3600)
					if limitedBy == "access" {
						expiresIn = int64(lifetime / time.Second)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fwat_synthetic_short", "token_type": "Bearer", "issued_token_type": accessTokenType, "expires_in": expiresIn, "workload_identity_id": "wid_synthetic"})
				}))
				defer server.Close()
				app := federationApp(t, server.URL)
				app.Now = func() time.Time { return now }
				if limitedBy == "subject" {
					t.Setenv("FLINT_OIDC_TOKEN", subjectJWT(server.URL, now.Add(lifetime), "short-lived"))
				}
				lookup := func() {
					t.Helper()
					token, e := app.workloadToken(context.Background(), server.URL, "")
					if e != nil || token != "fwat_synthetic_short" {
						t.Errorf("token=%q err=%v", token, e)
					}
				}
				// Sequential calls must reuse the completed exchange.
				for range 3 {
					lookup()
				}
				if exchanges.Load() != 1 {
					t.Fatalf("sequential exchanges=%d, want 1", exchanges.Load())
				}
				// Clear the cache to exercise concurrent initial acquisition and
				// callers that arrive after the initial flight has completed.
				c, e := app.federationConfig(server.URL)
				if e != nil {
					t.Fatal(e)
				}
				federationCache.Lock()
				delete(federationCache.tokens, c)
				federationCache.Unlock()
				exchanges.Store(0)
				var wg sync.WaitGroup
				start := make(chan struct{})
				for range 100 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						lookup()
					}()
				}
				close(start)
				wg.Wait()
				if exchanges.Load() != 1 {
					t.Fatalf("concurrent exchanges=%d, want 1", exchanges.Load())
				}
				federationCache.Lock()
				cached := federationCache.tokens[c]
				federationCache.Unlock()
				if !cached.Refresh.After(now) || cached.Refresh.After(cached.Expires.Add(-30*time.Second)) {
					t.Fatalf("refresh=%v outside usable lifetime", cached.Refresh.Sub(now))
				}
				// The cached token must stop being reused by the safety margin.
				now = cached.Expires.Add(-30 * time.Second)
				lookup()
				if exchanges.Load() != 2 {
					t.Fatal("token reused inside expiry safety margin")
				}
			})
		}
	}
}

func TestFederationForeignTokenFiles(t *testing.T) {
	for _, source := range []string{"token_file", "kubernetes"} {
		for _, foreign := range []string{"AWS_WEB_IDENTITY_TOKEN_FILE", "AZURE_FEDERATED_TOKEN_FILE", "kubernetes-default"} {
			for _, variant := range []string{"direct", "cleaned-relative", "symlink", "foreign-symlink", "missing"} {
				if foreign == "kubernetes-default" && variant != "direct" {
					continue
				}
				t.Run(source+"/"+foreign+"/"+variant, func(t *testing.T) {
					federationApp(t, "https://api.example.test")
					t.Setenv("KUBERNETES_SERVICE_HOST", "cluster.example")
					path := filepath.Join(t.TempDir(), "foreign-token")
					if foreign == "kubernetes-default" {
						path = "/var/run/secrets/kubernetes.io/serviceaccount/token"
					} else if variant != "missing" {
						if err := os.WriteFile(path, []byte("synthetic-foreign-token"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					configured, denied := path, path
					switch variant {
					case "cleaned-relative":
						// Run from the token's directory: on Windows the temp directory
						// can be on a different volume than the checkout.
						t.Chdir(filepath.Dir(path))
						configured = "unused/../" + filepath.Base(path)
					case "symlink", "foreign-symlink":
						link := filepath.Join(t.TempDir(), "alias")
						if err := os.Symlink(path, link); err != nil {
							t.Fatal(err)
						}
						if variant == "symlink" {
							configured = link
						} else {
							denied = link
						}
					}
					if foreign != "kubernetes-default" {
						t.Setenv(foreign, denied)
					}
					c := federationConfig{Source: source, File: configured}
					token, e := c.subjectToken(context.Background())
					if e == nil || e.Code != "FEDERATION_SOURCE_UNAVAILABLE" || !strings.Contains(e.Message, "Refusing to read") || token != "" {
						t.Fatalf("foreign file accepted or wrong error: token=%q err=%v", token, e)
					}
					if foreign != "kubernetes-default" && !strings.Contains(e.Message, foreign) {
						t.Fatalf("error does not identify foreign source: %v", e)
					}
				})
			}
		}
	}
}
