package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/flint-pay/flint-cli/internal/schemaview"
)

func TestArgumentValueMetadata(t *testing.T) {
	registry := NewRegistry()
	for _, tc := range []struct {
		command, argument string
		want              []string
	}{
		{"config.set", "key", []string{"profile", "merchant"}},
		{"config.unset", "key", []string{"merchant"}},
		{"api-keys.create", "access", []string{"full", "read_only", "restricted"}},
		{"api-keys.update", "access", []string{"full", "read_only", "restricted"}},
		{"support.open", "area", supportProductAreas},
		{"listen", "event_type", listenEventTypes()},
	} {
		command, _ := registry.ByName(tc.command)
		found := false
		for _, arg := range command.Arguments {
			if arg.Name == tc.argument {
				found = true
				if !reflect.DeepEqual(arg.Enum, tc.want) {
					t.Errorf("%s %s enum=%v want=%v", tc.command, tc.argument, arg.Enum, tc.want)
				}
			}
		}
		if !found {
			t.Errorf("missing %s %s", tc.command, tc.argument)
		}
		catalog := commandDocument(command, command.Name)
		catalogFound := false
		for _, raw := range catalog["arguments"].([]any) {
			argument := raw.(map[string]any)
			if argument["name"] == tc.argument {
				catalogFound = true
				if got := mcpSchemaStrings(argument["enum"]); !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s catalog %s enum=%v want=%v", tc.command, tc.argument, got, tc.want)
				}
			}
		}
		if !catalogFound {
			t.Errorf("catalog missing %s %s", tc.command, tc.argument)
		}
	}
	for _, tc := range []struct{ path, method, id string }{
		{"/v1/things/{state}", "get", "getThing"},
	} {
		operation := openAPIOperation{OperationID: tc.id, Parameters: []map[string]any{{"name": "state", "in": "path", "required": true, "description": "State to fetch.", "schema": map[string]any{"type": "string", "enum": []any{"open", "closed"}}}}}
		args := publicAPIArguments(tc.path, operation)
		// Path enums must be copied just as query enums are; this fixture has no
		// published operation ID to look up during registry enrichment.
		if len(args) != 1 || !reflect.DeepEqual(args[0].Enum, []string{"open", "closed"}) {
			t.Fatalf("path enum missing: %v", args)
		}
	}
}

func TestArgumentEnumValidationBeforeRequests(t *testing.T) {
	for _, argv := range [][]string{
		{"orders", "list", "--status", "opne"},
		{"api-keys", "create", "--name", "review", "--access", "partial"},
		{"api-keys", "create", "--name", "review", "--scope", "not.a.scope"},
		{"config", "set", "unknown", "value"},
		{"support", "open", "--area", "unknown"},
	} {
		app, out, _ := testApp(t, "http://127.0.0.1:1")
		app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("invalid enum sent a request"); return nil, nil })}
		exit, result := runJSON(t, app, out, argv...)
		code, _ := lookupPath(result, "error.code")
		allowed, _ := lookupPath(result, "error.details.allowed")
		if exit != ExitUsage || code != "INVALID_FLAG_VALUE" || allowed == nil || !strings.Contains(fmt.Sprint(result), "flint upgrade") {
			t.Fatalf("%v exit=%d result=%v", argv, exit, result)
		}
		if argv[0] == "orders" && !strings.Contains(fmt.Sprint(lookupPathValue(result, "error.details.suggestions")), "open") {
			t.Fatalf("misspelled enum has no useful suggestion: %v", result)
		}
	}
	if err := validateListenEventType([]string{"future_resource.future_event"}); err != nil {
		t.Fatal("listen must pass newer names to the server:", err)
	}
}

func TestArgumentValueHelpRendering(t *testing.T) {
	for _, count := range []int{1, 6, 7, 12, 13} {
		values := []string{}
		for i := 0; i < count; i++ {
			values = append(values, fmt.Sprintf("value%d", i))
		}
		var output bytes.Buffer
		printArgumentHelp(&output, &Command{CanonicalName: "fixture"}, Arg{Name: "state", Flag: "--state", Type: "string", Description: "State to filter.", Enum: values})
		text := output.String()
		if count <= 6 && !strings.Contains(text, strings.Join(values, "|")) {
			t.Fatal(text)
		}
		if count > 6 && count <= 12 && !strings.Contains(text, "Values: "+strings.Join(values, ", ")) {
			t.Fatal(text)
		}
		if count > 12 && !strings.Contains(text, "13 values: flint schema command fixture --output json") {
			t.Fatal(text)
		}
	}
	var events bytes.Buffer
	printArgumentHelp(&events, &Command{CanonicalName: "fixture"}, Arg{Name: "enabled_event", Flag: "--enabled-event", BodyPath: "enabled_events", Enum: listenEventTypes()})
	if !strings.Contains(events.String(), "see flint help events") {
		t.Fatal(events.String())
	}
	for _, query := range []string{"", "created_after"} {
		var output bytes.Buffer
		command := &Command{CanonicalName: "fixture", Arguments: []Arg{{Name: "created_after", Flag: "--created-after", Type: "time", Query: query}}}
		applyArgumentValues(command)
		printArgumentHelp(&output, command, command.Arguments[0])
		if got := strings.Contains(output.String(), "Also accepts today"); got != (query != "") {
			t.Fatalf("query=%q: %s", query, output.String())
		}
	}
	for _, argv := range [][]string{{"config", "set", "--help"}, {"config", "unset", "--help"}} {
		app, out, _ := testApp(t, "http://127.0.0.1:1")
		if exit := app.Run(argv); exit != ExitOK {
			t.Fatal(exit)
		}
		if !strings.Contains(out.String(), "merchant") {
			t.Fatal(out)
		}
	}
}

func TestHelpWinsBeforeTerminator(t *testing.T) {
	for _, argv := range [][]string{
		{"subscription-plans", "create", "--name", "x", "--help"},
		{"customers", "create", "--bogus", "value", "--help"},
		{"orders", "get", "--help"},
		{"orders", "get", "bad-prefix", "--help"},
		{"orders", "list", "--status", "invalid", "--help"},
		{"orders", "list", "--page-size", "bad", "--help"},
		{"orders", "list", "--timeout", "--help"},
		{"--page-size", "0", "--help"},
		{"orders", "list", "--credential-store", "invalid", "-h"},
	} {
		if cmd, _, help, err := parseInvocation(NewRegistry(), argv); err != nil || !help || cmd == nil {
			t.Fatalf("%v: cmd=%v help=%v err=%v", argv, cmd, help, err)
		}
	}
	for _, argv := range [][]string{
		{"--credential-store", "invalid", "orders", "list", "--help"},
		{"--output", "yaml", "orders", "list", "--help"},
		{"--bogus", "value", "orders", "list", "--help"},
		{"--bogus=value", "orders", "list", "-h"},
	} {
		if cmd, _, help, err := parseInvocation(NewRegistry(), argv); err != nil || !help || cmd == nil || cmd.Name != "orders.list" {
			t.Fatalf("flags before command %v: cmd=%v help=%v err=%v", argv, cmd, help, err)
		}
	}
	for _, argv := range [][]string{{"nonesuch", "--help"}, {"orders", "nonesuch", "--help"}} {
		if _, _, _, err := parseInvocation(NewRegistry(), argv); err == nil || err.Code != "UNKNOWN_COMMAND" {
			t.Fatalf("unknown command %v help=%v", argv, err)
		}
	}
	if _, opts, help, err := parseInvocation(NewRegistry(), []string{"help", "--", "--help"}); err != nil || help || !reflect.DeepEqual(opts.Positionals, []string{"--help"}) {
		t.Fatalf("terminator help=%v opts=%+v err=%v", help, opts, err)
	}
	app, out, _ := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_CREDENTIAL_STORE", "invalid")
	if exit := app.Run([]string{"orders", "get", "invalid-id", "--help", "--output", "json"}); exit != ExitOK || !strings.Contains(out.String(), `"canonical_name":"orders.get"`) {
		t.Fatalf("environment invalidated help: exit=%d output=%s", exit, out)
	}
	if _, opts, help, err := parseInvocation(NewRegistry(), []string{"orders", "list", "--page-size", "bad", "--help", "--output", "json"}); err != nil || !help || opts.Output != "json" {
		t.Fatalf("JSON help opts=%+v err=%v", opts, err)
	}
}

func TestQueryArrayStyleMetadata(t *testing.T) {
	for _, explode := range []bool{true, false} {
		operation := openAPIOperation{Parameters: []map[string]any{{"name": "status", "in": "query", "style": "form", "explode": explode, "schema": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []any{"open", "closed"}}}}}}
		args := publicAPIArguments("/v1/things", operation)
		if len(args) != 1 {
			t.Fatal(args)
		}
		arg := args[0]
		if arg.Style != "form" || arg.Explode == nil || *arg.Explode != explode {
			t.Fatal(arg)
		}
		opts := defaultOptions()
		opts.Raw["status"] = []string{"open, closed"}
		if err := validateArgumentValues(&Command{Arguments: args}, &opts); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		printArgumentHelp(&output, &Command{CanonicalName: "fixture"}, arg)
		if explode && !strings.Contains(output.String(), "Repeat this flag") || !explode && !strings.Contains(output.String(), "Separate values with commas") {
			t.Fatal(output.String())
		}
		app, _, _ := testApp(t, "http://127.0.0.1:1")
		cmd := &Command{Name: "fixture.list", CanonicalName: "fixture.list", Method: "GET", APIPath: "/v1/things", Arguments: args}
		req, err := app.prepareRequest(t.Context(), cmd, opts, ResolvedConfig{}, "", "http://127.0.0.1:1")
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := url.Parse(req.Path)
		want := []string{"open", "closed"}
		if !explode {
			want = []string{"open,closed"}
		}
		if got := parsed.Query()["status"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("explode=%v query=%v want=%v", explode, got, want)
		}
	}
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	cmd, _ := app.Registry.ByName("resource-timelines.get")
	opts := defaultOptions()
	opts.Positionals = []string{"pi_review"}
	for _, arg := range cmd.Arguments {
		if arg.Query == "include" && len(arg.Enum) > 1 {
			opts.Raw["include"] = arg.Enum[:2]
		}
	}
	if len(opts.Raw["include"]) != 2 {
		t.Fatal("timeline include fixture missing")
	}
	req, err := app.prepareRequest(t.Context(), cmd, opts, ResolvedConfig{}, "", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(req.Path)
	want := strings.Join(opts.Raw["include"], ",")
	if got := parsed.Query()["include"]; !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("non-exploded include=%v, want %s", got, want)
	}
}

func TestHelpDescriptionLint(t *testing.T) {
	// No upstream parameter descriptions may be empty after the spec refresh.
	upstreamEmptyParameterDescriptions := map[string]bool{}
	sentenceBoundary := regexp.MustCompile(`[.!?]\s+`)
	check := func(name, text string) {
		if strings.Contains(text, ".;") {
			t.Errorf("%s contains a punctuation splice: %s", name, text)
		}
		seen := map[string]bool{}
		for _, sentence := range sentenceBoundary.Split(strings.TrimSpace(text), -1) {
			normalized := strings.ToLower(strings.Join(strings.Fields(sentence), " "))
			if normalized != "" && seen[normalized] {
				t.Errorf("%s repeats a sentence: %s", name, text)
			}
			seen[normalized] = true
		}
	}
	for _, cmd := range NewRegistry().commandTree().Commands {
		if strings.TrimSpace(cmd.Description) == "" {
			t.Errorf("%s has no description", cmd.Name)
		}
		check(cmd.Name, cmd.Description)
		for _, arg := range cmd.Arguments {
			location := "cli"
			name := arg.Name
			if arg.Query != "" {
				location = "query"
				name = arg.Query
			} else if arg.Positional > 0 && !commandOwned(cmd) {
				location = "path"
			} else if arg.BodyPath != "" {
				location = "body"
				name = arg.BodyPath
			}
			key := cmd.OperationID + "/" + location + "/" + name
			if strings.TrimSpace(arg.Description) == "" {
				if _, ok := upstreamEmptyParameterDescriptions[key]; ok {
					upstreamEmptyParameterDescriptions[key] = true
				} else {
					t.Errorf("%s %s has no description", cmd.Name, arg.Name)
				}
			}
			check(cmd.Name+" "+arg.Name, arg.Description)
		}
	}
	for key, empty := range upstreamEmptyParameterDescriptions {
		if !empty {
			t.Errorf("stale empty-description baseline entry %s; remove it", key)
		}
	}
}

func TestQueryArrayHistoryReferences(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	resolved := ResolvedConfig{ProfileName: "default", Environment: "sandbox"}
	if err := app.saveHistory(History{Entries: []HistoryEntry{{ID: "ord_history", ResourceType: "order", Profile: "default", Environment: "sandbox"}}}); err != nil {
		t.Fatal(err)
	}
	cmd, _ := app.Registry.ByName("orders.list")
	opts := defaultOptions()
	opts.Raw["order-id"] = []string{"@last.ord,ord_explicit"}
	req, err := app.prepareRequest(t.Context(), cmd, opts, resolved, "", "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(req.Path)
	if got := parsed.Query()["order_id"]; !reflect.DeepEqual(got, []string{"ord_history", "ord_explicit"}) {
		t.Fatalf("query history values=%v", got)
	}
}

func TestMCPQueryTimeDescriptions(t *testing.T) {
	app := discoveryFixture(t)
	cmd, _ := app.Registry.ByName("customers.list")
	reader, original, snapshotErr := commandDiscoverySchema(cmd, true)
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	view, err := reader.Describe(original, schemaview.Options{})
	if err != nil {
		t.Fatal(err)
	}
	field := discoveryField(t, view, "created_after")
	if description, _ := field["description"].(string); !strings.Contains(description, queryTimeGuidance) || field["format"] != nil {
		t.Fatalf("discovery time guidance missing: %v", field)
	}
	full, schemaErr := schemaForCommand(cmd, true)
	if schemaErr != nil {
		t.Fatal(schemaErr)
	}
	property := lookupPathValue(full, "properties.created_after").(map[string]any)
	if description, _ := property["description"].(string); !strings.Contains(description, queryTimeGuidance) || property["format"] != nil {
		t.Fatalf("full schema time guidance missing: %v", property)
	}
	typed, typedErr := schemaForMCPTool(cmd, app.mcpPolicy(mcpServeOptions(t, "--tools", "all")))
	if typedErr != nil {
		t.Fatal(typedErr)
	}
	if format := lookupPathValue(typed, "properties.created_after.format"); format != nil {
		t.Fatalf("typed relative query format=%v", format)
	}

	catalog := commandDocument(cmd, cmd.Name)
	for _, raw := range catalog["arguments"].([]any) {
		argument := raw.(map[string]any)
		if argument["name"] != "created_after" {
			continue
		}
		description := argument["description"].(string)
		if strings.Contains(description, ".;") || strings.Count(description, queryTimeGuidance) != 1 {
			t.Fatalf("catalog time guidance malformed: %s", description)
		}
		var output bytes.Buffer
		for _, arg := range cmd.Arguments {
			if arg.Name == "created_after" {
				printArgumentHelp(&output, cmd, arg)
			}
		}
		if strings.Count(output.String(), queryTimeGuidance) != 1 {
			t.Fatalf("human time guidance malformed: %s", output.String())
		}
		return
	}
	t.Fatal("created_after is missing from the catalog")
}
