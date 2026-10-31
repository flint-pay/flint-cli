package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestMCPToolNamesAreAcceptedByModelAPIs(t *testing.T) {
	app, _, _ := testApp(t, "")
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	valid := regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	seen := map[string]string{}
	for _, entry := range index.Entries {
		if entry.Type != "operation" {
			continue
		}
		if strings.Contains(entry.ID, "_") {
			t.Fatalf("canonical name contains underscore: %s", entry.ID)
		}
		name := mcpToolName(entry.ID)
		if !valid.MatchString(name) {
			t.Errorf("invalid tool name: %s", name)
		}
		if previous, ok := seen[name]; ok {
			t.Errorf("name collision: %s and %s", previous, entry.ID)
		}
		seen[name] = entry.ID
		resolved, ok := index.Lookup(name)
		if !ok || resolved.ID != entry.ID {
			t.Errorf("%s does not resolve to %s", name, entry.ID)
		}
	}
	tools, _ := mcpListedTools(t, app, mcpAllToolsOptions(t), latestMCPProtocolVersion)
	for _, tool := range tools {
		if !valid.MatchString(tool["name"].(string)) {
			t.Error(tool["name"])
		}
	}
}

func TestMCPDottedNamesAndFiltersStillWork(t *testing.T) {
	server, api := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	for _, operation := range []string{"customers_list", "customers.list", "customers list"} {
		result := mcpCall(t, app, defaultOptions(), "flint_read", map[string]any{"operation": operation})
		if result["isError"] == true {
			t.Fatalf("%s: %s", operation, mcpResultText(result))
		}
	}
	if len(api.seen()) != 3 {
		t.Fatal(api.seen())
	}
	for _, filter := range []string{"payment-intents.create", "payment_intents_create", "payment-intents create"} {
		opts := mcpServeOptions(t, "--tools", filter)
		if e := app.validateMCPServeOptions(opts); e != nil {
			t.Fatal(e)
		}
		tools, _ := mcpListedTools(t, app, opts, latestMCPProtocolVersion)
		names := []string{}
		for _, tool := range tools {
			n := tool["name"].(string)
			if !strings.HasPrefix(n, "flint_") {
				names = append(names, n)
			}
		}
		if fmt.Sprint(names) != "[payment_intents_create]" {
			t.Fatalf("%s: %v", filter, names)
		}
	}
}

func TestMCPDefaultToolSetIsCuratedAndAllIsOptIn(t *testing.T) {
	app, _, _ := testApp(t, "")
	defaults, size := mcpListedTools(t, app, defaultOptions(), latestMCPProtocolVersion)
	names := map[string]bool{}
	for _, tool := range defaults {
		names[tool["name"].(string)] = true
	}
	if len(names) != 5 || size > 6500 {
		t.Fatalf("default tools=%v size=%d", names, size)
	}
	for _, name := range []string{"flint_search", "flint_describe", "flint_read", "flint_write", "flint_help"} {
		if !names[name] {
			t.Error(name)
		}
	}
	filtered, _ := mcpListedTools(t, app, mcpServeOptions(t, "--tools", "customers"), latestMCPProtocolVersion)
	found := map[string]bool{}
	for _, tool := range filtered {
		found[tool["name"].(string)] = true
	}
	for _, name := range []string{"flint_search", "flint_describe", "flint_help", "customers_create"} {
		if !found[name] {
			t.Error(name)
		}
	}
	for _, name := range []string{"flint_read", "flint_write", "help", "schema_input", "api"} {
		if found[name] {
			t.Error(name)
		}
	}
	if e := app.validateMCPServeOptions(mcpServeOptions(t, "--tools", "default")); e == nil {
		t.Fatal("--tools default accepted")
	}
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{"protocolVersion": latestMCPProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}}}, defaultOptions())
	instructions := fmt.Sprint(lookupPathValue(response, "result.instructions"))
	for _, want := range []string{"flint_search", "flint_describe", "flint_read", "flint_write", "idempotency_key", "minor units"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("missing %s: %s", want, instructions)
		}
	}
}

func TestMCPToolsFlagSuggestsPluralsFirst(t *testing.T) {
	app, _, _ := testApp(t, "")
	// The canonical singular customer operation group is valid; typo suggestions
	// still point toward the plural collection group when no group matches.
	err := app.validateMCPServeOptions(mcpServeOptions(t, "--tools", "custmers"))
	if err == nil || err.Code != "UNKNOWN_TOOL_PREFIX" {
		t.Fatalf("err=%v", err)
	}
	suggestions := err.Details.(map[string]any)["suggestions"].([]string)
	if len(suggestions) == 0 || !strings.HasPrefix(suggestions[0], "customers") {
		t.Fatal(suggestions)
	}
}

func TestMCPAgentsCannotReplaceServerSelections(t *testing.T) {
	server, api := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	for _, control := range []string{"profile", "merchant"} {
		for _, opts := range []Options{defaultOptions(), mcpServeOptions(t, "--tools", "customers")} {
			name := "flint_read"
			input := map[string]any{"operation": "customers.list", control: "other"}
			if !mcpPolicyFromOptions(opts).defaultToolSet() {
				name = "customers_list"
				input = map[string]any{control: "other"}
			}
			result := mcpCall(t, app, opts, name, input)
			if result["isError"] != true {
				t.Fatalf("%s accepted: %#v", control, result)
			}
		}
	}
	if len(api.seen()) != 0 {
		t.Fatal(api.seen())
	}
	guarded := mcpCall(t, app, mcpServeOptions(t, "--merchant", "mer_other"), "flint_read", map[string]any{"operation": "customers.list"})
	if !strings.Contains(mcpResultText(guarded), "MERCHANT_GUARD_MISMATCH") {
		t.Fatal(mcpResultText(guarded))
	}
}

func TestMCPHidesCommandsThatRewriteTheHumansSetup(t *testing.T) {
	app, _, _ := testApp(t, "")
	tools, _ := mcpListedTools(t, app, mcpAllToolsOptions(t), latestMCPProtocolVersion)
	for _, tool := range tools {
		switch tool["name"] {
		case "config_set", "config_unset", "auth_logout":
			t.Errorf("%s is exposed over MCP", tool["name"])
		case "doctor":
			if _, ok := lookupPath(tool, "inputSchema.properties.fix"); ok {
				t.Error("doctor exposes --fix, which rewrites the default profile")
			}
		}
	}
	for _, name := range []string{"config_set", "config.set", "config_unset", "config.unset", "auth_logout"} {
		call := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": name, "arguments": map[string]any{"key": "profile", "value": "x"}}}, mcpAllToolsOptions(t))
		if call["error"] == nil && lookupPathValue(call, "result.isError") != true {
			t.Fatalf("%s ran: %#v", name, call)
		}
	}
	// Signup would replace the profile's saved credential with a new account's.
	app.LoadCredential = func(string) (string, error) { return "flint_test_existing", nil }
	app.StoreCredential = func(string, string) error { t.Error("signup replaced a saved credential"); return nil }
	result := mcpCall(t, app, mcpAllToolsOptions(t), "signup", map[string]any{"email": "dev@example.com", "first_name": "Ada", "last_name": "Lovelace"})
	if result["isError"] != true || !strings.Contains(mcpResultText(result), "already has a saved Flint credential") {
		t.Fatalf("signup over a saved credential = %s", mcpResultText(result))
	}
}

func TestMCPControlsMatchEachTool(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, test := range []struct {
		tool          string
		want, without []string
	}{
		{"customers.list", []string{"all", "select"}, []string{"idempotency_key", "dry_run", "confirm", "preview", "_flint"}},
		{"customers.create", []string{"idempotency_key", "dry_run"}, []string{"all", "wait_for", "preview", "_flint"}},
		{"customers.get", []string{"wait_for", "wait_seconds"}, []string{"all", "idempotency_key", "dry_run", "_flint"}},
		{"webhook-endpoints.delete", []string{"confirm", "dry_run", "idempotency_key"}, []string{"all", "wait_for", "preview", "_flint"}},
		{"listen", []string{"max_events", "max_seconds"}, []string{"all", "_flint"}},
	} {
		schema, e := schemaForMCPTool(mustMCPTool(t, app, test.tool), mcpPolicy{allowLive: true, allowDestructive: true})
		if e != nil {
			t.Fatal(e)
		}
		props := schema["properties"].(map[string]any)
		for _, key := range test.want {
			if _, ok := props[key]; !ok {
				t.Errorf("%s lacks %s", test.tool, key)
			}
		}
		for _, key := range test.without {
			if _, ok := props[key]; ok {
				t.Errorf("%s exposes %s", test.tool, key)
			}
		}
	}
}

func TestMCPQueryParametersKeepTheirContract(t *testing.T) {
	app, _, _ := testApp(t, "")
	schema, _ := schemaForMCPTool(mustMCPTool(t, app, "customers.list"), mcpPolicy{})
	if enum := fmt.Sprint(lookupPathValue(schema, "properties.sort_by.enum")); !strings.Contains(enum, "created_at") {
		t.Fatalf("sort_by enum = %s", enum)
	}
	if got := fmt.Sprint(lookupPathValue(schema, "properties.page_size.default")); got != "10" {
		t.Fatalf("page_size default = %s", got)
	}
	if got := fmt.Sprint(lookupPathValue(schema, "properties.created_after.format")); got != "<nil>" {
		t.Fatalf("created_after format = %s", got)
	}
	if err := validateMCPArguments(mustMCPTool(t, app, "customers.list"), map[string]any{"sort_by": "nonsense"}); err == nil {
		t.Fatal("an undocumented sort_by value passed validation")
	}
	if got := fmt.Sprint(lookupPathValue(schema, "properties.status.type")); got != "array" {
		t.Fatalf("status filter type = %s", got)
	}
	for _, fixture := range []struct {
		status any
		valid  bool
	}{
		{[]any{"active", "deleted"}, true},
		{[]any{"nonsense"}, false},
		{"active", true},
	} {
		err := validateMCPArguments(mustMCPTool(t, app, "customers.list"), map[string]any{"status": fixture.status})
		if (err == nil) != fixture.valid {
			t.Errorf("status filter %#v: %v (valid=%v)", fixture.status, err, fixture.valid)
		}
	}
	enums := 0
	for _, tool := range app.mcpAllTools() {
		if !tool.Supports.Pagination {
			continue
		}
		toolSchema, _ := schemaForMCPTool(tool, mcpPolicy{})
		if _, ok := lookupPath(toolSchema, "properties.status.items.enum"); ok {
			enums++
		}
	}
	if enums < 20 {
		t.Fatalf("only %d list tools keep a status item enum", enums)
	}
	get, _ := schemaForMCPTool(mustMCPTool(t, app, "customers.get"), mcpPolicy{})
	if description := fmt.Sprint(lookupPathValue(get, "properties.customer_id.description")); description == "" || description == "<nil>" {
		t.Fatalf("customer_id description = %s", description)
	}
}

func TestMCPDescriptionsSayWhatTitlesDoNot(t *testing.T) {
	app, _, _ := testApp(t, "")
	tools, _ := mcpListedTools(t, app, mcpAllToolsOptions(t), latestMCPProtocolVersion)
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	sameAsTitle := 0
	for _, tool := range tools {
		description := tool["description"].(string)
		if strings.TrimSuffix(description, ".") == tool["title"] {
			sameAsTitle++
		}
		entry, _ := index.Lookup(tool["name"].(string))
		if description == "" && !mcpEmptyDescriptionBaseline[entry.OperationID] || len(description) > 450 {
			t.Errorf("%s description is %d bytes", tool["name"], len(description))
		}
		if strings.Contains(description, "_flint") {
			t.Errorf("%s describes removed controls: %s", tool["name"], description)
		}
		if strings.Contains(description, "has_more") && !strings.Contains(description, "no has_more") {
			t.Errorf("%s mentions has_more", tool["name"])
		}
	}
	if sameAsTitle > len(tools)/10 {
		t.Errorf("%d of %d descriptions repeat title", sameAsTitle, len(tools))
	}
}

func TestMCPHelpForAToolFollowsItsInputSchema(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, operation := range []string{"customers_create", "customers.create", "customers create"} {
		result := mcpCall(t, app, defaultOptions(), "flint_describe", map[string]any{"operation": operation})
		if result["isError"] == true || !strings.Contains(mcpResultText(result), "customers.create") || !strings.Contains(mcpResultText(result), "email") {
			t.Fatal(mcpResultText(result))
		}
	}
	group := mcpCall(t, app, defaultOptions(), "flint_help", map[string]any{"topic": "customers"})
	if group["isError"] == true || !strings.Contains(mcpResultText(group), "customers.create") {
		t.Fatal(mcpResultText(group))
	}
}

func TestMCPMissingCredentialAdviceFitsAClientConfig(t *testing.T) {
	app, _, _ := testApp(t, "")
	t.Setenv("FLINT_API_KEY", "")
	result := mcpCall(t, app, defaultOptions(), "customers_list", map[string]any{})
	text := mcpResultText(result)
	if result["isError"] != true || lookupPathValue(mcpErrorJSON(t, result), "error.code") != "API_KEY_REQUIRED" || !strings.Contains(text, "FLINT_API_KEY") || !strings.Contains(text, "restart") || strings.Contains(text, "flint auth login") {
		t.Fatalf("missing credential = %s", text)
	}
}

func TestMCPRetryGuidanceNamesTheIdempotencyControl(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	cmd, _ := app.Registry.ByName("refunds.create")
	apiError := map[string]any{"type": "unavailable_error", "code": "SERVICE_UNAVAILABLE", "message": "Flint is unavailable.", "details": []any{map[string]any{"reason": "fixture"}}, "remediation": map[string]any{"retry": "same_request", "next_steps": "Wait before retrying."}}
	e := apiErrorFromResponse(&apiResponse{Status: http.StatusServiceUnavailable, Value: map[string]any{"error": apiError}})
	e.requestSent = true
	withIdempotencyRetryContext(e, "flint-cli-abc")
	result := app.mcpCommandError(cmd, map[string]any{"order_id": "ord_fixture", "_flint": map[string]any{"idempotency_key": "flint-cli-abc"}}, defaultOptions(), mcpCallResult{value: mcpCLIErrorEnvelope(e), cliError: e, exit: e.ExitCode}, app.mcpPolicy(defaultOptions()), "")
	value := mcpErrorJSON(t, result)
	if !reflect.DeepEqual(value["error"], apiError) {
		t.Fatalf("API error changed: %#v", value)
	}
	if lookupPathValue(value, "cli.idempotency_key") != "flint-cli-abc" {
		t.Fatalf("retry key lost: %#v", value)
	}
	actions, _ := json.Marshal(lookupPathValue(value, "cli.next_actions"))
	if !strings.Contains(string(actions), "idempotency_key") || !strings.Contains(string(actions), "flint-cli-abc") {
		t.Fatalf("retry advice lacks reusable key: %s", actions)
	}
}

func TestMCPSuccessfulResultsCarryWarnings(t *testing.T) {
	server, _ := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	// History cannot be written, which the CLI reports as a warning.
	if err := os.Mkdir(filepath.Join(app.ConfigDir, "history.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	result := mcpCall(t, app, defaultOptions(), "customers_create", map[string]any{"email": "warnings@example.com"})
	content := result["content"].([]map[string]any)
	last := content[len(content)-1]["text"].(string)
	if result["isError"] == true || !strings.Contains(last, "Warning: ") || !strings.Contains(last, "could not record resource history") {
		t.Fatalf("result = %#v", result)
	}
	if _, ok := mcpResultJSON(t, result).(map[string]any)["data"]; !ok {
		t.Fatal("warnings must not change command envelope")
	}
}

func TestMCPSupportsProtocol20241105(t *testing.T) {
	app, _, _ := testApp(t, "")
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{
		"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"},
	}}, defaultOptions())
	if got := lookupPathValue(response, "result.protocolVersion"); got != "2024-11-05" {
		t.Fatalf("negotiated %v", got)
	}
	tools, _ := mcpListedTools(t, app, defaultOptions(), "2024-11-05")
	for _, tool := range tools {
		if _, ok := tool["annotations"]; ok {
			t.Fatalf("2024-11-05 tool %s has annotations, which arrived in 2025-03-26", tool["name"])
		}
		if _, ok := tool["title"]; ok {
			t.Fatalf("2024-11-05 tool %s has a title", tool["name"])
		}
	}
}

func TestMCPDoesNotExposeLegacyToolSurface(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, opts := range []Options{defaultOptions(), mcpAllToolsOptions(t)} {
		tools, _ := mcpListedTools(t, app, opts, latestMCPProtocolVersion)
		for _, tool := range tools {
			n := tool["name"].(string)
			if n == "api" || n == "help" || strings.HasPrefix(n, "schema_") {
				t.Errorf("exposed %s", n)
			}
		}
		for _, name := range []string{"api", "help", "schema_input", "customers.list", "payment-intents_create"} {
			response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": name}}, opts)
			if response["error"] == nil && lookupPathValue(response, "result.isError") != true {
				t.Fatalf("legacy tool %s accepted: %#v", name, response)
			}
		}
	}
	for _, input := range []map[string]any{{"operation": "customers.list", "_flint": map[string]any{"all": true}}, {"operation": "customers.list", "arguments": map[string]any{"_flint": map[string]any{"all": true}}}} {
		result := mcpCall(t, app, defaultOptions(), "flint_read", input)
		if result["isError"] != true {
			t.Fatal(result)
		}
	}
}

func TestMCPEventCatalogIsCompactAndFilterable(t *testing.T) {
	app, _, _ := testApp(t, "")
	events := mcpCall(t, app, defaultOptions(), "flint_help", map[string]any{"topic": "events"})
	if text := mcpResultText(events); events["isError"] == true || len(text) > 64<<10 || (!strings.Contains(text, "payment_intent") || !strings.Contains(text, "succeeded")) {
		t.Fatalf("events: %.500s", text)
	}
	payload := mcpCall(t, app, defaultOptions(), "flint_describe", map[string]any{"event": "payment_intent.succeeded"})
	if payload["isError"] == true || !strings.Contains(mcpResultText(payload), "payment_intent") {
		t.Fatal(mcpResultText(payload))
	}
}

func TestMCPRefusesOversizedReadResults(t *testing.T) {
	read := &Command{CanonicalName: "customers.list"}
	big := map[string]any{"data": strings.Repeat("x", mcpMaxResultBytes)}
	refused := mcpResultTooLarge(read, nil, big)
	if refused == nil || !strings.Contains(mcpResultText(refused), "select") {
		t.Fatalf("oversized read = %#v", refused)
	}
	if mcpResultTooLarge(&Command{CanonicalName: "customers.create", Mutation: true}, nil, big) != nil {
		t.Fatal("a completed write must return its result")
	}
}

func TestMCPCallsReportProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		next := "page_2"
		if r.URL.Query().Get("page_token") == "page_2" {
			next = ""
		}
		fmt.Fprintf(w, `{"data":[{"customer_id":"cus_%s"}],"next_page_token":%q,"request_id":"req_list"}`, r.URL.Query().Get("page_token"), next)
	}))
	t.Cleanup(server.Close)
	app, _, _ := testApp(t, server.URL)
	conn := startMCPTestConn(t, app, defaultOptions())
	conn.initialize(`{}`)
	conn.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"flint_read","arguments":{"operation":"customers.list","all":true},"_meta":{"progressToken":"tok"}}}`)
	var progress []map[string]any
	for {
		message := conn.next()
		if message["method"] == "notifications/progress" {
			progress = append(progress, message)
			continue
		}
		if fmt.Sprint(message["id"]) != "5" || lookupPathValue(message, "result.isError") == true {
			t.Fatalf("response = %#v", message)
		}
		break
	}
	if len(progress) == 0 || lookupPathValue(progress[0], "params.progressToken") != "tok" || !strings.Contains(fmt.Sprint(lookupPathValue(progress[0], "params.message")), "page") {
		t.Fatalf("progress = %#v", progress)
	}
	if exit := conn.close(); exit != ExitOK {
		t.Fatalf("exit=%d", exit)
	}
}

func TestMCPEmptyToolsIsRejectedWithLazyRegistry(t *testing.T) {
	app, _, _ := testApp(t, "")
	app.Registry = newLocalRegistry()
	if err := app.validateMCPServeOptions(mcpServeOptions(t, "--tools", "")); err == nil || err.Code != "INVALID_FLAG_VALUE" {
		t.Fatalf("empty tools: %v", err)
	}
	tools, _ := mcpListedTools(t, app, defaultOptions(), latestMCPProtocolVersion)
	if len(tools) != 5 {
		t.Fatalf("lazy discovery tools: %#v", tools)
	}
}

func TestMCPErrorEnvelopePreservesCoreDetailsAndRemediation(t *testing.T) {
	cmd, _ := NewRegistry().ByName("customers.create")
	apiError := map[string]any{"type": "unavailable_error", "code": "SERVICE_UNAVAILABLE", "message": "Unavailable.", "details": []any{map[string]any{"existing": "preserved"}}, "remediation": map[string]any{"retry": "same_request", "next_actions": []any{map[string]any{"reason_message": "Retry with --timeout 1m."}}}}
	e := apiErrorFromResponse(&apiResponse{Status: http.StatusServiceUnavailable, Value: map[string]any{"error": apiError}})
	e.requestSent = true
	withIdempotencyRetryContext(e, "flint-cli-retry-test")
	result := mcpCommandError(cmd, mcpCallResult{value: mcpCLIErrorEnvelope(e), cliError: e, exit: e.ExitCode}, mcpPolicy{}, "")
	value := mcpErrorJSON(t, result)
	if !reflect.DeepEqual(value["error"], apiError) {
		t.Fatalf("API error or remediation changed: %#v", value)
	}
	for path, want := range map[string]any{"cli.origin": "api", "cli.idempotency_key": "flint-cli-retry-test", "error.details.0.existing": "preserved"} {
		if got := lookupPathValue(value, path); got != want {
			t.Errorf("%s=%#v want=%#v", path, got, want)
		}
	}
	for _, field := range []string{"http_status", "retryable"} {
		if _, ok := value["error"].(map[string]any)[field]; ok {
			t.Errorf("CLI field added to API error: %s", field)
		}
	}
	for _, test := range []struct {
		code    string
		details any
	}{
		{"UNKNOWN_OPERATION", suggestionDetails("customer", []string{"customers"})},
		{"INVALID_REQUEST", []any{map[string]any{"code": "INVALID_VALUE", "param": "email", "message": "Invalid email."}}},
	} {
		t.Run(test.code, func(t *testing.T) {
			local := &CLIError{ExitCode: ExitUsage, Type: "usage_error", Code: test.code, Message: "Invalid input.", Details: test.details}
			result := mcpCommandError(cmd, mcpCallResult{value: mcpCLIErrorEnvelope(local), cliError: local, exit: ExitUsage}, mcpPolicy{}, "")
			value := mcpErrorJSON(t, result)
			if lookupPathValue(value, "error.code") != test.code || lookupPathValue(value, "cli.origin") != "cli" {
				t.Fatalf("local code lost: %#v", value)
			}
		})
	}
}
