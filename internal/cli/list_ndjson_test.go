package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The transport runs in the caller's goroutine, so it can inspect stdout before
// serving the next page without synchronizing with an HTTP server goroutine.
func ndjsonListApp(t *testing.T, page func(*http.Request) (int, string)) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	app, out, stderr := testApp(t, "http://127.0.0.1:1")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "127.0.0.1:1" {
			t.Fatalf("unexpected host: %s", r.URL.Host)
		}
		status, body := http.StatusOK, authContextJSON("sandbox")
		if r.URL.Path != "/v1/auth-context" {
			status, body = page(r)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return app, out, stderr
}

func TestListNDJSONPages(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		args                               []string
		pages                              int
		sizes                              []string
		count, perPage                     int
		selectFields, timeline, empty, env bool
	}{
		{name: "generated", args: []string{"--all"}, pages: 2, sizes: []string{"", ""}, count: 4},
		{name: "one page", pages: 1, sizes: []string{""}, count: 2},
		{name: "all", args: []string{"--all", "--page-size", "2"}, pages: 2, sizes: []string{"2", "2"}, count: 4},
		{name: "limit truncates", args: []string{"--limit", "3", "--page-size", "1"}, pages: 2, sizes: []string{"3", "1"}, count: 3},
		{name: "limit cap", args: []string{"--limit", "101"}, pages: 2, sizes: []string{"100", "1"}, count: 101, perPage: 100},
		{name: "limit first page", args: []string{"--limit", "1"}, pages: 1, sizes: []string{"1"}, count: 1},
		{name: "all and limit", args: []string{"--all", "--limit", "3"}, pages: 2, sizes: []string{"3", "1"}, count: 3},
		{name: "resume", args: []string{"--page-token", "start", "--page-size", "2"}, pages: 1, sizes: []string{"2"}, count: 2},
		{name: "empty", args: []string{"--all"}, pages: 1, sizes: []string{""}, empty: true},
		{name: "select", args: []string{"--all", "--select", "customer_id,email"}, pages: 2, sizes: []string{"", ""}, count: 4, selectFields: true},
		{name: "empty select", args: []string{"--select", "email"}, pages: 1, sizes: []string{""}, empty: true},
		{name: "timeline", args: []string{"--all", "--select", "resource_timeline_entry_id"}, pages: 2, sizes: []string{"", ""}, count: 4, timeline: true},
		{name: "environment", pages: 1, sizes: []string{""}, count: 2, env: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, emitted := 0, 0
			perPage := tc.perPage
			if perPage == 0 {
				perPage = 2
			}
			var out *bytes.Buffer
			app, stdout, stderr := ndjsonListApp(t, func(r *http.Request) (int, string) {
				// This assertion fails if resources are buffered until all pages finish.
				if strings.Count(out.String(), "\n") != emitted {
					t.Fatalf("page %d arrived before previous resources were written: %s", calls+1, out)
				}
				if calls >= len(tc.sizes) || r.URL.Query().Get("page_size") != tc.sizes[calls] {
					t.Fatalf("unexpected page size: %s", r.URL)
				}
				if calls > 0 && r.URL.Query().Get("page_token") != "next" || calls == 0 && tc.name == "resume" && r.URL.Query().Get("page_token") != "start" {
					t.Fatalf("unexpected token: %s", r.URL)
				}
				calls++
				data := []any{}
				for i := 0; i < perPage && !tc.empty; i++ {
					field := "customer_id"
					if tc.timeline {
						field = "resource_timeline_entry_id"
					}
					data = append(data, map[string]any{field: fmt.Sprintf("item_%d_%d", calls, i), "extra": true})
				}
				emitted = min(calls*perPage, tc.count)
				var collection any = data
				if tc.timeline {
					collection = map[string]any{"resource_id": "pi_test", "entries": data}
				}
				next := ""
				if calls == 1 && !tc.empty {
					next = "next"
				}
				body, _ := json.Marshal(map[string]any{"data": collection, "next_page_token": next, "request_id": "req_page"})
				return http.StatusOK, string(body)
			})
			out = stdout
			args := []string{"customers", "list"}
			if tc.name == "generated" {
				args = []string{"products", "list"}
			}
			if tc.timeline {
				args = []string{"timeline", "pi_test"}
			}
			if tc.env {
				t.Setenv("FLINT_OUTPUT", "NDJSON")
			} else {
				args = append(args, "--output", "ndjson")
			}
			if exit := app.Run(append(args, tc.args...)); exit != ExitOK || stderr.Len() != 0 || calls != tc.pages || strings.Count(out.String(), "\n") != tc.count {
				t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, calls, out, stderr)
			}
			for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
				if line == "" && tc.empty {
					continue
				}
				var resource map[string]any
				if err := json.Unmarshal([]byte(line), &resource); err != nil || resource["data"] != nil || resource["next_page_token"] != nil || resource["request_id"] != nil {
					t.Fatalf("not a resource: %s (%v)", line, err)
				}
				if tc.selectFields && (len(resource) != 2 || resource["email"] != nil || resource["customer_id"] == nil) || tc.timeline && (len(resource) != 1 || resource["resource_timeline_entry_id"] == nil) {
					t.Fatalf("incorrect projection: %s", line)
				}
			}
		})
	}
}

func TestListNDJSONFailures(t *testing.T) {
	for _, tc := range []struct {
		name, second, code  string
		status, exit, lines int
	}{
		{"later API error", `{"error":{"type":"invalid_request","code":"BAD_PAGE","message":"Bad page.","request_id":"req_failed"}}`, "BAD_PAGE", 400, ExitAPI, 1},
		{"loop", `{"data":[],"next_page_token":"next"}`, "PAGINATION_TOKEN_LOOP", 200, ExitAPI, 1},
		{"bad collection", `{"data":{}}`, "INVALID_PAGINATION_RESPONSE", 200, ExitAPI, 1},
		{"bad envelope", `[]`, "INVALID_PAGINATION_RESPONSE", 200, ExitAPI, 1},
		{"bad token", `{"data":[],"next_page_token":42}`, "INVALID_PAGINATION_RESPONSE", 200, ExitAPI, 1},
		{"bad select", `{"data":[]}`, "INVALID_SELECT", 200, ExitUsage, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			app, out, stderr := ndjsonListApp(t, func(*http.Request) (int, string) {
				calls++
				if calls == 1 {
					return 200, `{"data":[{"customer_id":"cus_1"}],"next_page_token":"next"}`
				}
				return tc.status, tc.second
			})
			args := []string{"customers", "list", "--all", "--output", "ndjson"}
			if tc.name == "bad select" {
				args = append(args, "--select", "not_a_field")
			}
			if exit := app.Run(args); exit != tc.exit || strings.Count(out.String(), "\n") != tc.lines || calls > 2 {
				t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, calls, out, stderr)
			}
			var failure map[string]any
			if err := json.Unmarshal(stderr.Bytes(), &failure); err != nil {
				t.Fatal(err, stderr)
			}
			if code, _ := lookupPath(failure, "error.code"); code != tc.code {
				t.Fatal(failure)
			}
			if tc.name == "later API error" {
				if id, _ := lookupPath(failure, "error.request_id"); id != "req_failed" || out.String() != "{\"customer_id\":\"cus_1\"}\n" {
					t.Fatal(failure, out)
				}
			}
		})
	}
}

func TestListNDJSONFlagsAndWriteFailure(t *testing.T) {
	for _, flags := range [][]string{{"--jq", ".data[]"}, {"--field", "data.0.customer_id"}, {"--preview"}, {"--dry-run=client"}} {
		app, out, stderr := ndjsonListApp(t, func(*http.Request) (int, string) {
			t.Fatal("invalid flags sent a request")
			return 0, ""
		})
		if exit := app.Run(append([]string{"customers", "list", "--output", "ndjson"}, flags...)); exit != ExitUsage || out.Len() != 0 || !json.Valid(stderr.Bytes()) {
			t.Fatalf("flags=%v exit=%d stdout=%s stderr=%s", flags, exit, out, stderr)
		}
	}
	app, _, stderr := ndjsonListApp(t, func(*http.Request) (int, string) {
		return 200, `{"data":[{"customer_id":"cus_1"}]}`
	})
	app.Stdout = failingWriter{}
	if exit := app.Run([]string{"customers", "list", "--output", "ndjson"}); exit != ExitNetwork || !strings.Contains(stderr.String(), "OUTPUT_WRITE_FAILED") {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	for _, command := range app.Registry.Commands {
		if command.Supports.Pagination && !command.Supports.NDJSON {
			t.Errorf("%s does not advertise NDJSON", command.Name)
		}
	}
	// Existing streams retain their output/error routing.
	out := &bytes.Buffer{}
	app.Stdout, app.Stderr = out, &bytes.Buffer{}
	app.writeError(usageError("UNSUPPORTED_FLAG", "Synthetic stream failure.", "field"), Options{Output: "ndjson"})
	var failure map[string]any
	if err := json.Unmarshal(out.Bytes(), &failure); err != nil || failure["error"].(map[string]any)["code"] != "UNSUPPORTED_FLAG" {
		t.Fatalf("stream error routing changed: %s (%v)", out, err)
	}
}
