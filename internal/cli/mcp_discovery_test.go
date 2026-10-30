package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func discoveryFixture(t *testing.T) *App {
	t.Helper()
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_ACCESS_TOKEN", "")
	t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("discovery contacted transport: %s %s", r.Method, r.URL.Path)
		return nil, fmt.Errorf("discovery must stay offline")
	})}
	return app
}

func discoveryData(t *testing.T, app *App, policy mcpPolicy, tool string, arguments map[string]any) map[string]any {
	t.Helper()
	value, e := app.mcpDiscovery(tool, arguments, defaultOptions(), policy)
	if e != nil {
		t.Fatalf("%s %v: %s", tool, arguments, e.Message)
	}
	return value.(map[string]any)
}

func discoveryFields(t *testing.T, value any) map[string]map[string]any {
	t.Helper()
	fields, ok := value.([]map[string]any)
	if !ok {
		t.Fatalf("fields have type %T", value)
	}
	out := map[string]map[string]any{}
	for _, field := range fields {
		out[field["name"].(string)] = field
	}
	return out
}

func TestMCPDiscoveryOfflineExposure(t *testing.T) {
	app := discoveryFixture(t)
	policy := app.mcpPolicy(defaultOptions())
	for _, name := range []string{"payment-intents.get", "payment-intents get", "payment_intents_get"} {
		cmd, entry, e := app.mcpResolveOperation(name, policy)
		if e != nil || cmd == nil || entry.ID != "payment-intents.get" {
			t.Fatalf("resolve %q: %+v %v", name, entry, e)
		}
	}
	for _, name := range []string{"api", "config.set", "help", "schema.input", "me.get"} {
		_, _, e := app.mcpResolveOperation(name, policy)
		if e == nil || e.Code != "OPERATION_NOT_AVAILABLE" || !strings.Contains(fmt.Sprint(e.Details), "credential_type") {
			t.Errorf("hidden %s: %v", name, e)
		}
	}
	for _, name := range []string{"payment-intents.get", "payment-intents_get"} {
		if _, ok := app.mcpTool(name, policy); ok {
			t.Errorf("non-exact typed name accepted: %q", name)
		}
	}
	if _, ok := app.mcpTool("payment_intents_get", policy); !ok {
		t.Fatal("all-underscore typed name unavailable")
	}
	for _, flags := range [][]string{{"--tools", "payment-intents"}, {"--tools", "payment_intents"}, {"--tools", "payment_intents_get"}} {
		opts := mcpServeOptions(t, flags...)
		if e := app.validateMCPServeOptions(opts); e != nil {
			t.Fatalf("scope %v: %v", flags, e)
		}
		p := app.mcpPolicy(opts)
		if _, _, e := app.mcpResolveOperation("payment-intents.get", p); e != nil {
			t.Fatal(e)
		}
		if _, _, e := app.mcpResolveOperation("customers.get", p); e == nil || !strings.Contains(fmt.Sprint(e.Details), "outside_tool_scope") {
			t.Fatalf("outside scope: %v", e)
		}
	}
	if e := app.validateMCPServeOptions(mcpServeOptions(t, "--tools", "default")); e == nil || e.Code != "UNKNOWN_TOOL_PREFIX" {
		t.Fatalf("retired default scope: %v", e)
	}
	if e := app.validateMCPServeOptions(mcpServeOptions(t, "--tools", "custmers")); e == nil || e.Code != "UNKNOWN_TOOL_PREFIX" {
		t.Fatalf("misspelled group: %v", e)
	} else {
		details := e.Details.(map[string]any)
		suggestions := details["suggestions"].([]string)
		if len(suggestions) == 0 || suggestions[0] != "customers" {
			t.Fatalf("misspelled group suggestions: %v", suggestions)
		}
	}
	readOnly := app.mcpPolicy(mcpServeOptions(t, "--read-only"))
	if _, _, e := app.mcpResolveOperation("customers.create", readOnly); e == nil || !strings.Contains(fmt.Sprint(e.Details), "read_only_server") {
		t.Fatalf("read-only write: %v", e)
	}
	for _, token := range []string{"ckat_v1.fixture", "unknown-fixture-token"} {
		t.Setenv("FLINT_ACCESS_TOKEN", token)
		p := app.mcpPolicy(defaultOptions())
		_, _, e := app.mcpResolveOperation("customers.list", p)
		if strings.HasPrefix(token, "ckat_") && (e == nil || e.Code != "OPERATION_NOT_AVAILABLE") {
			t.Fatalf("checkout credential sees merchant operation: %v", e)
		}
		if strings.HasPrefix(token, "unknown") && e != nil {
			t.Fatalf("unknown credential filtered: %v", e)
		}
	}
}

func TestMCPDiscoveryDefinitionBudgets(t *testing.T) {
	app := discoveryFixture(t)
	for mask := 0; mask < 16; mask++ {
		flags := []string{}
		for bit, flag := range []string{"--allow-live", "--allow-destructive", "--read-only", "--tools"} {
			if mask&(1<<bit) != 0 {
				flags = append(flags, flag)
				if flag == "--tools" {
					flags = append(flags, "customers")
				}
			}
		}
		policy := app.mcpPolicy(mcpServeOptions(t, flags...))
		instructions := mcpInstructions(policy)
		if utf8.RuneCountInString(instructions) > 1400 {
			t.Errorf("instructions %v: %d chars", flags, utf8.RuneCountInString(instructions))
		}
		tools, e := app.mcpAdvertisedTools(policy, latestMCPProtocolVersion)
		if e != nil {
			t.Fatal(e)
		}
		names := map[string]bool{}
		for _, tool := range tools {
			name := tool["name"].(string)
			names[name] = true
			if !regexp.MustCompile(`^[a-z0-9_]{1,52}$`).MatchString(name) {
				t.Errorf("invalid tool name: %q", name)
			}
			props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
			if props["_flint"] != nil || !policy.allowLive && props["live"] != nil {
				t.Errorf("unavailable controls in %s: %v", name, props)
			}
		}
		if !names["flint_search"] || !names["flint_describe"] || !names["flint_help"] {
			t.Errorf("missing discovery helpers: %v", names)
		}
		if policy.defaultToolSet() {
			want, budget := 5, 6500
			if policy.readOnly {
				want, budget = 4, 5000
			}
			raw, _ := json.Marshal(rpcResult(1, map[string]any{"tools": tools}))
			if len(tools) != want || len(raw) > budget {
				t.Errorf("discovery %v: count=%d bytes=%d budget=%d", flags, len(tools), len(raw), budget)
			}
		} else if names["flint_read"] || names["flint_write"] || !names["customers_get"] || names["payment_intents_get"] {
			t.Errorf("typed scope: %v", names)
		}
		if policy.readOnly && (names["flint_write"] || names["customers_create"]) {
			t.Errorf("read-only exposes writes: %v", names)
		}
	}
	for _, version := range []string{"2024-11-05", "2025-03-26", "2025-06-18"} {
		for _, tool := range mcpDiscoveryTools(mcpPolicy{}, version) {
			if version < "2025-03-26" && tool["annotations"] != nil || version < "2025-06-18" && tool["title"] != nil {
				t.Errorf("unsupported fields for %s: %v", version, tool)
			}
		}
	}
}

func TestMCPDiscoveryViews(t *testing.T) {
	app := discoveryFixture(t)
	policy := app.mcpPolicy(defaultOptions())
	refund := discoveryData(t, app, policy, "flint_describe", map[string]any{"operation": "refunds_create"})
	fields := discoveryFields(t, refund["arguments"])
	if refund["operation"] != "refunds.create" || refund["route"] != "POST /v1/refunds" || refund["idempotency_key"] != "required" || refund["confirm"] != true || fields["refund_type"]["required"] != true || refund["variants"] == nil {
		t.Fatalf("refund header/variant metadata missing: %v", refund)
	}
	branch := discoveryData(t, app, policy, "flint_describe", map[string]any{"operation": "refunds.create", "variant": "amount"})
	if discoveryFields(t, branch["arguments"])["amount_money"]["required"] != true {
		t.Fatalf("amount variant requirements missing: %v", branch)
	}
	nested := discoveryData(t, app, policy, "flint_describe", map[string]any{"operation": "refunds.create", "variant": "amount", "path": "amount_money"})
	if discoveryFields(t, nested["arguments"])["currency"] == nil {
		t.Fatalf("nested money fields missing: %v", nested)
	}
	_, e := app.mcpDiscovery("flint_describe", map[string]any{"operation": "refunds.create", "path": "not_a_field"}, defaultOptions(), policy)
	if e == nil || e.Code != "INVALID_REQUEST" || e.Param != "path" || !strings.Contains(e.Message, "fields:") {
		t.Fatalf("unknown path: %+v", e)
	}
	result := discoveryData(t, app, policy, "flint_describe", map[string]any{"operation": "payment-intents.get", "kind": "result"})
	event := discoveryData(t, app, policy, "flint_describe", map[string]any{"event": "payment_intent.succeeded"})
	resultFields := discoveryFields(t, result["fields"])
	eventFields := discoveryFields(t, event["fields"])
	if resultFields["payment_intent_id"] == nil || eventFields["payment_intent_id"] == nil || eventFields["object"] != nil || event["object"] != "payment_intent" {
		t.Fatalf("event resource root: %v", event)
	}
	for _, name := range []string{"doctor", "history", "history.clear", "listen", "trigger"} {
		value := discoveryData(t, app, policy, "flint_describe", map[string]any{"operation": name})
		fields := discoveryFields(t, value["arguments"])
		if value["route"] != "local" || fields["fix"] != nil || name == "history" && fields["clear"] != nil {
			t.Errorf("local %s: %v", name, value)
		}
		if name == "trigger" && fmt.Sprint(fields["max_seconds"]["maximum"]) != "60" || name == "listen" && (fields["max_events"] == nil || fields["max_seconds"] == nil) {
			t.Errorf("local bounds %s: %v", name, fields)
		}
	}
}

func TestMCPDiscoverySearchAndHelp(t *testing.T) {
	app := discoveryFixture(t)
	policy := app.mcpPolicy(defaultOptions())
	for _, query := range []string{"refund an order", "orders", "create customer", "payment succeeded event", "test card", "who am i"} {
		value := discoveryData(t, app, policy, "flint_search", map[string]any{"query": query})
		raw, _ := json.Marshal(value)
		if len(value["results"].([]map[string]any)) == 0 || len(raw) > 2000 {
			t.Errorf("search %q: %s (%d bytes)", query, raw, len(raw))
		}
	}
	for _, tc := range []struct {
		topic  string
		budget int
	}{{"", 3000}, {"events", 5500}} {
		value := discoveryData(t, app, policy, "flint_help", map[string]any{"topic": tc.topic})
		raw, _ := json.Marshal(value)
		if len(raw) > tc.budget {
			t.Errorf("help %q: %d bytes > %d", tc.topic, len(raw), tc.budget)
		}
	}
	scoped := app.mcpPolicy(mcpServeOptions(t, "--tools", "customers", "--read-only"))
	index, _ := app.mcpOperationIndex(scoped)
	value := discoveryData(t, app, scoped, "flint_search", map[string]any{"query": "customers", "limit": 50})
	for _, result := range value["results"].([]map[string]any) {
		if name, ok := result["operation"].(string); ok {
			entry, _ := index.Lookup(name)
			if !strings.HasPrefix(name, "customers.") || entry.Kind != "read" {
				t.Errorf("outside scope: %v", result)
			}
		}
	}
	groups := discoveryData(t, app, scoped, "flint_help", map[string]any{})["groups"].(map[string]int)
	if len(groups) != 1 || groups["customers"] == 0 {
		t.Errorf("scope groups=%v", groups)
	}
	for _, args := range []map[string]any{{"query": ""}, {"query": "orders", "limit": 51}, {"query": "orders", "unknown": true}} {
		if _, e := app.mcpDiscovery("flint_search", args, defaultOptions(), policy); e == nil || e.Code != "INVALID_REQUEST" {
			t.Errorf("invalid search %v: %v", args, e)
		}
	}
}

func TestMCPDiscoveryScopes(t *testing.T) {
	app := discoveryFixture(t)
	policy := app.mcpPolicy(defaultOptions())
	unknown := discoveryData(t, app, policy, "flint_describe", map[string]any{"operation": "customers.list"})
	if unknown["missing_scopes"] != nil {
		t.Fatal("unknown scopes reported missing")
	}
	app.mcpAuth = &mcpAuthState{}
	app.mcpAuth.store(AuthContext{Scopes: []string{}})
	missing := discoveryData(t, app, policy, "flint_describe", map[string]any{"operation": "customers.list"})
	if len(missing["missing_scopes"].([]string)) == 0 {
		t.Fatal("known missing scope omitted")
	}
	app.mcpAuth.store(AuthContext{Scopes: []string{"customers.write"}})
	held := discoveryData(t, app, policy, "flint_describe", map[string]any{"operation": "customers.list"})
	if held["missing_scopes"] != nil {
		t.Fatalf("implied scope not applied: %v", held)
	}
	t.Setenv("FLINT_ACCESS_TOKEN", "unclassified-fixture")
	app.mcpAuth.store(AuthContext{Scopes: []string{}})
	unclassified := discoveryData(t, app, app.mcpPolicy(defaultOptions()), "flint_describe", map[string]any{"operation": "customers.list"})
	if unclassified["missing_scopes"] != nil {
		t.Fatal("unclassified credential reported missing scopes")
	}
}
