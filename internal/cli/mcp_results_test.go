package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPResultsOneCompactValueAndNotes(t *testing.T) {
	for _, structured := range []bool{false, true} {
		for _, value := range []any{map[string]any{"data": "<a href=\"x&y\">世界</a>"}, "<>&", []any{1, "two"}} {
			result := mcpWithWarnings(mcpToolResult(value, structured, true, "sandbox"), "note: page size\nwarning: first warning\nsecond warning")
			decoded := mcpResultJSON(t, result)
			want, _ := json.Marshal(value)
			got, _ := json.Marshal(decoded)
			if string(want) != string(got) {
				t.Fatalf("result changed: %s != %s", got, want)
			}
			content := result["content"].([]map[string]any)
			text := content[0]["text"].(string)
			if strings.Contains(text, `\u003c`) || strings.Contains(text, `\u003e`) || strings.Contains(text, `\u0026`) || strings.HasSuffix(text, "\n") {
				t.Fatalf("result is escaped or not compact: %q", text)
			}
			if got := content[1]["text"]; got != "Flint environment: sandbox\nNote: page size\nWarning: first warning\nWarning: second warning" {
				t.Fatalf("notes = %q", got)
			}
			if meta, _ := result["_meta"].(map[string]any); meta[mcpEnvironmentMetaKey] != "sandbox" {
				t.Fatal(result)
			}
		}
	}
}

func TestMCPResultsCapUsesActualUTF8TextBytes(t *testing.T) {
	if mcpMaxResultBytes != 49_152 {
		t.Fatalf("read limit = %d", mcpMaxResultBytes)
	}
	// JSON quotes consume two bytes. HTML characters stay one byte each.
	for _, text := range []string{strings.Repeat("&", mcpMaxResultBytes-2), strings.Repeat("界", (mcpMaxResultBytes-2)/3)} {
		if refused := mcpResultTooLarge(nil, nil, text); refused != nil {
			t.Fatalf("result at or below the actual byte limit was refused: %s", mcpResultText(refused))
		}
	}
	tooLarge := strings.Repeat("界", mcpMaxResultBytes/3)
	refused := mcpResultTooLarge(nil, nil, tooLarge)
	if refused == nil || refused["isError"] != true {
		t.Fatal("oversized UTF-8 result accepted")
	}
	text := mcpResultText(refused)
	for _, want := range []string{"RESULT_TOO_LARGE", `"bytes":49154`, `"limit":49152`, "select", "page_size", "filters", `"retry":"none"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
}

func TestMCPResultsPaginationDefaultAndTerminalUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pageSize any
		terminal bool
		want     string
		note     bool
	}{
		{name: "MCP default", want: "10", note: true},
		{name: "MCP explicit", pageSize: 3, want: "3"},
		{name: "terminal default", terminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requested string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				requested = r.URL.Query().Get("page_size")
				fmt.Fprint(w, `{"data":[{"customer_id":"cus_page"}],"next_page_token":"page_2","request_id":"req_page"}`)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			if tc.terminal {
				if exit := app.Run([]string{"customers", "list", "--output=json", "--no-input"}); exit != ExitOK {
					t.Fatalf("terminal exit = %d", exit)
				}
			} else {
				arguments := map[string]any{}
				if tc.pageSize != nil {
					arguments["page_size"] = tc.pageSize
				}
				result := mcpCall(t, app, defaultOptions(), "flint_read", map[string]any{"operation": "customers.list", "arguments": arguments})
				if result["isError"] == true {
					t.Fatal(mcpResultText(result))
				}
				mcpResultJSON(t, result)
				if strings.Contains(mcpResultText(result), "Note: page_size is 10, the default here.") != tc.note {
					t.Fatal(mcpResultText(result))
				}
			}
			if requested != tc.want {
				t.Fatalf("page_size = %q, want %q", requested, tc.want)
			}
		})
	}
}

func TestMCPResultsAllStopsAtProjectedCap(t *testing.T) {
	for _, projected := range []bool{false, true} {
		t.Run(fmt.Sprint(projected), func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				pages++
				next := ""
				if pages < 3 {
					next = fmt.Sprintf("page_%d", pages+1)
				}
				fmt.Fprintf(w, `{"data":[{"customer_id":"cus_%d","email":%q}],"next_page_token":%q,"request_id":"req_page"}`, pages, strings.Repeat("x", 26_000), next)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			input := map[string]any{"operation": "customers.list", "all": true}
			if projected {
				input["select"] = []any{"customer_id"}
			}
			result := mcpCall(t, app, defaultOptions(), "flint_read", input)
			if projected {
				if pages != 3 || result["isError"] == true {
					t.Fatalf("pages=%d result=%s", pages, mcpResultText(result))
				}
				value := mcpResultJSON(t, result)
				if data, _ := lookupPathValue(value, "data").([]any); len(data) != 3 || strings.Contains(mcpResultText(result), `"email"`) {
					t.Fatal(value)
				}
			} else {
				text := mcpResultText(result)
				if pages != 2 || result["isError"] != true || !strings.Contains(text, "RESULT_TOO_LARGE") || !strings.Contains(text, `"pages_fetched":2`) {
					t.Fatalf("pages=%d result=%s", pages, text)
				}
			}
		})
	}
}

func TestMCPResultsOversizedWriteReturnedWithWarning(t *testing.T) {
	// Exceed both the old 16 MiB HTTP body and 32 MiB MCP stdout limits.
	response := strings.Repeat("x", mcpMaxOutputBytes+1)
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		writes++
		fmt.Fprintf(w, `{"data":{"customer_id":"cus_large","email":%q},"request_id":"req_write"}`, response)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	cmd := mustMCPTool(t, app, "customers.create")
	result := app.runMCPTool(t.Context(), cmd, map[string]any{"email": "fixture@example.com"}, defaultOptions(), mcpPolicy{}, true)
	if result["isError"] == true || writes != 1 {
		t.Fatalf("writes=%d result=%.500s", writes, mcpResultText(result))
	}
	value := mcpResultJSON(t, result)
	if lookupPathValue(value, "data.email") != response || !strings.Contains(mcpResultText(result), "over the 49,152-byte read limit; it is returned in full because the write completed.") {
		t.Fatal("completed write response or warning lost")
	}
}

func TestMCPResultsFullWriteResponseExceptionIsScoped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		status int
		mcp    bool
	}{
		{name: "MCP auth GET", method: http.MethodGet, status: http.StatusOK, mcp: true},
		{name: "MCP HEAD", method: http.MethodHead, status: http.StatusOK, mcp: true},
		{name: "MCP API failure", method: http.MethodPost, status: http.StatusBadRequest, mcp: true},
		{name: "terminal write", method: http.MethodPost, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _, _ := testApp(t, "http://127.0.0.1:1")
			app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(p4JSONBody(16 << 20))}, nil
			})}
			ctx := context.WithValue(t.Context(), mcpFullWriteResponseKey{}, tc.mcp)
			_, e := app.doRequest(ctx, "http://127.0.0.1:1", "", tc.method, "/v1/auth-context", nil, "", false)
			if e == nil || e.Code != "RESPONSE_TOO_LARGE" {
				t.Fatalf("oversized response escaped its transport limit: %+v", e)
			}
		})
	}
}
