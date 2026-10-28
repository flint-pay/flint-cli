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
		if r.URL.Path == "/v1/developer/auth-context" {
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

func mcpCall(t *testing.T, app *App, opts Options, name string, arguments map[string]any) map[string]any {
	t.Helper()
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": name, "arguments": arguments}}, opts)
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

func mcpListedTools(t *testing.T, app *App, opts Options, version string) ([]map[string]any, int) {
	t.Helper()
	response := app.handleMCPVersion(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}, opts, version)
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
	filtered, filteredSize := mcpListedTools(t, app, mcpServeOptions(t, "--tools", "customers,payment-links", "--tools", "help"), latestMCPProtocolVersion)
	if filteredSize >= size/4 || len(filtered) == 0 {
		t.Fatalf("filtered tools/list is %d bytes with %d tools", filteredSize, len(filtered))
	}
	for _, tool := range filtered {
		name := mcpCanonicalToolName(tool["name"].(string))
		if !(mcpPrefixMatches("customers", name) || mcpPrefixMatches("payment-links", name) || mcpPrefixMatches("help", name) || mcpAlwaysEnabled(name)) {
			t.Fatalf("filter exposed %s", name)
		}
		if name == "customer-sessions.create" {
			t.Fatal("prefix customers must not match customer-sessions")
		}
	}
	call := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{"name": "version"}}, mcpServeOptions(t, "--tools", "customers"))
	if message, _ := lookupPath(call, "error.message"); !strings.Contains(fmt.Sprint(message), "not enabled") {
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
	for _, want := range []string{"sandbox", "live", "--allow-live", "--tools", "dry_run", "idempotency_key", "help"} {
		if !strings.Contains(text, want) {
			t.Fatalf("instructions missing %q: %s", want, text)
		}
	}
}

func TestMCPAdvertisedControlsFollowServerPolicy(t *testing.T) {
	app, _, _ := testApp(t, "")
	controlsOf := func(opts Options, name string) map[string]any {
		tools, _ := mcpListedTools(t, app, opts, latestMCPProtocolVersion)
		for _, tool := range tools {
			if tool["name"] == name {
				controls, _ := lookupPath(tool, "inputSchema.properties._flint.properties")
				return controls.(map[string]any)
			}
		}
		t.Fatalf("tool %s missing", name)
		return nil
	}
	restricted := controlsOf(mcpServeOptions(t, "--tools", "api-keys"), "api-keys_revoke")
	for _, withheld := range []string{"live", "confirm"} {
		if _, ok := restricted[withheld]; ok {
			t.Fatalf("default server advertises _flint.%s", withheld)
		}
	}
	for _, kept := range []string{"preview", "dry_run", "idempotency_key"} {
		if _, ok := restricted[kept]; !ok {
			t.Fatalf("default server must keep _flint.%s", kept)
		}
	}
	allowed := controlsOf(mcpServeOptions(t, "--tools", "api-keys", "--allow-live", "--allow-destructive"), "api-keys_revoke")
	for _, control := range []string{"live", "confirm"} {
		if _, ok := allowed[control]; !ok {
			t.Fatalf("allow flags must advertise _flint.%s", control)
		}
	}
}

func TestMCPControlsAllHaveDescriptions(t *testing.T) {
	app, _, _ := testApp(t, "")
	tools, _ := mcpListedTools(t, app, mcpServeOptions(t, "--allow-live", "--allow-destructive"), latestMCPProtocolVersion)
	for _, tool := range tools {
		controls, ok := lookupPath(tool, "inputSchema.properties._flint.properties")
		if !ok {
			t.Fatalf("tool %s has no _flint controls", tool["name"])
		}
		for name, raw := range controls.(map[string]any) {
			if description, _ := raw.(map[string]any)["description"].(string); strings.TrimSpace(description) == "" {
				t.Errorf("%s _flint.%s has no description", tool["name"], name)
			}
		}
	}
	revoke, _ := app.Registry.ByName("api-keys.revoke")
	schema, _ := schemaForMCPTool(revoke, mcpPolicy{})
	key, _ := lookupPath(schema, "properties._flint.properties.idempotency_key.description")
	if !strings.Contains(fmt.Sprint(key), "Reuse the same key when retrying") {
		t.Fatalf("idempotency_key description = %v", key)
	}
	api, _ := app.Registry.ByName("api")
	apiSchema, _ := schemaForMCPTool(api, mcpPolicy{})
	if enum, _ := lookupPath(apiSchema, "properties.method.enum"); !strings.Contains(fmt.Sprint(enum), "DELETE") {
		t.Fatalf("api method enum = %v", enum)
	}
	for _, path := range []string{"properties.method.description", "properties.path.description"} {
		if value, _ := lookupPath(apiSchema, path); strings.TrimSpace(fmt.Sprint(value)) == "" {
			t.Fatalf("api %s is empty", path)
		}
	}
	if err := validateMCPArguments(api, map[string]any{"method": "TRACE", "path": "/v1/customers"}); err == nil {
		t.Fatal("api accepted an unsupported method")
	}
	list, _ := app.Registry.ByName("customers.list")
	listSchema, _ := schemaForMCPTool(list, mcpPolicy{})
	if maximum, _ := lookupPath(listSchema, "properties.page_size.maximum"); fmt.Sprint(maximum) != "100" {
		t.Fatalf("page_size maximum = %v", maximum)
	}
}

func TestMCPToolAnnotationsReflectSafety(t *testing.T) {
	app, _, _ := testApp(t, "")
	tools, _ := mcpListedTools(t, app, mcpAllToolsOptions(t), latestMCPProtocolVersion)
	byName := map[string]map[string]any{}
	for _, tool := range tools {
		byName[mcpCanonicalToolName(tool["name"].(string))] = tool["annotations"].(map[string]any)
	}
	for _, test := range []struct {
		name                                     string
		readOnly, destructive, idempotent, world bool
	}{
		{"refunds.create", false, true, false, true},
		{"gift-cards.cash-out", false, true, false, true},
		{"developer.partner.apps.rotate-secret", false, true, false, true},
		{"customer-sessions.revoke", false, true, false, true},
		{"customers.list", true, false, true, true},
		{"history", true, false, true, false},
		{"history.clear", false, true, true, false},
		{"listen", false, false, false, true},
		{"help.search", true, false, true, true},
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
	tool, ok := app.mcpTool(name, mcpPolicy{allTools: true})
	if !ok {
		t.Fatalf("tool %s missing", name)
	}
	return tool
}

func TestMCPHelpWithoutTopicIsAShortIndex(t *testing.T) {
	app, _, _ := testApp(t, "")
	result := mcpCall(t, app, defaultOptions(), "help", map[string]any{})
	text := mcpResultText(result)
	if result["isError"] == true || len(text) > 8<<10 {
		t.Fatalf("help index is %d bytes (error=%v)", len(text), result["isError"])
	}
	for _, want := range []string{"test-cards", "schema_input", "customers", "resource_groups", "other_groups", "invoices"} {
		if !strings.Contains(text, want) {
			t.Fatalf("help index missing %q: %s", want, text)
		}
	}
	topic := mcpCall(t, app, defaultOptions(), "help", map[string]any{"topic": "customers.create"})
	if topic["isError"] == true || !strings.Contains(mcpResultText(topic), "customers.create") {
		t.Fatalf("help topic = %s", mcpResultText(topic))
	}
}

func TestMCPAgentsCannotSelfAuthorizeLiveOrConfirm(t *testing.T) {
	server, api := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	for control, want := range map[string]string{"live": "--allow-live", "confirm": "--allow-destructive"} {
		result := mcpCall(t, app, mcpAllToolsOptions(t), "api-keys_revoke", map[string]any{"api_key_id": "key_123", "_flint": map[string]any{control: true}})
		text := mcpResultText(result)
		if result["isError"] != true || !strings.HasPrefix(text, "_flint."+control+" is not accepted") || !strings.Contains(text, want) {
			t.Fatalf("_flint.%s result = %s", control, text)
		}
	}
	if seen := api.seen(); len(seen) != 0 {
		t.Fatalf("rejected controls reached the API: %v", seen)
	}
}

func TestMCPLiveCredentialRequiresAllowLive(t *testing.T) {
	server, api := newMCPTestAPI(t, "live")
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "flint_live_test")
	for _, arguments := range []map[string]any{{}, {"_flint": map[string]any{"merchant": "mer_123"}}} {
		result := mcpCall(t, app, defaultOptions(), "customers.list", arguments)
		text := mcpResultText(result)
		if result["isError"] != true || !strings.HasPrefix(text, "The selected Flint credential or context is LIVE") || !strings.Contains(text, "flint mcp serve --allow-live") || strings.Contains(text, "Re-run with --live") {
			t.Fatalf("live call without --allow-live = %s", text)
		}
	}
	// Dry runs resolve a live API key offline and must not leak CLI advice.
	dryRun := mcpCall(t, app, defaultOptions(), "customers.create", map[string]any{"email": "a@example.com", "_flint": map[string]any{"dry_run": "client"}})
	if dryRun["isError"] != true || !strings.Contains(mcpResultText(dryRun), "LIVE_ACCESS_DISABLED") {
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
	if _, ok := result["structuredContent"].(map[string]any)["data"]; !ok {
		t.Fatalf("structuredContent must stay the command envelope: %#v", result["structuredContent"])
	}
}

func TestMCPConfirmationWithoutElicitationGuidesTheHuman(t *testing.T) {
	server, api := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	result := mcpCall(t, app, mcpAllToolsOptions(t), "api-keys_revoke", map[string]any{"api_key_id": "key_123"})
	text := mcpResultText(result)
	firstLine, _, _ := strings.Cut(text, "\n")
	if result["isError"] != true || !strings.HasPrefix(firstLine, "This operation requires confirmation.") || !strings.Contains(firstLine, "--allow-destructive") || strings.Contains(text, "Re-run with --confirm") || strings.Contains(text, "Flint command exited") {
		t.Fatalf("confirmation error = %s", text)
	}
	if seen := api.seen(); len(seen) != 0 {
		t.Fatalf("unconfirmed operation reached the API: %v", seen)
	}
	// With --allow-destructive the agent confirms, as before.
	opts := mcpAllToolsOptions(t, "--allow-destructive")
	needsConfirm := mcpCall(t, app, opts, "api-keys_revoke", map[string]any{"api_key_id": "key_123"})
	if text := mcpResultText(needsConfirm); !strings.Contains(text, "_flint.confirm set to true") || strings.Contains(text, "--confirm") {
		t.Fatalf("agent-confirmable error = %s", text)
	}
	confirmed := mcpCall(t, app, opts, "api-keys_revoke", map[string]any{"api_key_id": "key_123", "_flint": map[string]any{"confirm": true}})
	if confirmed["isError"] == true {
		t.Fatalf("confirmed revoke failed: %s", mcpResultText(confirmed))
	}
	if seen := api.seen(); len(seen) != 1 || seen[0] != "POST /v1/api-keys/key_123/revoke" {
		t.Fatalf("confirmed requests = %v", seen)
	}
}

func TestMCPErrorsLeadWithTheMessageAndUseToolTerms(t *testing.T) {
	cmd := &Command{Arguments: []Arg{{Name: "forward_to", Flag: "--forward-to"}}}
	for input, want := range map[string]string{
		"Retry the same command with --idempotency-key abc.":  "Retry the same command with _flint.idempotency_key abc.",
		"Use --dry-run=client first.":                         `Use _flint.dry_run="client" first.`,
		"--forward-to must use HTTP or HTTPS":                 "the forward_to argument must use HTTP or HTTPS",
		"This credential is live. Re-run with --live to ack.": "This credential is live. Re-run with --live to ack.",
	} {
		if got := mcpTranslateRemediation(input, cmd, mcpPolicy{}); got != want {
			t.Errorf("translate(%q) = %q, want %q", input, got, want)
		}
	}
	if got := mcpTranslateRemediation("Re-run with --live.", cmd, mcpPolicy{allowLive: true}); got != "Re-run with _flint.live." {
		t.Errorf("allowed live translation = %q", got)
	}
	app, _, _ := testApp(t, "")
	result := mcpCall(t, app, defaultOptions(), "customers.get", map[string]any{"customer_id": "cus_123"})
	text := mcpResultText(result)
	if result["isError"] != true || strings.HasPrefix(text, "{") || strings.HasPrefix(text, "Flint command exited") || !strings.Contains(text, `"exit_code"`) {
		t.Fatalf("error text = %s", text)
	}
}

func TestMCPUnknownToolSuggestsNames(t *testing.T) {
	app, _, _ := testApp(t, "")
	for name, want := range map[string]string{"customers.lst": "customers_list", "customers_lst": "customers_list", "payment.create": "payment-intents_create"} {
		response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": name}}, defaultOptions())
		message, _ := lookupPath(response, "error.message")
		suggestions, _ := lookupPath(response, "error.data.suggestions")
		if !strings.Contains(fmt.Sprint(message), "Did you mean "+want) || !strings.Contains(fmt.Sprint(suggestions), want) {
			t.Fatalf("%s: %#v", name, response)
		}
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
			conn.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"api-keys_revoke","arguments":{"api_key_id":"key_123"}}}`)
			request := conn.next()
			if request["method"] != "elicitation/create" || request["id"] == nil {
				t.Fatalf("expected elicitation request, got %#v", request)
			}
			message := fmt.Sprint(lookupPathValue(request, "params.message"))
			if !strings.Contains(message, "api-keys_revoke") || !strings.Contains(message, "sandbox") || !strings.Contains(message, "key_123") {
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
