package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func dxCustomerServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/auth-context":
			fmt.Fprint(w, authContextJSON("sandbox"))
		case r.URL.Path == "/v1/customers":
			fmt.Fprint(w, `{"data":[{"customer_id":"cus_1"},{"customer_id":"cus_2"}],"next_page_token":"tok_2","request_id":"req_list"}`)
		default:
			fmt.Fprint(w, `{"data":{"customer_id":"cus_1","email":"a@example.com"},"request_id":"req_get"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestJQWritesOneValuePerLine(t *testing.T) {
	server := dxCustomerServer(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"json many", []string{"--jq", ".data[].customer_id", "--output", "json"}, "\"cus_1\"\n\"cus_2\"\n"},
		{"human many", []string{"--jq", ".data[].customer_id"}, "cus_1\ncus_2\n"},
		{"json none", []string{"--jq", ".data[] | select(.customer_id == \"x\")", "--output", "json"}, ""},
		{"json one", []string{"--jq", ".data[0].customer_id", "--output", "json"}, "\"cus_1\"\n"},
		{"json array", []string{"--jq", "[.data[].customer_id]", "--output", "json"}, "[\"cus_1\",\"cus_2\"]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, stdout, stderr := testApp(t, server.URL)
			if exit := app.Run(append([]string{"customers", "list"}, tc.args...)); exit != ExitOK {
				t.Fatalf("exit=%d stderr=%s", exit, stderr)
			}
			if stdout.String() != tc.want {
				t.Fatalf("stdout=%q want %q", stdout, tc.want)
			}
		})
	}
}

func TestHumanListReportsMorePages(t *testing.T) {
	server := dxCustomerServer(t)
	app, _, stderr := testApp(t, server.URL)
	if exit := app.Run([]string{"customers", "list"}); exit != ExitOK {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stderr.String(), "--page-token tok_2") {
		t.Fatalf("stderr=%q", stderr)
	}
	for _, extra := range [][]string{{"--quiet"}, {"--output", "json"}} {
		app, _, stderr := testApp(t, server.URL)
		if exit := app.Run(append([]string{"customers", "list"}, extra...)); exit != ExitOK || stderr.Len() != 0 {
			t.Fatalf("%v: exit=%d stderr=%q", extra, exit, stderr)
		}
	}
}

func TestCommonFlagGuesses(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		exit       int
		stdout     string
		suggestion string
	}{
		{args: []string{"-h"}, exit: ExitOK, stdout: "Flint Pay command-line interface"},
		{args: []string{"customers", "list", "-h"}, exit: ExitOK, stdout: "List customers"},
		{args: []string{"--version", "--output", "json"}, exit: ExitOK, stdout: `"cli_version":"test"`},
		{args: []string{"-v", "--output", "json"}, exit: ExitUsage, stdout: `"code":"UNKNOWN_FLAG"`},
		{args: []string{"customers", "list", "--json", "--output", "json"}, exit: ExitUsage, suggestion: "--output json"},
		{args: []string{"customers", "list", "--outptu", "json", "--output", "json"}, exit: ExitUsage, suggestion: "--output"},
		{args: []string{"customers", "list", "-x", "--output", "json"}, exit: ExitUsage, stdout: "long form"},
		{args: []string{"customers", "lst", "--output", "json"}, exit: ExitUsage, suggestion: "customers list"},
		{args: []string{"custmers", "--output", "json"}, exit: ExitUsage, suggestion: "customers"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			app, stdout, _ := testApp(t, "")
			if exit := app.Run(tc.args); exit != tc.exit {
				t.Fatalf("exit=%d stdout=%s", exit, stdout)
			}
			if tc.stdout != "" && !strings.Contains(stdout.String(), tc.stdout) {
				t.Fatalf("stdout=%s", stdout)
			}
			if tc.suggestion != "" {
				var body struct {
					Error struct {
						Details struct {
							Suggestions []string `json:"suggestions"`
						} `json:"details"`
					} `json:"error"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &body); err != nil || len(body.Error.Details.Suggestions) == 0 || body.Error.Details.Suggestions[0] != tc.suggestion {
					t.Fatalf("stdout=%s", stdout)
				}
			}
		})
	}
}

func TestUnknownCommandWithoutCloseMatchPointsToHelp(t *testing.T) {
	app, _, stderr := testApp(t, "")
	if exit := app.Run([]string{"foo"}); exit != ExitUsage {
		t.Fatalf("exit=%d", exit)
	}
	if got := stderr.String(); !strings.Contains(got, "flint --help") || strings.Contains(got, "Did you mean") {
		t.Fatalf("stderr=%q", got)
	}
}

func TestNegativeNumberAndStdinStayPositional(t *testing.T) {
	for _, token := range []string{"-", "-5"} {
		if isShortFlag(token) {
			t.Fatalf("%q treated as a flag", token)
		}
	}
}

func TestConnectionFailureReportsCause(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	app, stdout, _ := testApp(t, url)
	exit := app.Run([]string{"customers", "list", "--timeout", "1s", "--output", "json"})
	// The dial error wording differs by OS; it must name the failed dial, not a timeout.
	if exit != ExitNetwork || !strings.Contains(stdout.String(), `"code":"NETWORK_ERROR"`) || !strings.Contains(stdout.String(), "dial tcp") || strings.Contains(stdout.String(), "timed out") {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
}

func TestWaitForUndocumentedFieldFailsFast(t *testing.T) {
	server := dxCustomerServer(t)
	app, stdout, _ := testApp(t, server.URL)
	started := time.Now()
	exit := app.Run([]string{"customers", "get", "cus_1", "--wait-for", "stauts=active", "--for", "30s", "--output", "json"})
	if exit != ExitUsage || !strings.Contains(stdout.String(), "UNKNOWN_OUTPUT_FIELD") {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("unknown wait field waited for the bound")
	}
	if !outputSchemaDeclaresPath(mustCommand(t, "payment-intents.get"), "status") {
		t.Fatal("payment intent status should be a declared wait field")
	}
}

func TestIntegerFlagErrorExplainsMinorUnits(t *testing.T) {
	server := dxCustomerServer(t)
	app, stdout, _ := testApp(t, server.URL)
	exit := app.Run([]string{"payment-links", "create", "--name", "T", "--amount", "10.00", "--currency", "USD", "--output", "json"})
	if exit != ExitUsage || !strings.Contains(stdout.String(), "smallest currency unit") || strings.Contains(stdout.String(), "strconv") {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
}

func TestHelpShowsPositionalsHistoryRefsAndTopics(t *testing.T) {
	app, stdout, _ := testApp(t, "")
	app.Run([]string{"payment-intents", "get", "--help"})
	if !strings.Contains(stdout.String(), "flint payment-intents get <payment_intent_id> [flags]") || !strings.Contains(stdout.String(), "@last.pi") {
		t.Fatalf("help=%s", stdout)
	}
	app, stdout, _ = testApp(t, "")
	if exit := app.Run([]string{"help", "nope", "--output", "json"}); exit != ExitUsage || !strings.Contains(stdout.String(), "test-cards") {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
	app, stdout, _ = testApp(t, "")
	app.Run([]string{"--help"})
	if !strings.Contains(stdout.String(), "FLINT_OUTPUT") {
		t.Fatalf("root help does not document FLINT_OUTPUT")
	}
}

func mustCommand(t *testing.T, name string) *Command {
	t.Helper()
	cmd, ok := NewRegistry().ByName(name)
	if !ok {
		t.Fatalf("missing command %s", name)
	}
	return cmd
}
