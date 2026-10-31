package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	apispec "github.com/flint-pay/flint-cli/internal/spec"
	"github.com/gofrs/flock"
)

func TestDXKeyringTimeoutAndRemediation(t *testing.T) {
	previous := keyringTimeout
	keyringTimeout = 10 * time.Millisecond
	t.Cleanup(func() { keyringTimeout = previous })
	release := make(chan struct{})
	defer close(release)
	started := time.Now()
	value, err := withKeyringTimeout(func() (string, error) { <-release; return "late", nil })
	if value != "" || !errors.Is(err, errKeyringTimeout) || time.Since(started) > time.Second {
		t.Fatalf("value=%q err=%v", value, err)
	}
	want := errors.New("D-Bus unavailable")
	if value, err := withKeyringTimeout(func() (string, error) { return "", want }); !errors.Is(err, want) || value != "" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	if value, err := withKeyringTimeout(func() (string, error) { return "saved", nil }); err != nil || value != "saved" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	app, out, _ := testApp(t, "")
	opts := defaultOptions()
	opts.Output = "json"
	app.writeError(credentialStoreError("CREDENTIAL_LOOKUP_FAILED", "Could not read credential.", want), opts)
	var result map[string]any
	if json.Unmarshal(out.Bytes(), &result) != nil {
		t.Fatal(out.String())
	}
	for _, path := range []string{"error.remediation.next_actions", "error.details.remediation.next_actions"} {
		if _, ok := lookupPath(result, path); !ok {
			t.Fatalf("missing %s: %s", path, out)
		}
	}
	t.Setenv("FLINT_API_KEY", "")
	app.LoadCredential = func(string) (string, error) { return "", want }
	out.Reset()
	if exit := app.Run([]string{"doctor", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), "FLINT_API_KEY") {
		t.Fatalf("exit=%d output=%s", exit, out)
	}
	app.LoadCredential = func(string) (string, error) { return "", nil }
	for _, args := range [][]string{{"doctor", "--output", "json"}, {"init", "--output", "json"}} {
		out.Reset()
		app.Run(args)
		for _, want := range []string{"FLINT_API_KEY", "flint login", "flint signup"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %s: %s", want, out)
			}
		}
	}
}

func TestDXHelpAndSchemaShapes(t *testing.T) {
	app, out, _ := testApp(t, "")
	for _, argv := range [][]string{{"--output", "json"}, {"--help", "--output", "json"}, {"help", "--output", "json"}} {
		out.Reset()
		if exit := app.Run(argv); exit != ExitOK || out.Len() > 25000 {
			t.Fatalf("argv=%v exit=%d size=%d output=%s", argv, exit, out.Len(), out)
		}
		for _, want := range []string{"start_here", "sections", "flint schema commands --output json"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %s: %s", want, out)
			}
		}
	}
	t.Setenv("FLINT_OUTPUT", "JSON")
	out.Reset()
	if exit := app.Run(nil); exit != ExitOK || out.Len() > 25000 || !json.Valid(out.Bytes()) {
		t.Fatalf("exit=%d output=%s", exit, out)
	}
	t.Setenv("FLINT_OUTPUT", "")
	for _, topic := range []string{"errors", "events", "exit-codes", "test-cards"} {
		exit, result := runJSON(t, app, out, "help", topic)
		if exit != ExitOK || result["data"] == nil {
			t.Fatalf("topic=%s exit=%d result=%v", topic, exit, result)
		}
	}
	_, detail := runJSON(t, app, out, "help", "events", "customer.created")
	if typ, _ := lookupPath(detail, "data.type"); typ != "customer.created" {
		t.Fatal(detail)
	}
	out.Reset()
	if exit := app.Run([]string{"help", "events"}); exit != ExitOK || out.Len() > 50000 || !strings.Contains(out.String(), "customer.created") {
		t.Fatalf("exit=%d size=%d", exit, out.Len())
	}
	_, schema := runJSON(t, app, out, "schema", "command", "customers.get")
	if expansions, _ := lookupPath(schema, "data.expansions"); !strings.Contains(fmt.Sprint(expansions), "default_payment_method") {
		t.Fatal(schema)
	}
	for _, kind := range []string{"input", "output", "command"} {
		out.Reset()
		if exit := app.Run([]string{"schema", kind, "customers.create"}); exit != ExitOK || !strings.Contains(out.String(), "\n  ") {
			t.Fatalf("kind=%s exit=%d output=%s", kind, exit, out)
		}
	}
	out.Reset()
	app.Run([]string{"customers", "create", "--help"})
	for _, want := range []string{"Request body:", "--input takes FILE.json, - for stdin, or inline JSON"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, out)
		}
	}
	out.Reset()
	app.Run([]string{"customers", "get", "--help"})
	if !strings.Contains(out.String(), "--expand default_payment_method") {
		t.Fatal(out.String())
	}
	out.Reset()
	app.Run([]string{"--help"})
	if strings.Contains(out.String(), "--color") || strings.Contains(out.String(), "FLINT_COLOR") || !strings.Contains(out.String(), "70 internal error") {
		t.Fatal(out.String())
	}
}

func TestDXEnvironmentOptions(t *testing.T) {
	for _, value := range []string{"1", "TRUE", "YeS", "ON"} {
		t.Setenv("FLINT_NO_INPUT", value)
		opts := defaultOptions()
		applyEnvironmentOptions(&opts)
		if !opts.NoInput {
			t.Fatalf("FLINT_NO_INPUT=%s", value)
		}
	}
	app, out, _ := testApp(t, "")
	t.Setenv("FLINT_OUTPUT", "NDJSON")
	if exit := app.Run([]string{"version"}); exit != ExitOK || !json.Valid(out.Bytes()) {
		t.Fatalf("exit=%d output=%s", exit, out)
	}
	out.Reset()
	if exit := app.Run([]string{"version", "--output", "ndjson"}); exit != ExitUsage {
		t.Fatalf("exit=%d output=%s", exit, out)
	}
	t.Setenv("FLINT_OUTPUT", "invalid")
	out.Reset()
	if exit := app.Run([]string{"version"}); exit != ExitUsage {
		t.Fatal(exit)
	}
	// Invalid output defaults to a human error and must identify its origin.
	_, stderr := app.Stdout, app.Stderr
	if !strings.Contains(stderr.(*bytes.Buffer).String(), "FLINT_OUTPUT") {
		t.Fatal(stderr)
	}
}

func TestDXParserAndRequestConveniences(t *testing.T) {
	registry := NewRegistry()
	for _, argv := range [][]string{{"customers", "create", "--confirm", "true"}, {"customers", "get", "cus_123", "--live", "false"}, {"version", "--color", "anything"}} {
		if _, _, _, err := parseInvocation(registry, argv); err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
	}
	_, options, _, err := parseInvocation(registry, []string{"customers", "get", "--quiet", "cus_123"})
	if err != nil || len(options.Positionals) != 1 || options.Positionals[0] != "cus_123" {
		t.Fatalf("options=%v err=%v", options, err)
	}
	app, out, _ := testApp(t, "http://127.0.0.1:1")
	for _, alias := range []string{"data", "body", "json-body"} {
		exit, result := runJSON(t, app, out, "customers", "create", "--"+alias, "{}")
		if exit != ExitUsage || !strings.Contains(fmt.Sprint(result), "--input") {
			t.Fatalf("exit=%d result=%v", exit, result)
		}
	}
	exit, result := runJSON(t, app, out, "customers", "create", "--input", `  {"email":"a@example.com"}`, "--dry-run=client", "--confirm")
	if email, _ := lookupPath(result, "data.body.email"); exit != ExitOK || email != "a@example.com" {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	exit, result = runJSON(t, app, out, "customers", "get", " pi_123 ", "--wait-for", "email=null")
	if exit != ExitUsage || !strings.Contains(fmt.Sprint(result), "cus_") {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	exit, result = runJSON(t, app, out, "customers", "create", "--email", "--bad")
	if exit != ExitUsage || !strings.Contains(fmt.Sprint(result), "--email=VALUE") {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	exit, result = runJSON(t, app, out, "payment-links", "create", "--name", "test", "--item-name", "item", "--amount", "100", "--currency", "usd", "--dry-run=client")
	if currency, _ := lookupPath(result, "data.body.line_items.0.unit_price_money.currency"); exit != ExitOK || currency != "USD" {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	exit, result = runJSON(t, app, out, "payment-links", "create", "--name", "test", "--item-name", "item", "--amount", "100", "--currency", "bogus", "--dry-run=client")
	if param, _ := lookupPath(result, "error.param"); exit != ExitUsage || param != "currency" || !strings.Contains(fmt.Sprint(result), "$.line_items[0].unit_price_money.currency (from --currency)") {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
}

func TestDXTimesAndNullWait(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for value, expected := range map[string]string{"7d": "2026-10-14T12:00:00Z", "2w": "2026-10-07T12:00:00Z", "2026-10-20": "2026-10-20T00:00:00Z"} {
		if actual, err := resolveTime(value, now); err != nil || actual != expected {
			t.Fatalf("value=%s actual=%s err=%v", value, actual, err)
		}
	}
	if ok, observed := conditionsMatch(map[string]any{"data": map[string]any{"deleted_at": nil}}, []waitCondition{{"deleted_at", "null"}}); !ok || observed["deleted_at"] != "null" {
		t.Fatalf("ok=%v observed=%v", ok, observed)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		fmt.Fprint(w, `{"data":{"payment_intent_id":"pi_123","status":"canceled"}}`)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	exit, result := runJSON(t, app, out, "payment-intents", "get", "pi_123", "--wait-for", "status=succeeded")
	if code, _ := lookupPath(result, "error.code"); exit != ExitWait || code != "WAIT_CONDITION_IMPOSSIBLE" {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	for _, path := range []string{"error.details.conditions", "error.details.observed", "error.details.last"} {
		if _, ok := lookupPath(result, path); !ok {
			t.Fatalf("missing %s: %v", path, result)
		}
	}
}

func TestDXErrorsPreserveAPIContext(t *testing.T) {
	for _, status := range []int{403, 429, 500, 501, 502, 503, 504, 507} {
		app, out, _ := testApp(t, "")
		raw := map[string]any{"type": "api_error", "code": "TEST", "message": "test", "details": map[string]any{"original": "kept"}, "remediation": map[string]any{"next_actions": []any{map[string]any{"command": "existing"}}}}
		e := apiErrorFromResponse(&apiResponse{Status: status, Headers: http.Header{}, Value: map[string]any{"error": raw}})
		e = withIdempotencyRetryContext(e, "idem_test")
		options := defaultOptions()
		options.Output = "json"
		app.writeError(e, options)
		var result map[string]any
		json.Unmarshal(out.Bytes(), &result)
		if actual, _ := lookupPath(result, "error.http_status"); actual != float64(status) {
			t.Fatal(result)
		}
		if actual, _ := lookupPath(result, "error.retryable"); actual != false {
			t.Fatal(result)
		}
		if original, _ := lookupPath(result, "error.details.original"); original != "kept" {
			t.Fatal(result)
		}
		if key, _ := lookupPath(result, "error.details.idempotency_key"); key != "idem_test" {
			t.Fatal(result)
		}
		actions, _ := lookupPath(result, "error.remediation.next_actions")
		if len(actions.([]any)) != 1 || actions.([]any)[0].(map[string]any)["command"] != "existing" {
			t.Fatal(result)
		}
		if status == 403 && e.ExitCode != ExitAuth {
			t.Fatal(e)
		}
	}
	for _, argv := range [][]string{{"completely-unrecognized"}, {"version", "--totally-unrecognized"}} {
		app, out, _ := testApp(t, "")
		_, result := runJSON(t, app, out, argv...)
		suggestions, ok := lookupPath(result, "error.details.suggestions")
		if !ok || suggestions == nil {
			t.Fatal(result)
		}
	}
}

func TestDXProjectionOptionalFields(t *testing.T) {
	app, out, _ := testApp(t, "")
	command, _ := app.Registry.ByName("customers.list")
	value := map[string]any{"data": []any{map[string]any{"customer_id": "cus_1", "email": "a@example.com"}, map[string]any{"customer_id": "cus_2"}}}
	options := defaultOptions()
	options.Output = "json"
	options.Select = []string{"email"}
	if e := app.writeResult(value, command, options); e != nil {
		t.Fatal(e)
	}
	var result map[string]any
	json.Unmarshal(out.Bytes(), &result)
	if actual, ok := lookupPath(result, "data.1.email"); !ok || actual != nil {
		t.Fatal(result)
	}
	for _, resources := range [][]any{{map[string]any{"customer_id": "cus_1"}}, {}} {
		out.Reset()
		value["data"] = resources
		if e := app.writeResult(value, command, options); e != nil {
			t.Fatal(e)
		}
	}
	options.Select = []string{"emial"}
	if e := app.writeResult(value, command, options); e == nil {
		t.Fatal("accepted unknown schema field on empty list")
	}
	command, _ = app.Registry.ByName("customers.get")
	options.Select = nil
	options.Field = "email"
	out.Reset()
	if e := app.writeResult(map[string]any{"data": map[string]any{"customer_id": "cus_1"}}, command, options); e != nil || strings.TrimSpace(out.String()) != "null" {
		t.Fatalf("error=%v output=%s", e, out)
	}
}

func TestDXPaginationLimitAndHistory(t *testing.T) {
	for _, fixture := range []struct {
		limit int
		pages int
		token string
	}{{2, 1, ""}, {3, 1, "p2"}, {5, 2, ""}, {6, 2, "p3"}, {8, 3, ""}} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/auth-context" {
				fmt.Fprint(w, authContextJSON("sandbox"))
				return
			}
			calls++
			if r.URL.Query().Get("page_size") != fmt.Sprint(fixture.limit-(calls-1)*3) {
				t.Error(r.URL.String())
			}
			data := []any{}
			for i := 0; i < 3; i++ {
				data = append(data, map[string]any{"customer_id": fmt.Sprintf("cus_%d_%d", calls, i)})
			}
			response := map[string]any{"data": data}
			if calls < 3 {
				response["next_page_token"] = fmt.Sprintf("p%d", calls+1)
			}
			writeJSON(w, response)
		}))
		app, out, _ := testApp(t, server.URL)
		exit, result := runJSON(t, app, out, "customers", "list", "--limit", fmt.Sprint(fixture.limit))
		if exit != ExitOK || calls != fixture.pages || len(result["data"].([]any)) != fixture.limit {
			t.Fatalf("fixture=%v calls=%d exit=%d result=%v", fixture, calls, exit, result)
		}
		token, _ := result["next_page_token"].(string)
		if token != fixture.token {
			t.Fatalf("fixture=%v token=%s", fixture, token)
		}
		history, err := app.loadHistory()
		if err != nil || len(history.Entries) != 0 {
			t.Fatalf("GET recorded history: %v %v", history, err)
		}
		server.Close()
	}
	app, out, _ := testApp(t, "")
	_, result := runJSON(t, app, out, "history")
	if data, ok := result["data"].([]any); !ok || len(data) != 0 {
		t.Fatal(result)
	}
}

func TestDXConfigCompatibilityAndSources(t *testing.T) {
	app, _, _ := testApp(t, "")
	path, _ := app.configPath()
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.loadConfig(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLINT_API_KEY", "")
	if err := os.MkdirAll(filepath.Join(app.WorkingDir, ".flint"), 0700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(app.WorkingDir, ".flint", "config.json")
	os.WriteFile(project, []byte(`{"profile":"new"}`), 0600)
	if _, _, err := app.resolveConfig(defaultOptions()); err == nil || !strings.Contains(err.Error(), project) || !strings.Contains(err.Error(), "flint login --profile new") {
		t.Fatalf("err=%v", err)
	}
	t.Setenv("FLINT_PROFILE", "from-env")
	if _, _, err := app.resolveConfig(defaultOptions()); err == nil || !strings.Contains(err.Error(), "FLINT_PROFILE") {
		t.Fatalf("err=%v", err)
	}
	options := defaultOptions()
	options.Profile = "from-flag"
	if _, _, err := app.resolveConfig(options); err == nil || !strings.Contains(err.Error(), "--profile") {
		t.Fatalf("err=%v", err)
	}
	t.Setenv("FLINT_API_KEY", "flint_test_test")
	if resolved, _, err := app.resolveConfig(options); err == nil {
		t.Fatalf("resolved=%v err=%v", resolved, err)
	}
	t.Setenv("FLINT_PROFILE", "")
	os.Remove(project)
	if exit := app.Run([]string{"config", "set", "merchant", "mer_guard"}); exit != ExitOK {
		t.Fatal(exit)
	}
	if exit := app.Run([]string{"config", "unset", "merchant"}); exit != ExitOK {
		t.Fatal(exit)
	}
	cfg, _ := app.loadConfig()
	if cfg.Profiles["default"].MerchantGuard != "" {
		t.Fatal(cfg)
	}
	for source, want := range map[string]string{"flag": "--merchant", "environment": "FLINT_MERCHANT", "profile": "profile", "project": project} {
		resolved := ResolvedConfig{MerchantGuard: "mer_expected", ProjectConfigPath: project, Sources: map[string]string{"merchant_guard": source}}
		e := validateResolvedCredentialIntent(AuthContext{Environment: "sandbox", MerchantID: "mer_actual"}, defaultOptions(), resolved, "")
		if e == nil || !strings.Contains(e.Message, want) {
			t.Fatalf("source=%s error=%v", source, e)
		}
	}
}

func TestDXAuthContextCache(t *testing.T) {
	authCalls := 0
	status := 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			authCalls++
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, `{"data":{}}`)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	now := time.Now()
	app.Now = func() time.Time { return now }
	key := "flint_test_cache_secret"
	for i := 0; i < 2; i++ {
		if _, e := app.fetchAuthContextResponse(context.Background(), server.URL, key, false); e != nil {
			t.Fatal(e)
		}
	}
	if authCalls != 1 {
		t.Fatal(authCalls)
	}
	path := app.authContextCachePath(server.URL, key)
	raw, err := os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte(key)) {
		t.Fatalf("cache error=%v contains key=%v", err, bytes.Contains(raw, []byte(key)))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	authContextCache.Delete(path)
	if _, e := app.fetchAuthContextResponse(context.Background(), server.URL, key, false); e != nil || authCalls != 1 {
		t.Fatalf("disk cache calls=%d error=%v", authCalls, e)
	}
	now = now.Add(5 * time.Minute)
	app.fetchAuthContextResponse(context.Background(), server.URL, key, false)
	if authCalls != 2 {
		t.Fatal(authCalls)
	}
	for _, code := range []int{401, 403} {
		status = code
		app.doRequest(context.Background(), server.URL, key, "GET", "/v1/customers", nil, "", false)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("cache not invalidated: %v", err)
		}
		app.fetchAuthContextResponse(context.Background(), server.URL, key, false)
	}
	if authCalls != 4 {
		t.Fatal(authCalls)
	}
	for i := 0; i < 2; i++ {
		app.fetchAuthContextResponse(context.Background(), server.URL, "oauth-access", false)
	}
	if authCalls != 6 {
		t.Fatal("OAuth unexpectedly cached")
	}
}

func TestDXLoginNoWaitResume(t *testing.T) {
	devices, polls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loginDevicePath:
			devices++
			writeTestDevice(w)
		case loginTokenPath:
			polls++
			writeTestTokens(w, testOAuthAccess, testOAuthRefresh)
		case "/v1/auth-context":
			fmt.Fprint(w, oauthAuthJSON("sandbox"))
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	app.OpenBrowser = func(string) error { t.Error("opened browser"); return nil }
	exit, result := runJSON(t, app, out, "login", "--no-wait", "--no-input", "--profile", "new")
	if status, _ := lookupPath(result, "data.status"); exit != ExitOK || status != "approval_pending" || devices != 1 || polls != 0 {
		t.Fatalf("exit=%d result=%v devices=%d polls=%d", exit, result, devices, polls)
	}
	path, _ := app.pendingLoginPath("new")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("pending file err=%v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("pending file mode=%v", info.Mode())
	}
	cfg, _ := app.loadConfig()
	if cfg.DefaultProfile != "default" {
		t.Fatal(cfg)
	}
	out.Reset()
	if exit := app.Run([]string{"login", "--no-wait", "--no-input", "--profile", "new"}); exit != ExitOK || !json.Valid(out.Bytes()) || devices != 1 || polls != 0 {
		t.Fatalf("exit=%d output=%s devices=%d polls=%d", exit, out, devices, polls)
	}
	stored := ""
	app.StoreCredential = func(profile, key string) error {
		if profile != "new" {
			t.Error(profile)
		}
		stored = key
		return nil
	}
	stderr.Reset()
	exit, result = runJSON(t, app, out, "login", "--profile", "new", "--no-input")
	if exit != ExitOK || devices != 1 || polls != 1 || stored == "" {
		t.Fatalf("exit=%d result=%v devices=%d polls=%d", exit, result, devices, polls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var record map[string]any
	if json.Unmarshal(stderr.Bytes(), &record) != nil || record["type"] != "login_pending" || record["verification_url"] == nil {
		t.Fatal(stderr.String())
	}
	cfg, _ = app.loadConfig()
	if cfg.DefaultProfile != "default" {
		t.Fatal(cfg)
	}
	if _, ok := cfg.Profiles["new"]; !ok {
		t.Fatal(cfg)
	}
}

func TestDXLockTimeoutAndRenderer(t *testing.T) {
	if localFileLockTimeout <= 30*time.Second {
		t.Fatal(localFileLockTimeout)
	}
	previous := localFileLockTimeout
	localFileLockTimeout = 20 * time.Millisecond
	t.Cleanup(func() { localFileLockTimeout = previous })
	path := filepath.Join(t.TempDir(), "config.json")
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	err := withLocalFileLock(context.Background(), path, func() error { t.Error("ran without lock"); return nil })
	e := configError("OAUTH_REFRESH_FAILED", "failed", err)
	if !errors.Is(err, errLocalFileLockTimeout) || e.ExitCode != ExitNetwork || e.Code != "CONFIG_LOCK_TIMEOUT" {
		t.Fatalf("err=%v e=%v", err, e)
	}
	var out bytes.Buffer
	renderHumanValue(&out, map[string]any{"nested": map[string]any{"url": "https://example.test", "values": []any{"one", "two"}, "empty": map[string]any{}}}, "", time.Now(), 0)
	for _, want := range []string{"  URL", "one, two", "(none)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(out.String())
		}
	}
	out.Reset()
	renderHumanValue(&out, map[string]any{"x": true}, "", time.Now(), 13)
	if !strings.Contains(out.String(), "…") {
		t.Fatal(out.String())
	}
}

func TestDXEmbeddedMetadataMatchesSpec(t *testing.T) {
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if apispec.APIVersion() != doc.FlintAPIVersion || !sameScopeSet(apispec.BootstrapScopes(), doc.FlintCLIScopes) {
		t.Fatal("metadata extraction differs from full snapshot")
	}
	app := New(BuildInfo{})
	app.Stdout = io.Discard
	app.Stderr = io.Discard
	if !app.Registry.localOnly {
		t.Fatal("New eagerly loaded API registry")
	}
	if exit := app.Run([]string{"version"}); exit != ExitOK || !app.Registry.localOnly {
		t.Fatalf("version loaded API registry: exit=%d", exit)
	}
}

func BenchmarkDXRegistry(b *testing.B) {
	for b.Loop() {
		NewRegistry()
	}
}
func BenchmarkDXLocalStartup(b *testing.B) {
	for b.Loop() {
		app := New(BuildInfo{})
		app.Stdout = io.Discard
		app.Stderr = io.Discard
		app.Run([]string{"version"})
	}
}

func TestDXURLAndProfileAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, authContextJSON("sandbox")) }))
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_BASE_URL", server.URL+"/v1")
	exit, result := runJSON(t, app, out, "customers", "create", "--email", "a@example.com", "--dry-run=client")
	if exit != ExitOK || !strings.Contains(stderr.String(), "stripped trailing /v1") {
		t.Fatalf("exit=%d result=%v stderr=%s", exit, result, stderr)
	}
	stderr.Reset()
	if exit, _ := runJSON(t, app, out, "customers", "create", "--email", "a@example.com", "--dry-run=client", "--quiet"); exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	t.Setenv("FLINT_BASE_URL", "not-a-url")
	exit, result = runJSON(t, app, out, "customers", "list")
	if code, _ := lookupPath(result, "error.code"); exit != ExitAuth || code != "INVALID_BASE_URL" {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	t.Setenv("FLINT_BASE_URL", server.URL)
	t.Setenv("FLINT_API_KEY", "sk_test_not_flint")
	exit, result = runJSON(t, app, out, "customers", "list")
	if code, _ := lookupPath(result, "error.code"); exit != ExitAuth || code != "CREDENTIAL_FORMAT_INVALID" || !strings.Contains(fmt.Sprint(result), "FLINT_API_KEY") {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	t.Setenv("FLINT_API_KEY", "")
	app.Stdin = strings.NewReader("flint_test_new\n")
	saved := ""
	app.StoreCredential = func(profile, key string) error { saved = profile; return nil }
	exit, result = runJSON(t, app, out, "auth", "import", "--stdin", "--profile", "created")
	if exit != ExitOK || saved != "created" {
		t.Fatalf("exit=%d result=%v saved=%s", exit, result, saved)
	}
	cfg, _ := app.loadConfig()
	if cfg.DefaultProfile != "default" {
		t.Fatal(cfg)
	}
	if _, ok := cfg.Profiles["created"]; !ok {
		t.Fatal(cfg)
	}
}

func TestDXSignupUsageAndOverride(t *testing.T) {
	app, out, _ := testApp(t, "")
	exit, result := runJSON(t, app, out, "signup", "--no-input")
	if code, _ := lookupPath(result, "error.code"); exit != ExitAuth || code != "ENVIRONMENT_CREDENTIAL_ACTIVE" {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	t.Setenv("FLINT_API_KEY", "")
	exit, result = runJSON(t, app, out, "signup", "--no-input", "--profile", "new")
	if code, _ := lookupPath(result, "error.code"); exit != ExitUsage || code != "SIGNUP_INPUT_REQUIRED" {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
}

func TestDXUnknownNestedBodyFields(t *testing.T) {
	app, out, _ := testApp(t, "")
	exit, result := runJSON(t, app, out, "products", "create", "--input", `{"name":"Test","product_type":"physical","default_variant":{"unit_price_money":{"amount":100,"currency":"USD","amunt":123}}}`, "--dry-run=client")
	if exit != ExitUsage || !strings.Contains(fmt.Sprint(result), "$.default_variant.unit_price_money.amunt") || !strings.Contains(fmt.Sprint(result), "did you mean amount?") {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
}

func TestDXPendingReplacementLoginWinsOverValidSession(t *testing.T) {
	devices, polls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loginDevicePath:
			devices++
			writeTestDevice(w)
		case loginTokenPath:
			polls++
			writeTestTokens(w, "replacement-access", "replacement-refresh")
		case "/v1/auth-context":
			fmt.Fprint(w, oauthAuthJSON("sandbox"))
		case oauthRevokePath:
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	stored := installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(time.Hour)))
	if exit, result := runJSON(t, app, out, "login", "--new-session", "--no-wait"); exit != ExitOK {
		t.Fatalf("exit=%d result=%v", exit, result)
	}
	path, _ := app.pendingLoginPath("default")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pending pendingLogin
	if json.Unmarshal(raw, &pending) != nil || len(pending.RequestedScopes) != len(initialCLIScopes) {
		t.Fatal("pending login lost requested scopes")
	}
	if exit, result := runJSON(t, app, out, "login"); exit != ExitOK || devices != 1 || polls != 1 || !strings.Contains(stored(), "replacement-access") {
		t.Fatalf("exit=%d devices=%d polls=%d result=%v", exit, devices, polls, result)
	}
}

func TestDXOAuthRefreshBudgetStartsAfterLock(t *testing.T) {
	previousRefresh, previousLock := oauthRefreshTimeout, localFileLockTimeout
	oauthRefreshTimeout = 10 * time.Millisecond
	localFileLockTimeout = 150 * time.Millisecond
	t.Cleanup(func() { oauthRefreshTimeout = previousRefresh; localFileLockTimeout = previousLock })
	app, _, _ := testApp(t, "")
	t.Setenv("FLINT_API_KEY", "")
	installTestOAuth(t, app, testOAuthCredential(t, "https://api.example.test", time.Now().Add(time.Hour)))
	path, _ := app.configPath()
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	unlocked := make(chan error, 1)
	go func() { time.Sleep(35 * time.Millisecond); unlocked <- lock.Unlock() }()
	if _, e := app.oauthAccess(context.Background(), "default"); e != nil {
		t.Fatal(e)
	}
	if err := <-unlocked; err != nil {
		t.Fatal(err)
	}
}

func TestDXSingleResourceUnknownProjection(t *testing.T) {
	app, out, _ := testApp(t, "")
	command, _ := app.Registry.ByName("customers.get")
	options := defaultOptions()
	options.Output = "json"
	options.Select = []string{"nmae"}
	value := map[string]any{"data": map[string]any{"customer_id": "cus_1", "name": "Ada", "metadata": map[string]any{}}}
	e := app.writeResult(value, command, options)
	if e == nil || e.Code != "UNKNOWN_OUTPUT_FIELD" || !strings.Contains(e.Message, "Did you mean name?") {
		t.Fatalf("error=%v output=%s", e, out)
	}
}

func TestDXProjectionThroughNullParent(t *testing.T) {
	app, out, _ := testApp(t, "")
	command, _ := app.Registry.ByName("customers.list")
	opts := defaultOptions()
	opts.Output = "json"
	opts.Select = []string{"metadata.label"}
	value := map[string]any{"data": []any{
		map[string]any{"metadata": map[string]any{"label": "first"}},
		map[string]any{"metadata": nil},
	}}
	if e := app.writeResult(value, command, opts); e != nil {
		t.Fatal(e)
	}
	var result map[string]any
	if json.Unmarshal(out.Bytes(), &result) != nil {
		t.Fatal(out.String())
	}
	if v, exists := lookupPath(result, "data.1.metadata.label"); !exists || v != nil {
		t.Fatal(result)
	}
	command, _ = app.Registry.ByName("customers.get")
	opts.Select = nil
	opts.Field = "metadata.label"
	out.Reset()
	if e := app.writeResult(map[string]any{"data": map[string]any{"metadata": nil}}, command, opts); e != nil || strings.TrimSpace(out.String()) != "null" {
		t.Fatalf("error=%v output=%s", e, out)
	}
}

func TestDXInvalidHTTPResponseStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not JSON")
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	_, e := app.doRequest(context.Background(), server.URL, "flint_test_xxx", http.MethodGet, "/v1/customers", nil, "", false)
	if e == nil || e.HTTPStatus != http.StatusOK {
		t.Fatalf("error=%v", e)
	}
	opts := defaultOptions()
	opts.Output = "json"
	app.writeError(e, opts)
	if !strings.Contains(out.String(), `"http_status":200`) {
		t.Fatal(out.String())
	}
}
