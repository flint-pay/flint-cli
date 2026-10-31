package cli

import (
	"bytes"
	"encoding/json"
	testflags "flag"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var updateCommandNames = testflags.Bool("update", false, "Update the route-derived command naming golden")

func TestCommandNamesFollowPathRule(t *testing.T) {
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	byOperation := map[string][]*Command{}
	for _, command := range registry.Commands {
		if command.OperationID != "" {
			byOperation[command.OperationID] = append(byOperation[command.OperationID], command)
		}
	}
	generated, excluded := 0, 0
	for path, methods := range doc.Paths {
		for method, operation := range methods {
			reason := commandExclusion(&doc, path, operation)
			if reason != "" {
				excluded++
				want := 0
				if boundOperations[operation.OperationID] != "" {
					want = 1
				}
				if len(byOperation[operation.OperationID]) != want {
					t.Errorf("%s (%s) has %d commands", operation.OperationID, reason, len(byOperation[operation.OperationID]))
				}
				continue
			}
			generated++
			name, kind, err := commandName(&doc, path, strings.ToUpper(method), operation)
			if err != nil {
				t.Fatal(err)
			}
			commands := byOperation[operation.OperationID]
			if len(commands) != 1 {
				t.Errorf("%s has %d commands", operation.OperationID, len(commands))
				continue
			}
			c := commands[0]
			if c.Name != name || c.Kind != kind || c.Description != strings.TrimSpace(operation.Summary) {
				t.Errorf("%s: got %s/%s/%q, want %s/%s/%q", operation.OperationID, c.Name, c.Kind, c.Description, name, kind, operation.Summary)
			}
			for _, arg := range c.Arguments {
				if arg.Positional > 0 && !arg.Required {
					t.Errorf("optional path parameter: %s %s", name, arg.Name)
				}
			}
		}
	}
	if generated != 611 || excluded != 12 || len(registry.Commands) != 642 {
		t.Fatalf("generated=%d excluded=%d total=%d", generated, excluded, len(registry.Commands))
	}
	for _, command := range curatedCommandDefinitions() {
		if command.OperationID != "" && boundOperations[command.OperationID] == "" {
			t.Errorf("API operation defined outside generator: %s", command.OperationID)
		}
	}
}

func TestCommandNameRuleCases(t *testing.T) {
	doc := &openAPIDocument{}
	doc.Components.Schemas = map[string]map[string]any{"Page": {"allOf": []any{map[string]any{"properties": map[string]any{"data": map[string]any{"type": "array"}, "next_page_token": map[string]any{"type": "string"}}}}}}
	for _, tc := range []struct {
		path, method, id, want, kind string
		page                         bool
	}{
		{"/v1/things/{id}", "GET", "getThing", "things.get", "item", false},
		{"/v1/things/{id}", "PATCH", "updateThing", "things.update", "item", false},
		{"/v1/things/{id}", "PUT", "replaceThing", "things.replace", "item", false},
		{"/v1/things/{id}", "DELETE", "deleteThing", "things.delete", "item", false},
		{"/v1/things/{id}", "POST", "createThing", "", "", false},
		{"/v1/things", "GET", "fetchThings", "things.list", "collection", true},
		{"/v1/things", "GET", "listThing", "things.get", "singleton", false},
		{"/v1/things", "POST", "createThing", "things.create", "collection", false},
		{"/v1/merchant", "GET", "getMerchant", "merchant.get", "singleton", false},
		{"/v1/merchant", "PATCH", "updateMerchant", "merchant.update", "singleton", false},
		{"/v1/orders/{id}/current", "GET", "getCurrent", "orders.current.get", "singleton", false},
		{"/v1/orders/{id}/pay", "POST", "payOrder", "orders.pay", "action", false},
		{"/v1/webhook-endpoints/{id}/rotate-secret", "POST", "rotateWebhookEndpointSecret", "webhook-endpoints.rotate-secret", "action", false},
		{"/v1/gift-cards/{id}/cash-out", "POST", "cashOutGiftCard", "gift-cards.cash-out", "action", false},
		{"/v1/orders/{id}/discounts", "POST", "applyOrderDiscount", "orders.discounts.create", "collection", false},
		{"/v1/gift-cards/{id}/transitions", "POST", "transitionGiftCard", "gift-cards.transitions.create", "collection", false},
		{"/v1/me/flint-wallet/cards/store-setups", "POST", "createSetup", "me.flint-wallet.cards.store-setups.create", "collection", false},
		{"/v1/openapi.json", "GET", "getOpenAPI", "openapi.get", "singleton", false},
		{"/v1/Things", "GET", "getThing", "", "", false},
		{"/v1/thing_one", "GET", "getThing", "", "", false},
		{"/v1/thing.pdf", "GET", "getThing", "", "", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			schema := map[string]any{"type": "object"}
			if tc.page {
				schema = map[string]any{"$ref": "#/components/schemas/Page"}
			}
			op := openAPIOperation{OperationID: tc.id, FlintCLIAction: "ignored", Responses: map[string]map[string]any{"200": {"content": map[string]any{"application/json": map[string]any{"schema": schema}}}}}
			got, kind, err := commandName(doc, tc.path, tc.method, op)
			if tc.want == "" {
				if err == nil {
					t.Fatal("expected naming error")
				}
				return
			}
			if err != nil || got != tc.want || kind != tc.kind {
				t.Fatalf("%s/%s %v, want %s/%s", got, kind, err, tc.want, tc.kind)
			}
		})
	}
}

func TestCommandNameCollectionResponseShape(t *testing.T) {
	array := map[string]any{"type": "array", "items": map[string]any{"type": "object"}}
	data := func(schema map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{"data": schema}}
	}
	page := data(array)
	page["properties"].(map[string]any)["next_page_token"] = map[string]any{"type": "string"}
	doc := &openAPIDocument{}
	doc.Components.Schemas = map[string]map[string]any{
		"Array":        array,
		"List":         data(array),
		"ComposedList": {"allOf": []any{map[string]any{"$ref": "#/components/schemas/List"}}},
		"Loop":         {"$ref": "#/components/schemas/Loop"},
	}
	for _, tc := range []struct {
		name   string
		schema map[string]any
		list   bool
	}{
		{"inline list without page token", data(array), true},
		{"inline list with page token", page, true},
		{"response reference", map[string]any{"$ref": "#/components/schemas/List"}, true},
		{"response allOf reference", map[string]any{"$ref": "#/components/schemas/ComposedList"}, true},
		{"data reference", data(map[string]any{"$ref": "#/components/schemas/Array"}), true},
		{"data allOf reference", data(map[string]any{"allOf": []any{map[string]any{"$ref": "#/components/schemas/Array"}}}), true},
		{"page token without array", map[string]any{"properties": map[string]any{"data": map[string]any{"type": "object"}, "next_page_token": map[string]any{"type": "string"}}}, false},
		{"page token without data", map[string]any{"properties": map[string]any{"next_page_token": map[string]any{"type": "string"}}}, false},
		{"nested array is not data", data(map[string]any{"properties": map[string]any{"items": array}}), false},
		{"bare array is not data", array, false},
		{"cyclic response reference", map[string]any{"$ref": "#/components/schemas/Loop"}, false},
		{"cyclic data reference", data(map[string]any{"$ref": "#/components/schemas/Loop"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := openAPIOperation{OperationID: "fetchThings", Responses: map[string]map[string]any{"200": {"content": map[string]any{"application/json": map[string]any{"schema": tc.schema}}}}}
			want, kind := "things.get", "singleton"
			if tc.list {
				want, kind = "things.list", "collection"
			}
			name, gotKind, err := commandName(doc, "/v1/things", "GET", op)
			if err != nil || name != want || gotKind != kind {
				t.Fatalf("name=%s kind=%s error=%v, want %s/%s", name, gotKind, err, want, kind)
			}
		})
	}
}

func TestRefreshedUnpaginatedListsKeepNames(t *testing.T) {
	registry := NewRegistry()
	for id, want := range map[string]string{
		"listBalances":                "balances.list",
		"listMerchantBillingBalances": "merchant-billing-balances.list",
		"listMeFlintWalletCards":      "me.flint-wallet.cards.list",
	} {
		command, ok := registry.ByName(want)
		if !ok || command.OperationID != id || command.Kind != "collection" {
			t.Fatalf("%s missing collection %s: %+v", id, want, command)
		}
		if command.Supports.Pagination {
			t.Errorf("%s has no pagination parameters", id)
		}
		for _, arg := range command.Arguments {
			if arg.Query == "page_size" || arg.Query == "page_token" {
				t.Errorf("%s exposes unsupported query parameter %s", id, arg.Query)
			}
		}
		_, opts, help, e := parseInvocation(registry, append(strings.Split(want, "."), "--all"))
		if e == nil {
			e = validateOptions(command, &opts, help)
		}
		if e == nil || e.Code != "UNSUPPORTED_FLAG" {
			t.Errorf("%s accepted --all without pagination: %v", id, e)
		}
	}
	// Pagination capability still comes from the actual page_size parameter.
	command, ok := registry.ByName("orders.list")
	if !ok || !command.Supports.Pagination {
		t.Fatal("orders list lost its published pagination")
	}
}

func TestCommandNameCollisions(t *testing.T) {
	for _, commands := range [][]*Command{
		{{Name: "a.b.list", OperationID: "first"}, {Name: "a.b.list", OperationID: "second"}},
		{{Name: "a-b.c.get", OperationID: "first"}, {Name: "a.b-c.get", OperationID: "second"}},
		{{Name: "a.get", OperationID: "first"}, {Name: "a.get.b", OperationID: "second"}},
	} {
		err := validateCommandNames(commands)
		if err == nil || !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
			t.Fatalf("collision must name both operations: %v", err)
		}
	}
	if err := validateCommandNames([]*Command{{Name: "x.create", Kind: "action", OperationID: "createX"}}); err == nil {
		t.Fatal("standard action verb accepted")
	}
}

func TestCommandTreeInvariants(t *testing.T) {
	var commands []*Command
	for _, c := range NewRegistry().Commands {
		if !commandOwned(c) {
			commands = append(commands, c)
		}
		for _, a := range c.Arguments {
			if strings.HasPrefix(a.Flag, "--body-") {
				t.Errorf("unexpected body collision: %s %s", c.Name, a.Flag)
			}
		}
	}
	if err := validateCommandNames(commands); err != nil {
		t.Fatal(err)
	}
	doc, _ := loadOpenAPI()
	exceptions := map[string]bool{"createOnboardingAPIKey": false}
	for _, c := range commands {
		if c.Method != "POST" || c.Kind != "collection" {
			continue
		}
		methods := doc.Paths[c.APIPath]
		structural := false
		if get, ok := methods["get"]; ok {
			structural = operationReturnsArray(&doc, get)
		}
		for path := range doc.Paths {
			rest, ok := strings.CutPrefix(path, c.APIPath+"/")
			if ok && strings.HasPrefix(rest, "{") && !strings.Contains(rest, "/") {
				structural = true
			}
		}
		if structural || strings.HasSuffix(c.APIPath, "s") {
			continue
		}
		if _, ok := exceptions[c.OperationID]; !ok {
			t.Errorf("nonplural collection without a structural signal: %s", c.OperationID)
		} else {
			exceptions[c.OperationID] = true
		}
	}
	for id, seen := range exceptions {
		if !seen {
			t.Errorf("stale plural-lint exception %s", id)
		}
	}
}

func TestShortcutsClosed(t *testing.T) {
	got := map[string]string{}
	for _, c := range NewRegistry().Commands {
		for _, alias := range c.Aliases {
			got[alias] = c.Name
		}
	}
	want := map[string]string{"login": "auth.login", "logout": "auth.logout", "reauth": "auth.reauth", "whoami": "auth.status"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shortcuts %v, want %v", got, want)
	}
	for _, name := range []string{"checkout.create", "checkout.get", "checkout.list", "payment.create", "sandboxes.test-key", "invoices.late-fees.assess", "update"} {
		if _, ok := NewRegistry().ByName(name); ok {
			t.Errorf("removed alias still resolves: %s", name)
		}
	}
}

func TestNoGeneratedNamesInCode(t *testing.T) {
	names := map[string]bool{}
	for _, c := range NewRegistry().Commands {
		if !commandOwned(c) {
			names[c.Name] = true
		}
	}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fs := token.NewFileSet()
		file, err := parser.ParseFile(fs, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err == nil && names[value] {
				t.Errorf("%s: generated command literal %q; key API behavior by operation ID", fs.Position(lit.Pos()), value)
			}
			return true
		})
	}
}

func TestCommandTreeGolden(t *testing.T) {
	type entry struct {
		Name        string `json:"name"`
		OperationID string `json:"operation_id,omitempty"`
		Kind        string `json:"kind"`
		Section     string `json:"section"`
		Audience    string `json:"audience"`
	}
	var entries []entry
	for _, c := range NewRegistry().commandTree().Commands {
		kind := c.Kind
		if kind == "" {
			kind = "cli"
		}
		entries = append(entries, entry{c.Name, c.OperationID, kind, c.Section, c.Audience})
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path := "testdata/command-tree.golden.json"
	if *updateCommandNames {
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatal("command names changed; review and run -update")
	}
}

func TestNamingExclusionsAndRemovedOrderMakeNoRequests(t *testing.T) {
	app, out, _ := testApp(t, "http://127.0.0.1:1")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("removed commands and flags must not send any request")
		return nil, nil
	})}
	for _, argv := range [][]string{{"oauth", "authorize"}, {"oauth", "token"}, {"payment-intents", "create", "--order", "ord_test"}} {
		exit, result := runJSON(t, app, out, argv...)
		code, _ := lookupPath(result, "error.code")
		if exit != ExitUsage || (code != "UNKNOWN_COMMAND" && code != "UNKNOWN_FLAG") {
			t.Fatalf("%v exit=%d result=%v", argv, exit, result)
		}
	}
}

func TestGeneratedQueryFlagsAndCompatibilityBodyFlags(t *testing.T) {
	registry := NewRegistry()
	for _, tc := range []struct{ name, flag string }{
		{"orders.list", "--customer-id"}, {"invoices.list", "--customer-id"}, {"payment-intents.list", "--customer-id"}, {"refunds.list", "--order-id"}, {"refunds.list", "--payment-intent-id"},
		{"payment-intents.create", "--customer-id"}, {"payment-intents.create", "--payment-options"}, {"api-keys.create", "--scopes"}, {"invoices.create", "--order-id"},
	} {
		c, _ := registry.ByName(tc.name)
		found := false
		for _, a := range c.Arguments {
			found = found || a.Flag == tc.flag
		}
		if !found {
			t.Errorf("%s missing %s", tc.name, tc.flag)
		}
	}
}

func TestCanonicalPrintedCommandsParse(t *testing.T) {
	// Reuse the exhaustive example check and inspect documented shell examples.
	TestEveryHelpExampleParsesAsItsDocumentedCommand(t)
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	fenced := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if !fenced || !strings.HasPrefix(line, "flint ") {
			continue
		}
		words := mcpFallbackWords(line)
		for i, word := range words {
			if word == "#" || word == "<" || word == "|" || word == "&&" {
				words = words[:i]
				break
			}
		}
		// README schema-only examples may omit required resource IDs or flags.
		words = append(words, "--help")
		if _, _, _, err := parseInvocation(registry, words[1:]); err != nil {
			t.Errorf("%s: %v", line, err)
		}
	}
}

func TestNamingSuggestionsResolveAndNeverEcho(t *testing.T) {
	registry := NewRegistry()
	for _, input := range []string{"checkout", "payment", "payments", "custmers"} {
		for _, suggestion := range registry.NounSuggestions(input) {
			if suggestion == input {
				t.Errorf("echoed %s", input)
			}
			if _, ok := registry.ByPath([]string{"flint", suggestion}); !ok && len(registry.commandsUnder([]string{"flint", suggestion})) == 0 {
				t.Errorf("unresolvable %s", suggestion)
			}
		}
	}
	for _, name := range []string{"customers", "orders.get"} {
		suggestions := commandNameSuggestions(registry, name, true)
		for _, s := range suggestions {
			if s == name {
				t.Errorf("echoed %s", name)
			}
		}
	}
}

func TestCompatibilityBodyFlagsUnchanged(t *testing.T) {
	// Captured from 02caeb3 before A1, with only createPaymentIntent --order
	// removed. A4 replaces this compatibility baseline with generated flags.
	raw, err := os.ReadFile("testdata/compatibility-body-flags.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, command := range NewRegistry().Commands {
		if commandOwned(command) {
			continue
		}
		for _, arg := range command.Arguments {
			if arg.BodyPath != "" {
				got[command.OperationID] = append(got[command.OperationID], arg.Flag)
			}
		}
	}
	for _, flags := range got {
		slices.Sort(flags)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compatibility body flags changed: got %v, want %v", got, want)
	}
}

func TestCommandExclusionRules(t *testing.T) {
	doc := &openAPIDocument{Security: []map[string][]string{{"BearerAuth": nil}}}
	for _, tc := range []struct {
		path      string
		operation openAPIOperation
		code      string
	}{
		{"/v1/oauth/future", openAPIOperation{OperationID: "futureOAuth"}, ""},
		{"/v1/oauth/future", openAPIOperation{OperationID: "futureOAuth", Security: []map[string][]string{}}, "E1"},
		{"/v1/feedback-reports/report", openAPIOperation{}, "E2"},
		{"/v1/hosted", openAPIOperation{FlintAudience: "flint_hosted"}, "E3"},
		{"/v1/auth-context", openAPIOperation{OperationID: "getAuthContext"}, "E4"},
		{"/v1/webhook-events/stream", openAPIOperation{OperationID: "streamWebhookEvents"}, "E4"},
		{"/v1/public", openAPIOperation{Security: []map[string][]string{}}, ""},
	} {
		got := commandExclusion(doc, tc.path, tc.operation)
		code, _, _ := strings.Cut(got, " ")
		if code != tc.code {
			t.Errorf("%s: %q, want %q", tc.path, got, tc.code)
		}
	}
}
