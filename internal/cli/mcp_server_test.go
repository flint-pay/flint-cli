package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mcpTestAPI is a fake Flint API that reports a fixed credential environment
// and records every request other than the auth-context check.
type mcpTestAPI struct {
	mu       sync.Mutex
	requests []string
}

func newMCPTestAPI(t *testing.T, environment string) (*httptest.Server, *mcpTestAPI) {
	t.Helper()
	state := &mcpTestAPI{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON(environment))
			return
		}
		state.mu.Lock()
		state.requests = append(state.requests, r.Method+" "+r.URL.Path)
		state.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/customers":
			fmt.Fprint(w, `{"data":[],"next_page_token":"","request_id":"req_list"}`)
		default:
			fmt.Fprint(w, `{"data":{"api_key_id":"key_123"},"request_id":"req_write"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server, state
}

func (s *mcpTestAPI) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func mcpServeOptions(t *testing.T, flags ...string) Options {
	t.Helper()
	_, opts, _, err := parseInvocation(NewRegistry(), append([]string{"mcp", "serve"}, flags...))
	if err != nil {
		t.Fatalf("parse mcp serve %v: %v", flags, err)
	}
	return opts
}

// mcpAllToolsOptions serves every tool, as flint mcp serve --tools all does.
func mcpAllToolsOptions(t *testing.T, flags ...string) Options {
	t.Helper()
	return mcpServeOptions(t, append([]string{"--tools", "all"}, flags...)...)
}

// mcpCall adapts old operational fixtures to the public call surface. Tests of
// accepted and rejected wire names use handleMCP directly.
func mcpCall(t *testing.T, app *App, opts Options, name string, arguments map[string]any) map[string]any {
	t.Helper()
	if !strings.HasPrefix(name, "flint_") {
		cmd := mustMCPTool(t, app, name)
		args, controls := map[string]any{}, map[string]any{}
		for key, value := range arguments {
			if key != "_flint" {
				args[key] = value
			}
		}
		if old, ok := arguments["_flint"].(map[string]any); ok {
			for key, value := range old {
				controls[key] = value
			}
		}
		if controls["dry_run"] == "client" {
			controls["dry_run"] = true
		}
		if value, ok := controls["for"].(string); ok {
			duration, err := time.ParseDuration(value)
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Stream {
				args["max_seconds"] = int(duration.Seconds())
			} else {
				controls["wait_seconds"] = int(duration.Seconds())
			}
			delete(controls, "for")
		}
		if cmd.Stream {
			if value, ok := controls["max_events"]; ok {
				args["max_events"] = value
				delete(controls, "max_events")
			}
		}
		if cmd.Mutation && cmd.Supports.IdempotencyKey && controls["idempotency_key"] == nil {
			controls["idempotency_key"] = "mcp-regression-fixture"
		}
		if mcpPolicyFromOptions(opts).defaultToolSet() {
			name = "flint_read"
			if cmd.Mutation || cmd.Stream || cmd.CanonicalName == "history.clear" || cmd.CanonicalName == "init" || cmd.CanonicalName == "signup" {
				name = "flint_write"
			}
			arguments = map[string]any{"operation": cmd.CanonicalName, "arguments": args}
		} else {
			name = mcpToolName(cmd.CanonicalName)
			arguments = args
		}
		for key, value := range controls {
			arguments[key] = value
		}
	}
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": name, "arguments": arguments}}, opts)
	assertMCPWireMessage(t, response)
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call %s: %#v", name, response)
	}
	return result
}

func mcpResultText(result map[string]any) string {
	var texts []string
	for _, item := range result["content"].([]map[string]any) {
		texts = append(texts, item["text"].(string))
	}
	return strings.Join(texts, "\n")
}

// mcpResultJSON reads the single compact JSON block used by every MCP protocol.
// Notes share one optional second block and never change the command value.
func mcpResultJSON(t *testing.T, result map[string]any) any {
	t.Helper()
	if _, exists := result["structuredContent"]; exists {
		t.Fatalf("duplicate structured content: %#v", result)
	}
	content, ok := result["content"].([]map[string]any)
	if !ok {
		raw, err := json.Marshal(result["content"])
		ok = err == nil && json.Unmarshal(raw, &content) == nil
	}
	if !ok || len(content) < 1 || len(content) > 2 {
		t.Fatalf("expected JSON and optional notes blocks: %#v", result)
	}
	raw, ok := content[0]["text"].(string)
	if !ok || content[0]["type"] != "text" {
		t.Fatalf("missing JSON text block: %#v", result)
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("invalid JSON result %q: %v", raw, err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(raw)); err != nil || compact.String() != raw {
		t.Fatalf("result is not compact JSON: %q", raw)
	}
	if len(content) == 2 {
		notes, ok := content[1]["text"].(string)
		if !ok || content[1]["type"] != "text" || notes == "" {
			t.Fatalf("invalid notes block: %#v", content[1])
		}
		for _, line := range strings.Split(notes, "\n") {
			if !strings.HasPrefix(line, "Flint environment: ") && !strings.HasPrefix(line, "Note: ") && !strings.HasPrefix(line, "Warning: ") {
				t.Fatalf("invalid notes line: %q", line)
			}
		}
	}
	return value
}

func mcpErrorJSON(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	if result["isError"] != true {
		t.Fatalf("expected tool error: %#v", result)
	}
	value, ok := mcpResultJSON(t, result).(map[string]any)
	if !ok {
		t.Fatalf("error is not an object: %#v", result)
	}
	raw, _ := json.Marshal(result["content"])
	var content []map[string]any
	_ = json.Unmarshal(raw, &content)
	if len(content) != 1 || len(value) != 2 {
		t.Fatalf("expected one {error,cli} JSON block: %#v", result)
	}
	errorObject, ok := value["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %#v", value)
	}
	metadata, ok := value["cli"].(map[string]any)
	if !ok || (metadata["origin"] != "api" && metadata["origin"] != "cli") {
		t.Fatalf("missing CLI origin: %#v", value)
	}
	if metadata["exit_code"] == nil {
		t.Fatalf("missing CLI exit code: %#v", value)
	}
	actions, ok := metadata["next_actions"].([]any)
	if !ok || len(actions) == 0 {
		t.Fatalf("missing next actions: %#v", value)
	}
	for _, rawAction := range actions {
		action, ok := rawAction.(map[string]any)
		if !ok {
			t.Fatalf("invalid next action: %#v", rawAction)
		}
		switch action["actor"] {
		case "agent":
			call, ok := action["call"].(map[string]any)
			if !ok || call["tool"] == nil || call["arguments"] == nil {
				t.Fatalf("incomplete agent call: %#v", action)
			}
		case "user":
			if text, _ := action["text"].(string); !strings.HasPrefix(text, "Ask the user to") {
				t.Fatalf("user action does not identify who acts: %#v", action)
			}
		default:
			t.Fatalf("missing next action actor: %#v", action)
		}
	}
	if metadata["origin"] == "cli" {
		if errorObject["error_source"] != "caller" {
			t.Fatalf("local error source: %#v", errorObject)
		}
		if _, ok := errorObject["details"].([]any); !ok {
			t.Fatalf("local details must be an array: %#v", errorObject)
		}
	}
	return value
}

// Terminal output retains its own envelope while MCP errors use {error,cli}.
func terminalErrorEnvelope(t *testing.T, e *CLIError) map[string]any {
	t.Helper()
	var out bytes.Buffer
	(&App{Stdout: &out}).writeError(e, Options{Output: "json"})
	var value map[string]any
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func mcpListedTools(t *testing.T, app *App, opts Options, version string) ([]map[string]any, int) {
	t.Helper()
	response := app.handleMCPVersion(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}, opts, version)
	assertMCPWireMessage(t, response)
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response["result"]); err != nil {
		t.Fatal(err)
	}
	tools, ok := lookupPath(response, "result.tools")
	if !ok {
		t.Fatalf("tools/list: %#v", response)
	}
	return tools.([]map[string]any), len(bytes.TrimSpace(raw.Bytes()))
}

func TestMCPToolsListIsCompactAndFilterable(t *testing.T) {
	app, _, _ := testApp(t, "")
	tools, size := mcpListedTools(t, app, mcpAllToolsOptions(t), latestMCPProtocolVersion)
	// The refreshed public request schemas include larger checkout, delivery,
	// and subscription unions, and named variants such as the API key access
	// branches. Keep a bounded catalog and check output schemas separately so
	// their accidental inclusion cannot hide inside this budget.
	if size > 3<<20 {
		t.Fatalf("tools/list is %d bytes; expected at most 3 MiB", size)
	}
	for _, tool := range tools {
		if _, ok := tool["outputSchema"]; ok {
			t.Fatalf("tool %s embeds an outputSchema", tool["name"])
		}
		if title, _ := tool["title"].(string); title == "" {
			t.Fatalf("tool %s has no title", tool["name"])
		}
	}
	filtered, filteredSize := mcpListedTools(t, app, mcpServeOptions(t, "--tools", "customers,payment-links"), latestMCPProtocolVersion)
	if filteredSize >= size/4 || len(filtered) == 0 {
		t.Fatalf("filtered tools/list is %d bytes with %d tools", filteredSize, len(filtered))
	}
	for _, tool := range filtered {
		name := tool["name"].(string)
		if !(mcpPrefixMatches("customers", name) || strings.HasPrefix(name, "payment_links_") || mcpPrefixMatches("help", name) || mcpAlwaysEnabled(name)) {
			t.Fatalf("filter exposed %s", name)
		}
		if name == "customer-sessions.create" {
			t.Fatal("prefix customers must not match customer-sessions")
		}
	}
	call := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{"name": "version"}}, mcpServeOptions(t, "--tools", "customers"))
	if call["error"] == nil && lookupPathValue(call, "result.isError") != true {
		t.Fatalf("filtered-out tool call = %#v", call)
	}
}

func TestMCPServeRejectsInvalidStartupFlags(t *testing.T) {
	for _, test := range []struct {
		args []string
		code string
	}{
		{[]string{"mcp", "serve", "--tools", "customers,not-a-resource"}, "UNKNOWN_TOOL_PREFIX"},
		{[]string{"mcp", "serve", "--tools", ","}, "INVALID_FLAG_VALUE"},
		{[]string{"mcp", "serve", "--live"}, "LIVE_REQUIRES_ALLOW_LIVE"},
	} {
		app, stdout, stderr := testApp(t, "")
		app.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
		exit := app.Run(append(test.args, "--output", "json"))
		if exit != ExitUsage || !strings.Contains(stdout.String(), test.code) || strings.Contains(stdout.String(), `"result"`) {
			t.Fatalf("%v: exit=%d stdout=%s stderr=%s", test.args, exit, stdout, stderr)
		}
	}
}

func TestMCPServeHelpDocumentsSetup(t *testing.T) {
	app, stdout, _ := testApp(t, "")
	if exit := app.Run([]string{"mcp", "serve", "--help"}); exit != ExitOK {
		t.Fatalf("exit=%d", exit)
	}
	for _, want := range []string{"--tools", "--allow-live", "--allow-destructive", "FLINT_API_KEY", "FLINT_PROFILE", "Examples:"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("mcp serve --help is missing %q:\n%s", want, stdout)
		}
	}
}

func TestMCPInitializeIncludesInstructions(t *testing.T) {
	app, _, _ := testApp(t, "")
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{
		"protocolVersion": latestMCPProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"},
	}}, defaultOptions())
	instructions, _ := lookupPath(response, "result.instructions")
	text, _ := instructions.(string)
	for _, want := range []string{"sandbox", "live", "--allow-live", "flint_search", "dry_run", "idempotency_key", "flint_describe"} {
		if !strings.Contains(text, want) {
			t.Fatalf("instructions missing %q: %s", want, text)
		}
	}
}

func TestMCPAdvertisedControlsFollowServerPolicy(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, opts := range []Options{defaultOptions(), mcpServeOptions(t, "--allow-live"), mcpServeOptions(t, "--tools", "api-keys"), mcpServeOptions(t, "--tools", "api-keys", "--allow-live")} {
		tools, _ := mcpListedTools(t, app, opts, latestMCPProtocolVersion)
		wantLive := mcpPolicyFromOptions(opts).allowLive
		for _, tool := range tools {
			name := tool["name"].(string)
			if strings.HasPrefix(name, "flint_") && name != "flint_read" && name != "flint_write" {
				continue
			}
			props := lookupPathValue(tool, "inputSchema.properties").(map[string]any)
			if _, ok := props["live"]; ok != wantLive {
				t.Errorf("%s live=%v want=%v", name, ok, wantLive)
			}
			if _, ok := props["_flint"]; ok {
				t.Errorf("%s exposes _flint", name)
			}
		}
	}
}

func TestMCPControlsAllHaveDescriptions(t *testing.T) {
	app, _, _ := testApp(t, "")
	tools, _ := mcpListedTools(t, app, mcpServeOptions(t, "--allow-live", "--allow-destructive"), latestMCPProtocolVersion)
	for _, tool := range tools {
		props := lookupPathValue(tool, "inputSchema.properties").(map[string]any)
		for name, raw := range props {
			if strings.TrimSpace(fmt.Sprint(raw.(map[string]any)["description"])) == "" {
				t.Errorf("%s %s has no description", tool["name"], name)
			}
		}
	}
}

func TestMCPToolAnnotationsReflectSafety(t *testing.T) {
	app, _, _ := testApp(t, "")
	byName := map[string]map[string]any{}
	for _, tool := range app.mcpAllTools() {
		annotations := mcpToolAnnotations(tool)
		annotations["title"] = mcpToolTitle(tool)
		byName[tool.CanonicalName] = annotations
	}
	for _, test := range []struct {
		name                                     string
		readOnly, destructive, idempotent, world bool
	}{
		{"refunds.create", false, true, false, true},
		{"gift-cards.cash-out", false, true, false, true},
		{"partner-apps.rotate-secret", false, true, false, true},
		{"customer-sessions.revoke", false, true, false, true},
		{"customers.list", true, false, true, true},
		{"history", true, false, true, false},
		{"history.clear", false, true, true, false},
		{"listen", false, false, false, true},
		{"doctor", true, false, true, true},
		// Deleting again leaves the resource deleted.
		{"customers.addresses.delete", false, true, true, true},
		{"webhook-endpoints.delete", false, true, true, true},
	} {
		annotations, ok := byName[test.name]
		if !ok {
			t.Fatalf("tool %s missing", test.name)
		}
		got := []any{annotations["readOnlyHint"], annotations["destructiveHint"], annotations["idempotentHint"], annotations["openWorldHint"]}
		want := []any{test.readOnly, test.destructive, test.idempotent, test.world}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s annotations = %v, want %v", test.name, got, want)
		}
		if title, _ := annotations["title"].(string); title == "" {
			t.Errorf("%s has no annotation title", test.name)
		}
	}
	for _, tool := range app.Registry.Commands {
		if mcpCommandExposed(tool) && tool.Sensitive && byName[tool.CanonicalName]["destructiveHint"] != true {
			t.Errorf("sensitive tool %s is not marked destructive", tool.CanonicalName)
		}
	}
}

func TestMCPHistoryClearIsASeparateTool(t *testing.T) {
	app, _, _ := testApp(t, "")
	if err := validateMCPArguments(mustMCPTool(t, app, "history"), map[string]any{"clear": true}); err == nil {
		t.Fatal("history tool still accepts clear")
	}
	result := mcpCall(t, app, mcpAllToolsOptions(t), "history_clear", map[string]any{})
	if result["isError"] != true || !strings.Contains(mcpResultText(result), "Only the user can confirm") {
		t.Fatalf("history.clear without confirmation = %#v", result)
	}
	result = mcpCall(t, app, mcpAllToolsOptions(t, "--allow-destructive"), "history_clear", map[string]any{"_flint": map[string]any{"confirm": true}})
	if result["isError"] == true {
		t.Fatalf("history.clear with confirmation failed: %s", mcpResultText(result))
	}
}

func mustMCPTool(t *testing.T, app *App, name string) *Command {
	t.Helper()
	for _, tool := range app.mcpAllTools() {
		if tool.CanonicalName == name || tool.CanonicalName == mcpCanonicalToolName(name) || mcpToolName(tool.CanonicalName) == name {
			return tool
		}
	}
	t.Fatalf("tool %s missing", name)
	return nil
}

func TestMCPHelpWithoutTopicIsAShortIndex(t *testing.T) {
	app, _, _ := testApp(t, "")
	result := mcpCall(t, app, defaultOptions(), "flint_help", map[string]any{})
	text := mcpResultText(result)
	if result["isError"] == true || len(text) > 8<<10 {
		t.Fatalf("help index is %d bytes: %s", len(text), text)
	}
	for _, want := range []string{"test-cards", "safety", "groups", "customers", "invoices"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s: %s", want, text)
		}
	}
	topic := mcpCall(t, app, defaultOptions(), "flint_help", map[string]any{"topic": "customers.create"})
	if topic["isError"] == true || !strings.Contains(mcpResultText(topic), "customers.create") {
		t.Fatal(mcpResultText(topic))
	}
}

func TestMCPAgentsCannotSelfAuthorizeLiveOrConfirm(t *testing.T) {
	server, api := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	result := mcpCall(t, app, mcpAllToolsOptions(t), "api-keys_revoke", map[string]any{"api_key_id": "key_123", "_flint": map[string]any{"live": true}})
	if result["isError"] != true || !strings.Contains(mcpResultText(result), "live is not an available control") {
		t.Fatal(mcpResultText(result))
	}
	// Until the separate gate redesign, server confirmation policy still governs
	// calls that use the new top-level confirm control.
	result = mcpCall(t, app, mcpAllToolsOptions(t), "api-keys_revoke", map[string]any{"api_key_id": "key_123", "_flint": map[string]any{"confirm": true}})
	if result["isError"] != true || !strings.Contains(mcpResultText(result), "--allow-destructive") {
		t.Fatal(mcpResultText(result))
	}
	if len(api.seen()) != 0 {
		t.Fatal(api.seen())
	}
}

func TestMCPLiveCredentialRequiresAllowLive(t *testing.T) {
	server, api := newMCPTestAPI(t, "live")
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "flint_live_test")
	for _, arguments := range []map[string]any{{}} {
		result := mcpCall(t, app, defaultOptions(), "customers.list", arguments)
		text := mcpResultText(result)
		if result["isError"] != true || !strings.HasPrefix(fmt.Sprint(lookupPathValue(mcpErrorJSON(t, result), "error.message")), "The selected Flint credential or context is LIVE") || !strings.Contains(text, "flint mcp serve --allow-live") || strings.Contains(text, "Re-run with --live") {
			t.Fatalf("live call without --allow-live = %s", text)
		}
	}
	// A dry run validates offline without requiring live access or confirmation.
	dryRun := mcpCall(t, app, defaultOptions(), "customers.create", map[string]any{"email": "a@example.com", "_flint": map[string]any{"dry_run": "client"}})
	if dryRun["isError"] == true || lookupPathValue(mcpResultJSON(t, dryRun), "data.dry_run") != true {
		t.Fatalf("live dry run without --allow-live = %s", mcpResultText(dryRun))
	}
	if seen := api.seen(); len(seen) != 0 {
		t.Fatalf("live requests reached the API without --allow-live: %v", seen)
	}
	allowed := mcpCall(t, app, mcpServeOptions(t, "--allow-live"), "customers.list", map[string]any{"_flint": map[string]any{"live": true}})
	if allowed["isError"] == true {
		t.Fatalf("live call with --allow-live failed: %s", mcpResultText(allowed))
	}
	if !strings.Contains(mcpResultText(allowed), "Flint environment: live") {
		t.Fatalf("live result does not report its environment: %s", mcpResultText(allowed))
	}
	if meta, _ := allowed["_meta"].(map[string]any); meta[mcpEnvironmentMetaKey] != "live" {
		t.Fatalf("live result _meta = %#v", allowed["_meta"])
	}
	if seen := api.seen(); len(seen) != 1 || seen[0] != "GET /v1/customers" {
		t.Fatalf("allowed live requests = %v", seen)
	}
}

func TestMCPSelectedLiveContextRequiresAllowLive(t *testing.T) {
	app, out, stderr, _, _ := newContextTestApp(t)
	if exit := app.Run([]string{"context", "switch", "ctx_live", "--live", "--output", "json"}); exit != ExitOK {
		t.Fatalf("switch: %d %s %s", exit, out, stderr)
	}
	// The context server fails the test on any request other than its auth
	// and token endpoints, so a forwarded list request would be caught.
	result := mcpCall(t, app, defaultOptions(), "payment-intents.list", map[string]any{})
	text := mcpResultText(result)
	if result["isError"] != true || !strings.Contains(text, "LIVE_ACCESS_DISABLED") || !strings.Contains(text, "--allow-live") {
		t.Fatalf("selected live context without --allow-live = %s", text)
	}
}

func TestMCPSandboxResultsReportEnvironment(t *testing.T) {
	server, _ := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	result := mcpCall(t, app, defaultOptions(), "customers.list", map[string]any{})
	if result["isError"] == true || !strings.Contains(mcpResultText(result), "Flint environment: sandbox") {
		t.Fatalf("sandbox result = %#v", result)
	}
	value := mcpResultJSON(t, result)
	if _, ok := value.(map[string]any)["data"]; !ok {
		t.Fatalf("result lost command envelope: %#v", value)
	}
}

func TestMCPConfirmationWithoutElicitationGuidesTheHuman(t *testing.T) {
	server, api := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	result := mcpCall(t, app, mcpAllToolsOptions(t), "api_keys_revoke", map[string]any{"api_key_id": "key_123"})
	text := mcpResultText(result)
	firstLine := fmt.Sprint(lookupPathValue(mcpErrorJSON(t, result), "error.message"))
	if result["isError"] != true || !strings.HasPrefix(firstLine, "This operation requires confirmation.") || !strings.Contains(firstLine, "--allow-destructive") || strings.Contains(text, "Re-run with --confirm") || strings.Contains(text, "Flint command exited") {
		t.Fatalf("confirmation error = %s", text)
	}
	if seen := api.seen(); len(seen) != 0 {
		t.Fatalf("unconfirmed operation reached the API: %v", seen)
	}
	// With --allow-destructive the agent confirms, as before.
	opts := mcpAllToolsOptions(t, "--allow-destructive")
	needsConfirm := mcpCall(t, app, opts, "api_keys_revoke", map[string]any{"api_key_id": "key_123"})
	if text := mcpResultText(needsConfirm); !strings.Contains(text, "confirm set to true") || strings.Contains(text, "--confirm") {
		t.Fatalf("agent-confirmable error = %s", text)
	}
	confirmed := mcpCall(t, app, opts, "api_keys_revoke", map[string]any{"api_key_id": "key_123", "_flint": map[string]any{"confirm": true}})
	if confirmed["isError"] == true {
		t.Fatalf("confirmed revoke failed: %s", mcpResultText(confirmed))
	}
	if seen := api.seen(); len(seen) != 1 || seen[0] != "POST /v1/api-keys/key_123/revoke" {
		t.Fatalf("confirmed requests = %v", seen)
	}
}

func TestMCPErrorsUseOneJSONEnvelopeAndToolTerms(t *testing.T) {
	cmd := &Command{Arguments: []Arg{{Name: "forward_to", Flag: "--forward-to"}}}
	for input, want := range map[string]string{
		"Retry the same command with --idempotency-key abc.":  "Retry the same command with idempotency_key abc.",
		"Use --dry-run=client first.":                         `Use dry_run: true first.`,
		"--forward-to must use HTTP or HTTPS":                 "the forward_to argument must use HTTP or HTTPS",
		"This credential is live. Re-run with --live to ack.": "This credential is live. Re-run with --live to ack.",
	} {
		if got := mcpTranslateRemediation(input, cmd, mcpPolicy{}); got != want {
			t.Errorf("translate(%q) = %q, want %q", input, got, want)
		}
	}
	if got := mcpTranslateRemediation("Re-run with --live.", cmd, mcpPolicy{allowLive: true}); got != "Re-run with live." {
		t.Errorf("allowed live translation = %q", got)
	}
	app, _, _ := testApp(t, "")
	t.Setenv("FLINT_API_KEY", "")
	result := mcpCall(t, app, defaultOptions(), "customers.get", map[string]any{"customer_id": "cus_123"})
	text := mcpResultText(result)
	value := mcpErrorJSON(t, result)
	if lookupPathValue(value, "cli.origin") != "cli" || lookupPathValue(value, "error.code") != "API_KEY_REQUIRED" || lookupPathValue(value, "cli.exit_code") == nil || !strings.HasPrefix(text, "{") {
		t.Fatalf("error text = %s", text)
	}
}

func TestMCPUnknownToolSuggestsNames(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, name := range []string{"customers.lst", "customers_lst", "flint_serch"} {
		response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": name}}, defaultOptions())
		if response["error"] == nil {
			t.Fatalf("unknown tool accepted: %#v", response)
		}
	}
	unknown := mcpCall(t, app, defaultOptions(), "flint_read", map[string]any{"operation": "customers.lst"})
	if unknown["isError"] != true || !strings.Contains(mcpResultText(unknown), "customers.list") {
		t.Fatal(mcpResultText(unknown))
	}
}

// mcpTestConn drives serveMCP over pipes, one JSON message per line.
type mcpTestConn struct {
	t        *testing.T
	in       *io.PipeWriter
	messages chan map[string]any
	done     chan int
}

func startMCPTestConn(t *testing.T, app *App, opts Options) *mcpTestConn {
	t.Helper()
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	app.Stdin = stdinReader
	app.Stdout = stdoutWriter
	conn := &mcpTestConn{t: t, in: stdinWriter, messages: make(chan map[string]any, 64), done: make(chan int, 1)}
	go func() {
		conn.done <- app.serveMCP(opts)
		_ = stdoutWriter.Close()
	}()
	go func() {
		defer close(conn.messages)
		scanner := bufio.NewScanner(stdoutReader)
		scanner.Buffer(make([]byte, 1<<20), 64<<20)
		for scanner.Scan() {
			var message map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
				t.Errorf("server wrote invalid JSON: %s", scanner.Text())
				continue
			}
			assertMCPWireMessage(t, message)
			conn.messages <- message
		}
	}()
	t.Cleanup(func() { _ = stdinWriter.Close() })
	return conn
}

func (c *mcpTestConn) send(raw string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, raw+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *mcpTestConn) next() map[string]any {
	c.t.Helper()
	select {
	case message, ok := <-c.messages:
		if !ok {
			c.t.Fatal("server closed its output")
		}
		return message
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out waiting for a server message")
	}
	return nil
}

func (c *mcpTestConn) initialize(capabilities string) {
	c.t.Helper()
	c.send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":` + capabilities + `,"clientInfo":{"name":"t","version":"1"}}}`)
	if message := c.next(); message["result"] == nil {
		c.t.Fatalf("initialize = %#v", message)
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

func (c *mcpTestConn) close() int {
	c.t.Helper()
	_ = c.in.Close()
	select {
	case exit := <-c.done:
		return exit
	case <-time.After(5 * time.Second):
		c.t.Fatal("server did not exit")
	}
	return -1
}

func TestMCPJSONRPCFraming(t *testing.T) {
	app, _, stderr := testApp(t, "")
	conn := startMCPTestConn(t, app, defaultOptions())
	conn.initialize(`{}`)
	// A request without an id is a notification: no response, not executed.
	conn.send(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"version"}}`)
	conn.send(`{"jsonrpc":"2.0","id":"n1","method":"notifications/initialized"}`)
	if message := conn.next(); message["id"] != "n1" || fmt.Sprint(lookupPathValue(message, "error.code")) != "-32601" {
		t.Fatalf("notification with id = %#v", message)
	}
	conn.send(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if message := conn.next(); fmt.Sprint(lookupPathValue(message, "error.code")) != "-32600" || message["id"] != nil {
		t.Fatalf("batch = %#v", message)
	}
	conn.send(`{"jsonrpc":"2.0","id":2,"method":"ping","params":` + `"` + strings.Repeat("x", mcpMaxLineBytes) + `"}`)
	if message := conn.next(); fmt.Sprint(lookupPathValue(message, "error.code")) != "-32600" || !strings.Contains(fmt.Sprint(lookupPathValue(message, "error.message")), "exceeds") {
		t.Fatalf("oversized line = %#v", message)
	}
	conn.send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if message := conn.next(); message["id"] != "3" && fmt.Sprint(message["id"]) != "3" {
		t.Fatalf("server stopped serving after an oversized line: %#v", message)
	}
	if exit := conn.close(); exit != ExitOK {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr.String(), "without an id") {
		t.Fatalf("ignored notification was not logged: %s", stderr)
	}
}

func lookupPathValue(value any, path string) any {
	found, _ := lookupPath(value, path)
	return found
}

func TestMCPElicitsHumanConfirmation(t *testing.T) {
	for _, test := range []struct {
		name   string
		reply  string
		revoke bool
	}{
		{"accept", `{"action":"accept","content":{"confirm":true}}`, true},
		{"accept unchecked", `{"action":"accept","content":{"confirm":false}}`, false},
		{"decline", `{"action":"decline"}`, false},
		{"cancel", `{"action":"cancel"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, api := newMCPTestAPI(t, "sandbox")
			app, _, _ := testApp(t, server.URL)
			conn := startMCPTestConn(t, app, mcpAllToolsOptions(t))
			conn.initialize(`{"elicitation":{}}`)
			conn.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"api_keys_revoke","arguments":{"api_key_id":"key_123"}}}`)
			request := conn.next()
			if request["method"] != "elicitation/create" || request["id"] == nil {
				t.Fatalf("expected elicitation request, got %#v", request)
			}
			message := fmt.Sprint(lookupPathValue(request, "params.message"))
			if !strings.Contains(message, "api_keys_revoke") || !strings.Contains(message, "sandbox") || !strings.Contains(message, "key_123") {
				t.Fatalf("elicitation message = %s", message)
			}
			if seen := api.seen(); len(seen) != 0 {
				t.Fatalf("API called before confirmation: %v", seen)
			}
			id, _ := json.Marshal(request["id"])
			conn.send(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":` + test.reply + `}`)
			response := conn.next()
			if fmt.Sprint(response["id"]) != "7" {
				t.Fatalf("tool response = %#v", response)
			}
			isError := lookupPathValue(response, "result.isError") == true
			seen := api.seen()
			if test.revoke {
				if isError || len(seen) != 1 {
					t.Fatalf("approved call: response=%#v requests=%v", response, seen)
				}
			} else {
				text := fmt.Sprint(lookupPathValue(response, "result.content"))
				if !isError || len(seen) != 0 || !strings.Contains(text, "declined") {
					t.Fatalf("declined call: response=%#v requests=%v", response, seen)
				}
			}
			if exit := conn.close(); exit != ExitOK {
				t.Fatalf("exit=%d", exit)
			}
		})
	}
}

func TestMCPElicitationCapabilityModes(t *testing.T) {
	for capabilities, want := range map[string]bool{
		`{}`:                                   false,
		`{"elicitation":{}}`:                   true,
		`{"elicitation":{"form":{}}}`:          true,
		`{"elicitation":{"url":{}}}`:           false,
		`{"elicitation":{"form":{},"url":{}}}`: true,
	} {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(capabilities), &parsed); err != nil {
			t.Fatal(err)
		}
		session := newMCPSession(func(map[string]any) error { return nil })
		session.configure(parsed, "2025-11-25")
		if session.canElicit() != want {
			t.Errorf("%s: canElicit=%v", capabilities, session.canElicit())
		}
		session.configure(parsed, "2025-03-26")
		if session.canElicit() {
			t.Errorf("%s: elicitation enabled for 2025-03-26", capabilities)
		}
	}
}

func TestReadMCPLineHandlesLimitsAndEOF(t *testing.T) {
	input := "short\r\n" + strings.Repeat("y", 40) + "\nlast"
	reader := bufio.NewReaderSize(bytes.NewReader([]byte(input)), 16)
	for _, want := range []struct {
		line    string
		tooLong bool
	}{{"short", false}, {"", true}, {"last", false}} {
		line, tooLong, err := readMCPLine(reader, 32)
		if err != nil || string(line) != want.line || tooLong != want.tooLong {
			t.Fatalf("readMCPLine = %q %v %v, want %q %v", line, tooLong, err, want.line, want.tooLong)
		}
	}
	if _, _, err := readMCPLine(reader, 32); err != io.EOF {
		t.Fatalf("final read err = %v", err)
	}
}
