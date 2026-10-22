package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestMCPToolNamesAreAcceptedByModelAPIs(t *testing.T) {
	app, _, _ := testApp(t, "")
	// The Claude API requires ^[a-zA-Z0-9_-]{1,128}$; OpenAI-style APIs are
	// stricter still about dots.
	valid := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	seen := map[string]string{}
	for _, tool := range app.mcpAllTools() {
		if strings.Contains(tool.CanonicalName, "_") {
			t.Fatalf("canonical name %s contains an underscore, so MCP names would not map back", tool.CanonicalName)
		}
		name := mcpToolName(tool.CanonicalName)
		if !valid.MatchString(name) {
			t.Errorf("tool name %q is not accepted by model APIs", name)
		}
		if previous, exists := seen[name]; exists {
			t.Errorf("tool name %s collides: %s and %s", name, previous, tool.CanonicalName)
		}
		seen[name] = tool.CanonicalName
		if mcpCanonicalToolName(name) != tool.CanonicalName {
			t.Errorf("%s does not map back to %s", name, tool.CanonicalName)
		}
	}
	tools, _ := mcpListedTools(t, app, mcpAllToolsOptions(t), latestMCPProtocolVersion)
	for _, tool := range tools {
		if name := tool["name"].(string); !valid.MatchString(name) {
			t.Errorf("tools/list advertises %q", name)
		}
	}
}

func TestMCPDottedNamesAndFiltersStillWork(t *testing.T) {
	server, api := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	for _, name := range []string{"customers_list", "customers.list"} {
		if result := mcpCall(t, app, defaultOptions(), name, map[string]any{}); result["isError"] == true {
			t.Fatalf("%s failed: %s", name, mcpResultText(result))
		}
	}
	if seen := api.seen(); len(seen) != 2 {
		t.Fatalf("requests = %v", seen)
	}
	for _, filter := range []string{"payment-intents.create", "payment-intents_create"} {
		opts := mcpServeOptions(t, "--tools", filter)
		if err := app.validateMCPServeOptions(opts); err != nil {
			t.Fatalf("--tools %s rejected: %v", filter, err)
		}
		tools, _ := mcpListedTools(t, app, opts, latestMCPProtocolVersion)
		var names []string
		for _, tool := range tools {
			if name := tool["name"].(string); !mcpAlwaysEnabled(mcpCanonicalToolName(name)) {
				names = append(names, name)
			}
		}
		if fmt.Sprint(names) != "[payment-intents_create]" {
			t.Fatalf("--tools %s exposed %v", filter, names)
		}
	}
}

func TestMCPDefaultToolSetIsCuratedAndAllIsOptIn(t *testing.T) {
	app, _, _ := testApp(t, "")
	names := func(opts Options) (map[string]bool, int) {
		tools, size := mcpListedTools(t, app, opts, latestMCPProtocolVersion)
		out := map[string]bool{}
		for _, tool := range tools {
			out[tool["name"].(string)] = true
		}
		return out, size
	}
	defaults, defaultSize := names(defaultOptions())
	t.Logf("default tools/list: %d tools, %d bytes in compact result JSON", len(defaults), defaultSize)
	all, allSize := names(mcpAllToolsOptions(t))
	// The reviewed refund credential/mode unions increase schema size; keep
	// the same 32 tools and a bounded default payload.
	if len(all) != len(app.mcpAllTools()) || len(defaults) >= len(all)/4 || defaultSize >= allSize/4 || len(defaults) > 40 || defaultSize > 180000 {
		t.Fatalf("default exposes %d tools in %d bytes; all exposes %d in %d", len(defaults), defaultSize, len(all), allSize)
	}
	for _, want := range []string{"help", "schema_input", "schema_output", "customers_create", "payment-intents_create", "checkout-sessions_create", "payment-links_create", "refunds_create", "webhook-endpoints_create", "webhook-events_list", "products_create"} {
		if !defaults[want] {
			t.Errorf("default set is missing %s", want)
		}
	}
	for _, unwanted := range []string{"invoices_create", "api-keys_revoke", "orders_create", "orders_get", "orders_list", "auth_status", "orders_pay", "sandboxes_delete"} {
		if defaults[unwanted] {
			t.Errorf("default set exposes %s", unwanted)
		}
	}
	extended, _ := names(mcpServeOptions(t, "--tools", "default,invoices"))
	if !extended["invoices_create"] || !extended["customers_create"] {
		t.Fatal("--tools default,invoices must extend the default set")
	}
	// help and the schema tools stay available to every filter, so guidance
	// that names them always resolves.
	filtered, _ := names(mcpServeOptions(t, "--tools", "customers"))
	for _, want := range []string{"help", "schema_input", "schema_output", "schema_command", "schema_commands", "schema_events"} {
		if !filtered[want] {
			t.Errorf("--tools customers dropped %s", want)
		}
	}
	call := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": "invoices_create"}}, defaultOptions())
	if message := fmt.Sprint(lookupPathValue(call, "error.message")); !strings.Contains(message, "not enabled") || !strings.Contains(message, "--tools default,invoices") {
		t.Fatalf("disabled tool error = %s", message)
	}
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{
		"protocolVersion": latestMCPProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"},
	}}, defaultOptions())
	instructions := fmt.Sprint(lookupPathValue(response, "result.instructions"))
	for _, want := range []string{"customers_create", "default tool set", "--tools all", "schema_input", "schema_output", "next_page_token", "minor units"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions missing %q: %s", want, instructions)
		}
	}
	if strings.Contains(instructions, "schema.output") || strings.Contains(instructions, "customers.create") {
		t.Errorf("instructions use dotted tool names: %s", instructions)
	}
}

func TestMCPToolsFlagSuggestsPluralsFirst(t *testing.T) {
	app, _, _ := testApp(t, "")
	err := app.validateMCPServeOptions(mcpServeOptions(t, "--tools", "customer"))
	if err == nil || err.Code != "UNKNOWN_TOOL_PREFIX" {
		t.Fatalf("err = %v", err)
	}
	suggestions := err.Details.(map[string]any)["suggestions"].([]string)
	if len(suggestions) == 0 || suggestions[0] != "customers" {
		t.Fatalf("suggestions = %v", suggestions)
	}
	if got := mcpNameSuggestions("payment-intent", mcpToolGroups(app.mcpAllTools())); len(got) == 0 || got[0] != "payment-intents" {
		t.Fatalf("payment-intent suggestions = %v", got)
	}
}

func TestMCPAgentsCannotReplaceServerSelections(t *testing.T) {
	for _, name := range []string{"FLINT_PROFILE", "FLINT_MERCHANT"} {
		t.Setenv(name, "")
	}
	server, api := newMCPTestAPI(t, "sandbox") // The credential's merchant is mer_123.
	app, _, _ := testApp(t, server.URL)
	config := `{"default_profile":"default","profiles":{"default":{},"other":{},"guarded":{"merchant_guard":"mer_123"}}}`
	if err := os.WriteFile(filepath.Join(app.ConfigDir, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	list := func(opts Options, controls map[string]any) map[string]any {
		t.Helper()
		return mcpCall(t, app, opts, "customers_list", map[string]any{"_flint": controls})
	}
	rejected := func(result map[string]any, control string) {
		t.Helper()
		if text := mcpResultText(result); result["isError"] != true || !strings.HasPrefix(text, "_flint."+control+" is not accepted") {
			t.Fatalf("expected _flint.%s to be rejected: %s", control, text)
		}
	}
	human := mcpServeOptions(t, "--merchant", "mer_human")
	if result := list(human, nil); result["isError"] != true || !strings.Contains(mcpResultText(result), "MERCHANT_GUARD_MISMATCH") {
		t.Fatalf("server guard did not apply: %s", mcpResultText(result))
	}
	// The credential's own merchant would satisfy a replaced guard: that is
	// exactly the widening the server must refuse.
	rejected(list(human, map[string]any{"merchant": "mer_123"}), "merchant")
	if result := list(human, map[string]any{"merchant": "mer_human"}); strings.HasPrefix(mcpResultText(result), "_flint.merchant is not accepted") {
		t.Fatal("repeating the server's guard must be accepted")
	}
	t.Setenv("FLINT_MERCHANT", "mer_human")
	rejected(list(defaultOptions(), map[string]any{"merchant": "mer_123"}), "merchant")
	t.Setenv("FLINT_MERCHANT", "")
	if seen := api.seen(); len(seen) != 0 {
		t.Fatalf("guarded calls reached the API: %v", seen)
	}
	// Without a server guard, an agent may add one: it only narrows.
	if result := list(defaultOptions(), map[string]any{"merchant": "mer_other"}); !strings.Contains(mcpResultText(result), "MERCHANT_GUARD_MISMATCH") {
		t.Fatalf("agent guard = %s", mcpResultText(result))
	}
	if result := list(defaultOptions(), map[string]any{"merchant": "mer_123"}); result["isError"] == true {
		t.Fatalf("matching agent guard failed: %s", mcpResultText(result))
	}

	rejected(list(mcpServeOptions(t, "--profile", "other"), map[string]any{"profile": "default"}), "profile")
	if result := list(mcpServeOptions(t, "--profile", "other"), map[string]any{"profile": "other"}); result["isError"] == true {
		t.Fatalf("repeating the server's profile failed: %s", mcpResultText(result))
	}
	t.Setenv("FLINT_PROFILE", "other")
	rejected(list(defaultOptions(), map[string]any{"profile": "default"}), "profile")
	t.Setenv("FLINT_PROFILE", "")
	// An unpinned profile can change, unless the change drops a guard.
	if result := list(defaultOptions(), map[string]any{"profile": "other"}); result["isError"] == true {
		t.Fatalf("unpinned profile choice failed: %s", mcpResultText(result))
	}
	if err := os.WriteFile(filepath.Join(app.ConfigDir, "config.json"), []byte(strings.Replace(config, `"default_profile":"default"`, `"default_profile":"guarded"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	rejected(list(defaultOptions(), map[string]any{"profile": "other"}), "profile")
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
		if _, failed := call["error"]; !failed {
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
	controlsOf := func(name string) map[string]any {
		t.Helper()
		schema, e := schemaForMCPTool(mustMCPTool(t, app, name), mcpPolicy{allowLive: true, allowDestructive: true})
		if e != nil {
			t.Fatal(e)
		}
		controls, _ := lookupPath(schema, "properties._flint.properties")
		out, _ := controls.(map[string]any)
		return out
	}
	for _, test := range []struct {
		tool          string
		want, without []string
	}{
		{"customers.list", []string{"all", "select"}, []string{"idempotency_key", "dry_run", "wait_for", "confirm", "preview"}},
		{"customers.create", []string{"idempotency_key", "dry_run"}, []string{"all", "wait_for", "confirm", "preview"}},
		{"customers.get", []string{"wait_for", "for"}, []string{"all", "idempotency_key", "dry_run"}},
		{"webhook-endpoints.delete", []string{"confirm", "preview", "idempotency_key"}, []string{"all", "wait_for"}},
		{"listen", []string{"max_events", "for"}, []string{"all", "select"}},
		{"help", []string{"jq"}, []string{"live", "merchant", "profile", "timeout"}},
	} {
		controls := controlsOf(test.tool)
		for _, name := range test.want {
			if _, ok := controls[name]; !ok {
				t.Errorf("%s lacks _flint.%s", test.tool, name)
			}
		}
		for _, name := range test.without {
			if _, ok := controls[name]; ok {
				t.Errorf("%s advertises inapplicable _flint.%s", test.tool, name)
			}
		}
	}
	for _, tool := range app.mcpAllTools() {
		schema, _ := schemaForMCPTool(tool, mcpPolicy{allowLive: true, allowDestructive: true})
		controls, _ := lookupPath(schema, "properties._flint.properties")
		for _, inert := range []string{"debug", "quiet", "color", "progress", "open"} {
			if _, ok := controls.(map[string]any)[inert]; ok {
				t.Errorf("%s advertises _flint.%s, which does nothing over MCP", tool.CanonicalName, inert)
			}
		}
		if description := fmt.Sprint(lookupPathValue(schema, "properties._flint.description")); len(description) > 60 {
			t.Errorf("%s control description is long: %s", tool.CanonicalName, description)
		}
		// No enum description may merely restate its value.
		walkJSONSchemas(schema, func(node map[string]any) {
			values, _ := node["enum"].([]any)
			descriptions, _ := node["x-enum-descriptions"].([]any)
			for i, description := range descriptions {
				if i < len(values) && description != "" && strings.EqualFold(strings.Trim(fmt.Sprint(description), "."), strings.ReplaceAll(fmt.Sprint(values[i]), "_", " ")) {
					t.Errorf("%s keeps a restating enum description %q", tool.CanonicalName, description)
				}
			}
		})
	}
}

func TestMCPQueryParametersKeepTheirContract(t *testing.T) {
	app, _, _ := testApp(t, "")
	schema, _ := schemaForMCPTool(mustMCPTool(t, app, "customers.list"), mcpPolicy{})
	if enum := fmt.Sprint(lookupPathValue(schema, "properties.sort_by.enum")); !strings.Contains(enum, "created_at") {
		t.Fatalf("sort_by enum = %s", enum)
	}
	if got := fmt.Sprint(lookupPathValue(schema, "properties.page_size.default")); got != "20" {
		t.Fatalf("page_size default = %s", got)
	}
	if got := fmt.Sprint(lookupPathValue(schema, "properties.created_after.format")); got != "date-time" {
		t.Fatalf("created_after format = %s", got)
	}
	if err := validateMCPArguments(mustMCPTool(t, app, "customers.list"), map[string]any{"sort_by": "nonsense"}); err == nil {
		t.Fatal("an undocumented sort_by value passed validation")
	}
	enums := 0
	for _, tool := range app.mcpAllTools() {
		if !tool.Supports.Pagination {
			continue
		}
		toolSchema, _ := schemaForMCPTool(tool, mcpPolicy{})
		if _, ok := lookupPath(toolSchema, "properties.status.enum"); ok {
			enums++
		}
	}
	if enums < 20 {
		t.Fatalf("only %d list tools keep a status enum", enums)
	}
	get, _ := schemaForMCPTool(mustMCPTool(t, app, "customers.get"), mcpPolicy{})
	if description := fmt.Sprint(lookupPathValue(get, "properties.customer_id.description")); !strings.Contains(description, "cus_") {
		t.Fatalf("customer_id description = %s", description)
	}
}

func TestMCPDescriptionsSayWhatTitlesDoNot(t *testing.T) {
	app, _, _ := testApp(t, "")
	tools, _ := mcpListedTools(t, app, mcpAllToolsOptions(t), latestMCPProtocolVersion)
	byName := map[string]string{}
	sameAsTitle := 0
	for _, tool := range tools {
		description := tool["description"].(string)
		byName[tool["name"].(string)] = description
		if strings.TrimSuffix(description, ".") == tool["title"] {
			sameAsTitle++
		}
		if len(description) > 450 {
			t.Errorf("%s description is %d bytes: %s", tool["name"], len(description), description)
		}
		if strings.Contains(description, "has_more") && !strings.Contains(description, "no has_more") {
			t.Errorf("%s mentions has_more: %s", tool["name"], description)
		}
	}
	if sameAsTitle > len(tools)/10 {
		t.Errorf("%d of %d descriptions only repeat the title", sameAsTitle, len(tools))
	}
	for name, wants := range map[string][]string{
		"payment-intents_create":   {"Provide exactly one of", "{amount_money, payment_options}", "order", "minor units"},
		"checkout-sessions_create": {"order_id", "quick_pay_item", "subscription_plan_id"},
		"refunds_create":           {"at least one of", "payment_intent_id"},
		"products_create":          {"default_variant", "{options, variants}"},
		"bundles_update":           {"expected_version is required when sending"},
		"gift-cards_transitions":   {"Set action to one of: freeze, unfreeze, close", "reason"},
		"customers_list":           {"next_page_token", "page_token"},
		"listen":                   {"_flint.max_events or _flint.for"},
	} {
		for _, want := range wants {
			if !strings.Contains(byName[name], want) {
				t.Errorf("%s description lacks %q: %s", name, want, byName[name])
			}
		}
	}
}

func TestMCPHelpForAToolFollowsItsInputSchema(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, topic := range []string{"customers_create", "customers.create"} {
		result := mcpCall(t, app, defaultOptions(), "help", map[string]any{"topic": topic})
		text := mcpResultText(result)
		if result["isError"] == true || strings.Contains(text, "--input") || strings.Contains(text, "flint customers create") {
			t.Fatalf("help %s is not MCP-oriented: %s", topic, text)
		}
		schema, _ := schemaForMCPTool(mustMCPTool(t, app, "customers.create"), mcpPolicy{})
		required := mcpStrings(schema["required"])
		data, _ := lookupPath(result["structuredContent"], "data")
		if fmt.Sprint(lookupPathValue(data, "required")) != fmt.Sprint(required) || lookupPathValue(data, "name") != "customers_create" {
			t.Fatalf("help required = %v, schema required = %v", lookupPathValue(data, "required"), required)
		}
		for _, argument := range lookupPathValue(data, "arguments").([]map[string]any) {
			if argument["required"] != containsString(required, argument["name"].(string)) {
				t.Errorf("help says %s required=%v", argument["name"], argument["required"])
			}
		}
	}
	group := mcpCall(t, app, defaultOptions(), "help", map[string]any{"topic": "customers"})
	if text := mcpResultText(group); group["isError"] == true || !strings.Contains(text, "customers_create") {
		t.Fatalf("group help = %s", text)
	}
	controls := mcpCall(t, app, defaultOptions(), "help", map[string]any{"topic": "controls"})
	if text := mcpResultText(controls); !strings.Contains(text, "_flint.idempotency_key") {
		t.Fatalf("controls help = %s", text)
	}
	input := mcpCall(t, app, defaultOptions(), "schema_input", map[string]any{"command": "customers_create"})
	schema, _ := schemaForMCPTool(mustMCPTool(t, app, "customers.create"), mcpPolicy{})
	if fmt.Sprint(input["structuredContent"]) != fmt.Sprint(schema) {
		t.Fatal("schema_input differs from the advertised input schema")
	}
	output := mcpCall(t, app, defaultOptions(), "schema_output", map[string]any{"command": "invoices_create"})
	if output["isError"] == true {
		t.Fatalf("schema_output for a tool outside the set failed: %s", mcpResultText(output))
	}
}

func TestMCPMissingCredentialAdviceFitsAClientConfig(t *testing.T) {
	app, _, _ := testApp(t, "")
	t.Setenv("FLINT_API_KEY", "")
	result := mcpCall(t, app, defaultOptions(), "customers_list", map[string]any{})
	text := mcpResultText(result)
	if result["isError"] != true || !strings.HasPrefix(text, "No Flint credential is configured for this MCP server.") || !strings.Contains(text, "FLINT_API_KEY") || !strings.Contains(text, "restart") || strings.Contains(text, "flint auth login") {
		t.Fatalf("missing credential = %s", text)
	}
}

func TestMCPRetryGuidanceNamesTheIdempotencyControl(t *testing.T) {
	cmd := &Command{CanonicalName: "customers.create", Mutation: true}
	value := map[string]any{"error": map[string]any{
		"type": "unavailable_error", "code": "SERVICE_UNAVAILABLE", "message": "Flint is unavailable.",
		"details": map[string]any{
			"idempotency_key": "flint-cli-abc",
			"remediation":     map[string]any{"next_actions": []any{map[string]any{"reason_message": "Retry the same command with --idempotency-key flint-cli-abc.", "command": "flint customers create --idempotency-key flint-cli-abc"}}},
		},
	}}
	result := mcpCommandError(cmd, mcpCallResult{value: value, exit: ExitAPI}, mcpPolicy{}, "")
	text := mcpResultText(result)
	firstLine, _, _ := strings.Cut(text, "\n")
	if !strings.Contains(firstLine, `_flint.idempotency_key set to "flint-cli-abc"`) {
		t.Fatalf("message = %s", firstLine)
	}
	if !strings.Contains(text, "Retry the same command with _flint.idempotency_key flint-cli-abc.") || !strings.Contains(text, "flint customers create --idempotency-key flint-cli-abc") {
		t.Fatalf("details = %s", text)
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
	if result["isError"] == true || !strings.HasPrefix(last, "Warnings:\n") || !strings.Contains(last, "could not record resource history") {
		t.Fatalf("result = %#v", result)
	}
	if _, ok := result["structuredContent"].(map[string]any)["data"]; !ok {
		t.Fatal("warnings must not change structuredContent")
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

func TestMCPAPIToolDocumentsItsBody(t *testing.T) {
	server, _ := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	schema, _ := schemaForMCPTool(mustMCPTool(t, app, "api"), mcpPolicy{})
	if got := lookupPathValue(schema, "properties.body.type"); got != "object" {
		t.Fatalf("api body schema = %v", lookupPathValue(schema, "properties.body"))
	}
	opts := mcpAllToolsOptions(t)
	result := mcpCall(t, app, opts, "api", map[string]any{"method": "POST", "path": "/v1/customers", "body": map[string]any{"email": "a@example.com"}, "_flint": map[string]any{"dry_run": "client"}})
	if got := lookupPathValue(result["structuredContent"], "data.body.email"); got != "a@example.com" {
		t.Fatalf("api body result = %s", mcpResultText(result))
	}
	legacy := mcpCall(t, app, opts, "api", map[string]any{"method": "POST", "path": "/v1/customers", "email": "b@example.com", "_flint": map[string]any{"dry_run": "client"}})
	if got := lookupPathValue(legacy["structuredContent"], "data.body.email"); got != "b@example.com" {
		t.Fatalf("legacy api body result = %s", mcpResultText(legacy))
	}
	mixed := mcpCall(t, app, opts, "api", map[string]any{"method": "POST", "path": "/v1/customers", "email": "b@example.com", "body": map[string]any{}, "_flint": map[string]any{"dry_run": "client"}})
	if mixed["isError"] != true || !strings.Contains(mcpResultText(mixed), "inside body") {
		t.Fatalf("mixed api body result = %s", mcpResultText(mixed))
	}
}

func TestMCPEventCatalogIsCompactAndFilterable(t *testing.T) {
	app, _, _ := testApp(t, "")
	compact := mcpCall(t, app, defaultOptions(), "schema_events", map[string]any{})
	text := mcpResultText(compact)
	if compact["isError"] == true || len(text) > 64<<10 || !strings.Contains(text, "payment_intent.succeeded") {
		t.Fatalf("compact catalog is %d bytes (error=%v)", len(text), compact["isError"])
	}
	if help := mcpResultText(mcpCall(t, app, defaultOptions(), "help", map[string]any{"topic": "events"})); len(help) > 64<<10 || !strings.Contains(help, "event_types") {
		t.Fatalf("help events is %d bytes", len(help))
	}
	filtered := mcpCall(t, app, defaultOptions(), "schema_events", map[string]any{"event_type": "payment_intent"})
	webhooks, _ := lookupPath(filtered["structuredContent"], "webhooks")
	if filtered["isError"] == true || len(webhooks.(map[string]any)) == 0 {
		t.Fatalf("filtered catalog = %.300s", mcpResultText(filtered))
	}
	for eventType := range webhooks.(map[string]any) {
		if !strings.HasPrefix(eventType, "payment_intent.") {
			t.Fatalf("payment_intent filter returned %s", eventType)
		}
	}
	unknown := mcpCall(t, app, defaultOptions(), "schema_events", map[string]any{"event_type": "payment_intnet"})
	if unknown["isError"] != true || !strings.Contains(mcpResultText(unknown), "payment_intent") {
		t.Fatalf("unknown event type = %s", mcpResultText(unknown))
	}
}

func TestMCPRefusesOversizedReadResults(t *testing.T) {
	read := &Command{CanonicalName: "customers.list"}
	big := map[string]any{"data": strings.Repeat("x", mcpMaxResultBytes)}
	refused := mcpResultTooLarge(read, nil, big)
	if refused == nil || !strings.Contains(mcpResultText(refused), "_flint.select") {
		t.Fatalf("oversized read = %#v", refused)
	}
	if mcpResultTooLarge(&Command{CanonicalName: "customers.create", Mutation: true}, nil, big) != nil {
		t.Fatal("a completed write must return its result")
	}
}

func TestMCPCallsReportProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
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
	conn.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"customers_list","arguments":{"_flint":{"all":true}},"_meta":{"progressToken":"tok"}}}`)
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

func TestMCPEmptyToolsUsesDefaultWithLazyRegistry(t *testing.T) {
	app, _, _ := testApp(t, "")
	app.Registry = newLocalRegistry()
	opts := mcpServeOptions(t, "--tools", "")
	if err := app.validateMCPServeOptions(opts); err != nil {
		t.Fatal(err)
	}
	defaults, size := mcpListedTools(t, app, defaultOptions(), latestMCPProtocolVersion)
	empty, emptySize := mcpListedTools(t, app, opts, latestMCPProtocolVersion)
	if app.Registry.localOnly || size != emptySize || fmt.Sprint(defaults) != fmt.Sprint(empty) {
		t.Fatal("empty --tools must load and expose the same API tools as the default set")
	}
}

func TestMCPErrorEnvelopePreservesCoreDetailsAndRemediation(t *testing.T) {
	cmd := &Command{CanonicalName: "customers.create", Mutation: true}
	err := &CLIError{
		ExitCode: ExitAPI, Type: "unavailable_error", Code: "SERVICE_UNAVAILABLE", Message: "Unavailable.", HTTPStatus: http.StatusServiceUnavailable,
		Details: map[string]any{
			"type": "unavailable_error", "code": "SERVICE_UNAVAILABLE", "message": "Unavailable.",
			"details":     map[string]any{"existing": "preserved"},
			"remediation": map[string]any{"next_actions": []any{map[string]any{"reason_message": "Retry with --timeout 1m."}}},
		},
	}
	envelope := mcpCLIErrorEnvelope(withIdempotencyRetryContext(err, "flint-cli-retry-test"))
	result := mcpCommandError(cmd, mcpCallResult{value: envelope, exit: ExitAPI}, mcpPolicy{}, "")
	text := mcpResultText(result)
	_, raw, _ := strings.Cut(text, "\n\n")
	var details map[string]any
	if err := json.Unmarshal([]byte(raw), &details); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]any{
		"result.error.http_status":             float64(http.StatusServiceUnavailable),
		"result.error.retryable":               true,
		"result.error.details.existing":        "preserved",
		"result.error.details.idempotency_key": "flint-cli-retry-test",
	} {
		if got, _ := lookupPath(details, path); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if !strings.Contains(text, "_flint.timeout 1m") || strings.Contains(text, "--timeout") || strings.Contains(text, "--idempotency-key") {
		t.Fatalf("untranslated remediation: %s", text)
	}
	if !strings.Contains(text, `_flint.idempotency_key set to "flint-cli-retry-test"`) {
		t.Fatalf("missing retry key advice: %s", text)
	}
	for _, test := range []struct {
		code    string
		details map[string]any
	}{
		{"UNKNOWN_COMMAND", suggestionDetails("customer", []string{"customers"})},
		{"INVALID_REQUEST_BODY", map[string]any{"problems": []any{map[string]any{"path": "$.email", "message": "Invalid email."}}}},
	} {
		t.Run(test.code, func(t *testing.T) {
			envelope := mcpCLIErrorEnvelope(&CLIError{ExitCode: ExitUsage, Type: "usage_error", Code: test.code, Message: "Invalid input.", Details: test.details})
			if _, ok := lookupPathValue(envelope, "error.details").(map[string]any); !ok {
				t.Fatalf("details must be an object: %#v", envelope)
			}
			result := mcpCommandError(cmd, mcpCallResult{value: envelope, exit: ExitUsage}, mcpPolicy{}, "")
			if !strings.Contains(mcpResultText(result), `"`+test.code+`"`) {
				t.Fatalf("error code lost: %#v", result)
			}
		})
	}
}
