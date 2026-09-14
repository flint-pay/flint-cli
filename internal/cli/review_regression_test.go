package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestReviewRawAPIQueryFlagsPreserveFilters(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			io.WriteString(w, authContextJSON("sandbox"))
			return
		}
		queries = append(queries, r.URL.RawQuery)
		io.WriteString(w, `{"data":[],"next_page_token":""}`)
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	if exit := app.Run([]string{"api", "get", "/v1/customers?email=review%40example.com&page_size=1", "--page-size", "2", "--page-token", "next", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if len(queries) != 1 || queries[0] != "email=review%40example.com&page_size=2&page_token=next" {
		t.Fatalf("request queries=%v; filters and pagination flags must share one query string", queries)
	}
}

func TestReviewRawAPIExpansionsUseRouteContract(t *testing.T) {
	for _, expansion := range []string{"customer", "unsupported"} {
		_, _, _, err := parseInvocation(NewRegistry(), []string{"api", "get", "/v1/orders/ord_review", "--expand", expansion})
		if expansion == "customer" && err != nil {
			t.Fatalf("documented expansion rejected: %v", err)
		}
		if expansion == "unsupported" && (err == nil || err.Code != "UNSUPPORTED_EXPANSION") {
			t.Fatalf("undocumented expansion must be rejected: %v", err)
		}
	}
}

func TestReviewMCPKeepsLargeRequestIDs(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	app.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":9007199254740993,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}` + "\n")
	if exit := app.serveMCP(defaultOptions()); exit != ExitOK || !strings.Contains(stdout.String(), `"id":9007199254740993`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

func TestReviewJSONNumbersRejectTrailingContent(t *testing.T) {
	for _, raw := range []string{`{"data":1} {"data":2}`, `{"data":1} garbage`} {
		var value any
		if err := decodeJSONNumbers([]byte(raw), &value); err == nil {
			t.Fatalf("accepted trailing content: %s", raw)
		}
	}
}

func TestReviewArgumentTerminator(t *testing.T) {
	for _, positionals := range [][]string{{"--help"}, {"--live"}, {"--unknown"}, {"--"}, {"--output", "json"}} {
		t.Run(strings.Join(positionals, " "), func(t *testing.T) {
			argv := append([]string{"help", "search", "--"}, positionals...)
			_, opts, help, err := parseInvocation(NewRegistry(), argv)
			if err != nil || help || !reflect.DeepEqual(opts.Positionals, positionals) || opts.Live || opts.Output != "human" {
				t.Fatalf("parse %v: positionals=%v help=%v live=%v output=%s err=%v", argv, opts.Positionals, help, opts.Live, opts.Output, err)
			}
		})
	}
}

func TestReviewHelpFalseRunsCommand(t *testing.T) {
	app, stdout, stderr := testApp(t, "")
	if exit := app.Run([]string{"version", "--help=false", "--output=json"}); exit != ExitOK {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"cli_version":"test"`) {
		t.Fatalf("expected version JSON, got %s", stdout)
	}
}

func TestReviewEmptyAllPagesRemainsArray(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			io.WriteString(w, authContextJSON("sandbox"))
			return
		}
		io.WriteString(w, `{"data":[],"next_page_token":""}`)
	}))
	defer server.Close()
	for _, extra := range [][]string{nil, {"--select", "customer_id"}, {"--jq", ".data | length"}} {
		app, stdout, stderr := testApp(t, server.URL)
		argv := append([]string{"customers", "list", "--all", "--output", "json"}, extra...)
		if exit := app.Run(argv); exit != ExitOK {
			t.Fatalf("%v: exit=%d stdout=%s stderr=%s", argv, exit, stdout, stderr)
		}
		if len(extra) == 0 && !strings.Contains(stdout.String(), `"data":[]`) {
			t.Fatalf("empty list must stay an array: %s", stdout)
		}
	}
}

func TestReviewHumanOutputEmptyMetadataKeys(t *testing.T) {
	app, stdout, _ := testApp(t, "")
	value := map[string]any{"data": map[string]any{"metadata": map[string]any{"": "empty key", "notes.": "trailing dot"}}}
	if err := app.writeResult(value, nil, defaultOptions()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"empty key", "trailing dot"} {
		if !bytes.Contains(stdout.Bytes(), []byte(want)) {
			t.Fatalf("missing %q in output %s", want, stdout)
		}
	}
}

func TestReviewLargeIntegersSurviveOutput(t *testing.T) {
	const amount = "9007199254740993"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			io.WriteString(w, authContextJSON("sandbox"))
			return
		}
		io.WriteString(w, `{"data":{"amount":`+amount+`}}`)
	}))
	defer server.Close()
	for _, extra := range [][]string{nil, {"--field", "data.amount"}, {"--select", "amount"}, {"--jq", ".data.amount"}} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			app, stdout, stderr := testApp(t, server.URL)
			argv := append([]string{"customers", "get", "cus_review", "--output", "json"}, extra...)
			if exit := app.Run(argv); exit != ExitOK || !strings.Contains(stdout.String(), amount) {
				t.Fatalf("exit=%d stdout=%s stderr=%s; expected exact integer %s", exit, stdout, stderr, amount)
			}
		})
	}
	t.Run("MCP", func(t *testing.T) {
		app, _, _ := testApp(t, server.URL)
		cmd, _ := app.Registry.ByName("customers.get")
		result, _, exit := app.callMCPCommand(t.Context(), cmd, map[string]any{"customer_id": "cus_review"}, defaultOptions())
		raw, err := json.Marshal(result)
		if err != nil || exit != ExitOK || !bytes.Contains(raw, []byte(amount)) {
			t.Fatalf("exit=%d result=%s err=%v; expected exact integer %s", exit, raw, err, amount)
		}
	})
}
