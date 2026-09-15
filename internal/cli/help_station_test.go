package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const exhaustiveHelpSearchJSON = `[{"title":"Webhook signature check fails","url":"https://help.withflintpay.com/t/webhook-signature-check-fails","slug":"webhook-signature-check-fails","status":"answered","kind":"question","productArea":"webhooks","excerpt":"Verify against the raw body.","replyCount":3,"answered":true,"updatedAt":1756720800}]`

func TestHelpSearchSendsQueryAndRendersResults(t *testing.T) {
	var gotPath, gotQuery, gotLimit, gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotQuery = request.URL.Query().Get("q")
		gotLimit = request.URL.Query().Get("limit")
		gotAuthorization = request.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
			{"title":"Webhook signature check fails","url":"https://help.withflintpay.com/t/sig","slug":"sig","status":"answered","kind":"question","productArea":"webhooks","excerpt":"Verify against the raw body.","replyCount":3,"answered":true,"updatedAt":1756720800},
			{"title":"Retry after a 409","url":"https://help.withflintpay.com/t/retry","slug":"retry","status":"open","kind":"question","productArea":"api","excerpt":"Reuse the idempotency key.","replyCount":1,"answered":false,"updatedAt":1756807200}
		]`)
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, "")
	t.Setenv("FLINT_HELP_URL", server.URL)
	if exit := app.Run([]string{"help", "search", "webhook", "signature check"}); exit != ExitOK {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if gotPath != "/api/search" {
		t.Errorf("path = %q, want /api/search", gotPath)
	}
	if gotQuery != "webhook signature check" {
		t.Errorf("q = %q, want the joined query", gotQuery)
	}
	if gotLimit != "8" {
		t.Errorf("limit = %q, want 8", gotLimit)
	}
	if gotAuthorization != "" {
		t.Errorf("Flint Help search sent a credential: %q", gotAuthorization)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %s", stderr)
	}
	output := stdout.String()
	for _, want := range []string{
		"1. Webhook signature check fails",
		"✓ answered",
		"webhooks",
		"3 replies",
		"https://help.withflintpay.com/t/sig",
		"2. Retry after a 409",
		"1 reply",
		"Ask your own: " + server.URL + "/new?kind=question&source=cli.help",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("human output is missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "2. Retry after a 409  ✓ answered") {
		t.Errorf("unanswered thread was marked answered:\n%s", output)
	}
}

func TestHelpSearchJSONOutputCarriesEveryField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, exhaustiveHelpSearchJSON)
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, "")
	t.Setenv("FLINT_HELP_URL", server.URL)
	if exit := app.Run([]string{"help", "search", "webhook", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	var envelope struct {
		Data struct {
			Query   string `json:"query"`
			AskURL  string `json:"ask_url"`
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Slug        string `json:"slug"`
				Status      string `json:"status"`
				Kind        string `json:"kind"`
				ProductArea string `json:"product_area"`
				Excerpt     string `json:"excerpt"`
				ReplyCount  int    `json:"reply_count"`
				Answered    bool   `json:"answered"`
				UpdatedAt   int64  `json:"updated_at"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode output: %v: %s", err, stdout)
	}
	if envelope.Data.Query != "webhook" {
		t.Errorf("query = %q", envelope.Data.Query)
	}
	if envelope.Data.AskURL != server.URL+"/new?kind=question&source=cli.help" {
		t.Errorf("ask_url = %q", envelope.Data.AskURL)
	}
	if len(envelope.Data.Results) != 1 {
		t.Fatalf("results = %#v", envelope.Data.Results)
	}
	result := envelope.Data.Results[0]
	if result.Title != "Webhook signature check fails" || result.Slug != "webhook-signature-check-fails" ||
		result.ProductArea != "webhooks" || result.ReplyCount != 3 || !result.Answered ||
		result.Status != "answered" || result.Kind != "question" ||
		result.Excerpt != "Verify against the raw body." || result.UpdatedAt != 1756720800 ||
		result.URL != "https://help.withflintpay.com/t/webhook-signature-check-fails" {
		t.Errorf("result = %#v", result)
	}
}

func TestHelpSearchReportsEmptyResultsAndServerFailures(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}))
	defer empty.Close()
	app, stdout, stderr := testApp(t, "")
	t.Setenv("FLINT_HELP_URL", empty.URL)
	if exit := app.Run([]string{"help", "search", "nothing", "matches"}); exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout.String(), "No Flint Help threads matched") ||
		!strings.Contains(stdout.String(), "Ask your own: ") {
		t.Errorf("empty output = %s", stdout)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer broken.Close()
	app, stdout, stderr = testApp(t, "")
	t.Setenv("FLINT_HELP_URL", broken.URL)
	if exit := app.Run([]string{"help", "search", "anything"}); exit != ExitAPI {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stderr.String(), "HTTP 502") {
		t.Errorf("stderr = %s", stderr)
	}
}

func TestHelpSearchRequiresAQueryAndAValidHelpURL(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	if exit := app.Run([]string{"help", "search"}); exit != ExitUsage {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}

	app, _, stderr = testApp(t, "")
	t.Setenv("FLINT_HELP_URL", "help.withflintpay.com")
	if exit := app.Run([]string{"help", "search", "webhooks"}); exit != ExitAuth {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr.String(), "FLINT_HELP_URL") {
		t.Errorf("stderr = %s", stderr)
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
	t.Setenv("FLINT_HELP_URL", "https://help.example.com")
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
	want := "https://help.example.com/new?kind=question&area=payments&visibility=private&env=sandbox&resource=pi_123&request_id=req_123&source=cli.support"
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
			t.Setenv("FLINT_HELP_URL", "https://help.example.com")
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
