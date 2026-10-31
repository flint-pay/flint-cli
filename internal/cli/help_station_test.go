package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

func offlineHelpSearchApp(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	app, out, errOut := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_API_KEY", "")
	t.Setenv("FLINT_ACCESS_TOKEN", "")
	app.ConfigDir = t.TempDir() // Search also works with no saved profile.
	t.Setenv("FLINT_HELP_URL", "ignored://retired-setting")
	app.LoadCredential = func(string) (string, error) { t.Fatal("offline search read a credential"); return "", nil }
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("offline search sent a request"); return nil, nil })}
	return app, out, errOut
}

func TestHelpSearchOfflineOutput(t *testing.T) {
	for _, tc := range []struct {
		query, want, kind string
		top               int
	}{
		{"forward webhooks to localhost", "listen", "command", 1},
		{"exit codes", "exit-codes", "topic", 1},
		{"api schema for a command", "schema.input", "command", 3},
		{"POST /v1/refunds", "refunds.create", "command", 1},
		{"payment succeeded event", "payment_intent.succeeded", "event", 1},
		{"test card", "test-cards", "topic", 1},
	} {
		for _, jsonOutput := range []bool{false, true} {
			app, out, errOut := offlineHelpSearchApp(t)
			argv := append([]string{"help", "search"}, strings.Fields(tc.query)...)
			if jsonOutput {
				argv = append(argv, "--output", "json")
			}
			if exit := app.Run(argv); exit != ExitOK || errOut.Len() != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, errOut)
			}
			if !jsonOutput {
				label := strings.ReplaceAll(tc.want, ".", " ")
				if tc.kind == "event" {
					label = "help events " + tc.want
				}
				if !strings.Contains(out.String(), label) || !strings.Contains(out.String(), "flint support open") {
					t.Fatal(out)
				}
				continue
			}
			var value map[string]any
			if err := json.Unmarshal(out.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			data := value["data"].(map[string]any)
			results := data["results"].([]any)
			if data["query"] != tc.query || len(results) == 0 || len(results) > 8 || data["ask_url"] != nil {
				t.Fatal(data)
			}
			found := false
			for i, raw := range results {
				result := raw.(map[string]any)
				if i < tc.top && result["name"] == tc.want && result["type"] == tc.kind {
					found = true
				}
				if result["type"] == "command" {
					cmd, _ := app.Registry.ByName(result["name"].(string))
					want := map[string]any{"name": cmd.Name, "type": "command", "command": strings.Join(cmd.Path, " "), "description": cmd.Description, "section": cmd.Section, "method": cmd.Method, "api_path": cmd.APIPath, "operation_id": cmd.OperationID}
					if !reflect.DeepEqual(result, want) {
						t.Fatalf("got=%v want=%v", result, want)
					}
				} else {
					want := "flint help " + result["name"].(string)
					if result["type"] == "event" {
						want = "flint help events " + result["name"].(string)
					}
					if result["command"] != want || result["description"] == "" {
						t.Fatal(result)
					}
				}
			}
			if !found {
				t.Fatalf("%s: want %s in top %d: %v", tc.query, tc.want, tc.top, results)
			}
		}
	}
}

func TestHelpSearchNoMatchesAndMissingQuery(t *testing.T) {
	app, out, errOut := offlineHelpSearchApp(t)
	if exit := app.Run([]string{"help", "search"}); exit != ExitUsage {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, errOut)
	}
	for _, jsonOutput := range []bool{false, true} {
		app, out, errOut = offlineHelpSearchApp(t)
		argv := []string{"help", "search", "zzzxxyyq"}
		if jsonOutput {
			argv = append(argv, "--output", "json")
		}
		if exit := app.Run(argv); exit != ExitOK || errOut.Len() != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, errOut)
		}
		if !strings.Contains(out.String(), "Try fewer words") || !strings.Contains(out.String(), "flint help --all") {
			t.Fatal(out)
		}
		if jsonOutput && !strings.Contains(out.String(), `"results":[]`) {
			t.Fatal(out)
		}
	}
}

func TestHelpSearchEveryResultResolves(t *testing.T) {
	app, _, _ := offlineHelpSearchApp(t)
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	filter := app.cliSearchFilter()
	for _, entry := range index.Entries {
		if entry.ID == "safety" || entry.ID == "history.clear" {
			if filter.Exposed(entry) {
				t.Fatalf("synthetic or unsupported result exposed: %s", entry.ID)
			}
			continue
		}
		if !filter.Exposed(entry) {
			t.Fatalf("terminal entry missing: %s", entry.ID)
		}
		switch entry.Type {
		case "operation":
			cmd, ok := app.Registry.ByName(entry.ID)
			if !ok {
				t.Fatal(entry.ID)
			}
			resolved := app.Registry.resolveWords(cmd.Path[1:])
			if resolved.Command != cmd {
				t.Fatalf("invalid command link %v", cmd.Path)
			}
		case "event":
			resolved := app.Registry.commandTree().Resolve([]string{"events", entry.ID})
			if resolved.Topic != "events" || !reflect.DeepEqual(resolved.Arguments, []string{entry.ID}) {
				t.Fatalf("invalid event link %s: %+v", entry.ID, resolved)
			}
			if _, err := eventHelpDetail(entry.ID); err != nil {
				t.Fatalf("event link has no help %s: %v", entry.ID, err)
			}
		case "topic":
			if resolved := app.Registry.commandTree().Resolve([]string{entry.ID}); resolved.Topic != entry.ID {
				t.Fatalf("invalid topic link %s: %+v", entry.ID, resolved)
			}
		}
	}
}

func TestHelpSearchMatchesMCPSearch(t *testing.T) {
	app, _, _ := offlineHelpSearchApp(t)
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	cliFilter := app.cliSearchFilter()
	cliFilter.Limit = len(index.Entries)
	mcpFilter := opindex.Filter{Limit: len(index.Entries), Exposed: func(e opindex.Entry) bool { return e.MCP }}
	for _, query := range []string{"refund an order", "forward webhooks to localhost", "exit codes", "api schema for a command", "POST /v1/refunds", "payment succeeded event", "test card", "orders.line-items"} {
		cliHits, _ := index.Search(query, cliFilter)
		mcpHits, _ := index.Search(query, mcpFilter)
		cliIDs, mcpIDs := []string{}, []string{}
		for _, hit := range cliHits {
			if mcpFilter.Exposed(hit.Entry) {
				cliIDs = append(cliIDs, hit.ID)
			}
		}
		for _, hit := range mcpHits {
			if cliFilter.Exposed(hit.Entry) {
				mcpIDs = append(mcpIDs, hit.ID)
			}
		}
		if !reflect.DeepEqual(cliIDs, mcpIDs) {
			t.Fatalf("%s CLI=%v MCP=%v", query, cliIDs, mcpIDs)
		}
	}
}

func TestHelpSearchIgnoresConfiguredCredentials(t *testing.T) {
	for _, credential := range []string{"synthetic_merchant_key", "flint_cses_synthetic"} {
		app, out, errOut := offlineHelpSearchApp(t)
		t.Setenv("FLINT_ACCESS_TOKEN", credential)
		if exit := app.Run([]string{"help", "search", "exit", "codes"}); exit != ExitOK || errOut.Len() != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, errOut)
		}
	}
}

func TestHelpSearchDoesNotShadowOfflineHelpTopics(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	if exit := app.Run([]string{"help", "test-cards"}); exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout.String(), "pm_card_visa") {
		t.Errorf("offline topic output = %s", stdout)
	}
}

func TestSupportComposerURLBuilding(t *testing.T) {
	base := "https://help.withflintpay.com"
	for _, test := range []struct {
		name     string
		composer supportComposerContext
		want     string
	}{
		{
			name:     "bare",
			composer: supportComposerContext{},
			want:     base + "/new?kind=question&source=cli.support",
		},
		{
			name:     "every field in a fixed order",
			composer: supportComposerContext{Area: "webhooks", Private: true, RequestID: "req_123", Resource: "whep_123", Environment: "sandbox"},
			want:     base + "/new?kind=question&area=webhooks&visibility=private&env=sandbox&resource=whep_123&request_id=req_123&source=cli.support",
		},
		{
			name:     "public thread omits visibility",
			composer: supportComposerContext{Area: "payouts", RequestID: "req_456"},
			want:     base + "/new?kind=question&area=payouts&request_id=req_456&source=cli.support",
		},
		{
			name:     "live environment",
			composer: supportComposerContext{Environment: "live"},
			want:     base + "/new?kind=question&env=live&source=cli.support",
		},
		{
			name:     "values are escaped",
			composer: supportComposerContext{RequestID: "req 1&2", Resource: "pi_a/b"},
			want:     base + "/new?kind=question&resource=pi_a%2Fb&request_id=req+1%262&source=cli.support",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := buildSupportComposerURL(base, test.composer); got != test.want {
				t.Errorf("buildSupportComposerURL()\n got %s\nwant %s", got, test.want)
			}
		})
	}
}

func TestSupportOpenPrintsTheComposerLinkWithoutOpeningABrowser(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	t.Setenv("FLINT_HELP_URL", "ignored://retired-setting")
	t.Setenv("FLINT_API_KEY", "flint_test_key")
	exit := app.Run([]string{
		"support", "open",
		"--request-id", "req_123",
		"--resource", "pi_123",
		"--area", "payments",
		"--private",
		"--no-open",
	})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	want := "https://help.withflintpay.com/new?kind=question&area=payments&visibility=private&env=sandbox&resource=pi_123&request_id=req_123&source=cli.support"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("stdout = %s, want the composer link %s", stdout, want)
	}
	if !strings.Contains(stdout.String(), "The CLI does not create the thread.") {
		t.Errorf("stdout does not say posting happens in the composer: %s", stdout)
	}
}

func TestSupportOpenPrefillsTitleAndBody(t *testing.T) {
	for _, fields := range []struct{ title, body string }{
		{title: "Webhook failure & retries?"},
		{body: "First line\n\nSecond line: + & # = café"},
		{title: "Payment failed", body: "  Keep indentation\nand trailing whitespace  "},
	} {
		t.Run(fields.title+fields.body, func(t *testing.T) {
			app, stdout, stderr := testApp(t, "")
			exit := app.Run([]string{"support", "open", "--title", fields.title, "--body", fields.body, "--no-open", "--output", "json"})
			if exit != ExitOK || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			var envelope struct {
				Data struct {
					URL, Title, Body string
					Opened           bool
				} `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			target, err := url.Parse(envelope.Data.URL)
			if err != nil {
				t.Fatal(err)
			}
			query := target.Query()
			if query.Get("title") != fields.title || query.Get("body") != fields.body {
				t.Fatalf("prefill did not round-trip: %s", target)
			}
			if query.Has("title") != (fields.title != "") || query.Has("body") != (fields.body != "") {
				t.Fatalf("empty prefill should be omitted: %s", target)
			}
			if envelope.Data.Title != fields.title || envelope.Data.Body != fields.body || envelope.Data.Opened {
				t.Fatalf("unexpected output: %s", stdout)
			}
		})
	}
}

func TestSupportOpenAIAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  bool
	}{
		{name: "default"},
		{name: "enabled", flags: []string{"--ai-agent"}, want: true},
		{name: "explicit true", flags: []string{"--ai-agent=true"}, want: true},
		{name: "explicit false", flags: []string{"--ai-agent=false"}},
		{name: "last wins", flags: []string{"--ai-agent", "--ai-agent=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, stdout, stderr := testApp(t, "")
			args := append([]string{"support", "open", "--no-open", "--output", "json"}, tc.flags...)
			if exit := app.Run(args); exit != ExitOK {
				t.Fatalf("exit=%d stderr=%s", exit, stderr)
			}
			var envelope struct {
				Data struct {
					URL     string `json:"url"`
					AIAgent *bool  `json:"ai_agent"`
				} `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Data.AIAgent == nil || *envelope.Data.AIAgent != tc.want {
				t.Fatalf("unexpected AI agent metadata: %s", stdout)
			}
			target, err := url.Parse(envelope.Data.URL)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want {
				if target.Query().Get("ai_agent") != "true" {
					t.Fatalf("missing AI agent prefill: %s", target)
				}
			} else if target.Query().Has("ai_agent") {
				t.Fatalf("false should omit prefill: %s", target)
			}
		})
	}
}

func TestSupportOpenDerivesTheEnvironmentFromTheCredential(t *testing.T) {
	for key, want := range map[string]string{
		"flint_test_key": "env=sandbox",
		"flint_live_key": "env=live",
		"opaque_key":     "",
	} {
		t.Run(key, func(t *testing.T) {
			app, stdout, stderr := testApp(t, "")
			t.Setenv("FLINT_HELP_URL", "ignored://retired-setting")
			t.Setenv("FLINT_API_KEY", key)
			if exit := app.Run([]string{"support", "open", "--no-open", "--output", "json"}); exit != ExitOK {
				t.Fatalf("exit=%d stderr=%s", exit, stderr)
			}
			var envelope struct {
				Data struct {
					URL         string `json:"url"`
					Opened      bool   `json:"opened"`
					Environment string `json:"environment"`
					Visibility  string `json:"visibility"`
				} `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("decode output: %v: %s", err, stdout)
			}
			if envelope.Data.Opened {
				t.Error("--no-open still reported an opened browser")
			}
			if envelope.Data.Visibility != "public" {
				t.Errorf("visibility = %q", envelope.Data.Visibility)
			}
			if want == "" {
				if strings.Contains(envelope.Data.URL, "env=") {
					t.Errorf("unrecognized credential produced an environment: %s", envelope.Data.URL)
				}
				if envelope.Data.Environment != "" {
					t.Errorf("environment = %q", envelope.Data.Environment)
				}
				return
			}
			if !strings.Contains(envelope.Data.URL, want) {
				t.Errorf("url = %s, want %s", envelope.Data.URL, want)
			}
		})
	}
}

func TestSupportProductAreasMatchHelpComposer(t *testing.T) {
	want := []string{
		"checkout", "payments", "payouts", "disputes", "invoices", "subscriptions",
		"payment_links", "api", "webhooks", "dashboard", "documentation", "flint_billing", "account", "sales",
	}
	if !slices.Equal(supportProductAreas, want) {
		t.Fatalf("support product areas = %v, want %v", supportProductAreas, want)
	}
	app, stdout, stderr := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_HELP_URL", "ignored://retired-setting")
	if exit := app.Run([]string{"support", "open", "--area", "disputes", "--no-open", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	var envelope struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(envelope.Data.URL)
	if err != nil {
		t.Fatal(err)
	}
	if link.Query().Get("area") != "disputes" {
		t.Fatalf("composer URL lost disputes area: %s", envelope.Data.URL)
	}
	command, _ := app.Registry.ByName("support.open")
	for _, arg := range command.Arguments {
		if arg.Name == "area" && strings.Contains(arg.Description, "disputes") {
			return
		}
	}
	t.Fatal("support help does not list disputes")
}

func TestSupportOpenRejectsAnUndocumentedProductArea(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	if exit := app.Run([]string{"support", "open", "--area", "billing", "--no-open"}); exit != ExitUsage {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stderr.String(), "payment_links") {
		t.Errorf("error does not list the accepted areas: %s", stderr)
	}
	for _, area := range supportProductAreas {
		app, _, stderr := testApp(t, "")
		if exit := app.Run([]string{"support", "open", "--area", area, "--no-open"}); exit != ExitOK {
			t.Errorf("area %s was rejected: exit=%d stderr=%s", area, exit, stderr)
		}
	}
}

func TestSupportOpenIsNotOfferedToAgentsOverMCP(t *testing.T) {
	command, ok := NewRegistry().ByName("support.open")
	if !ok {
		t.Fatal("support.open is not registered")
	}
	if mcpCommandExposed(command) {
		t.Error("support.open is exposed as an MCP tool; an agent cannot use a browser window")
	}
	search, ok := NewRegistry().ByName("help.search")
	if !ok {
		t.Fatal("help.search is not registered")
	}
	if !mcpCommandExposed(search) {
		t.Error("help.search should be available to agents over MCP")
	}
}

func TestHelpSearchUnknownTopicKeepsSuggestions(t *testing.T) {
	app, out, _ := offlineHelpSearchApp(t)
	exit, value := runJSON(t, app, out, "help", "custmers")
	if exit != ExitUsage || lookupPathValue(value, "error.code") != "UNKNOWN_COMMAND" {
		t.Fatalf("unknown help topic exit=%d value=%v", exit, value)
	}
	if !strings.Contains(fmt.Sprint(lookupPathValue(value, "error.details.suggestions")), "customers") || !strings.Contains(fmt.Sprint(lookupPathValue(value, "error.message")), "flint help search custmers") {
		t.Fatalf("unknown help topic lost discovery guidance: %v", value)
	}
}
