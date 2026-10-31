package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func followRow(id string, at time.Time) map[string]any {
	return map[string]any{"api_request_log_id": id, "created_at": at.Format(time.RFC3339Nano), "http_method": "POST", "path": "/v1/customers", "status_code": 201, "latency_milliseconds": 12, "request_id": "req_test"}
}

func fakeFollowClock(app *App, start time.Time, stopAfter int) *[]time.Duration {
	now := start
	delays := []time.Duration{}
	app.Now = func() time.Time { return now }
	app.followSleep = func(_ context.Context, d time.Duration) bool {
		delays = append(delays, d)
		now = now.Add(d)
		return stopAfter == 0 || len(delays) < stopAfter
	}
	return &delays
}

func TestFollowOverlapAndDedupe(t *testing.T) {
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := calls.Add(1)
		q := r.URL.Query()
		if q.Get("page_size") != "100" || q.Get("http_method") != "POST" || q.Get("client_request_id") != "example" {
			t.Errorf("filters not forwarded: %v", q)
		}
		if i > 1 && q.Get("created_after") != base.Format(time.RFC3339Nano) {
			t.Errorf("bootstrap floor not retained: %v", q)
		}
		w.Header().Set("Date", base.Add(time.Duration(i)*time.Second).Format(http.TimeFormat))
		var rows []any
		token := ""
		switch i {
		case 1:
			w.Header().Set("Date", base.Format(http.TimeFormat))
			rows = []any{followRow("initial", base.Add(time.Millisecond))}
		case 2:
			rows = []any{followRow("second", base.Add(time.Second)), followRow("initial", base.Add(time.Millisecond))}
			token = "next"
		case 3:
			if q.Get("page_token") != "next" {
				t.Errorf("missing second page: %v", q)
			}
			rows = []any{followRow("first", base.Add(time.Second/2))}
		case 4:
			rows = []any{followRow("third", base.Add(3*time.Second)), followRow("second", base.Add(time.Second)), followRow("late", base.Add(700*time.Millisecond))}
		default:
			t.Error("unexpected poll")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows, "next_page_token": token})
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, server.URL)
	delays := fakeFollowClock(app, base, 5)
	opts := defaultOptions()
	opts.Output, opts.MaxEvents = "ndjson", 4
	_, e := app.executeFollow(context.Background(), opts, preparedRequest{Method: "GET", Path: "/v1/api-request-logs?http_method=POST&client_request_id=example"}, "flint_test_fake", server.URL)
	if e != nil {
		t.Fatal(e)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 5 || len(*delays) != 2 || (*delays)[0] != 2*time.Second {
		t.Fatalf("lines=%v delays=%v stderr=%s", lines, *delays, stderr)
	}
	for i, id := range []string{"first", "second", "late", "third"} {
		var row map[string]any
		_ = json.Unmarshal([]byte(lines[i]), &row)
		if row["api_request_log_id"] != id {
			t.Fatalf("row %d: %v", i, row)
		}
	}
	if !strings.Contains(lines[4], `"seen":4`) || !strings.Contains(lines[4], `"type":"end"`) {
		t.Fatal(lines[4])
	}
	seen := map[string]time.Time{"old": base.Add(-91 * time.Second), "edge": base.Add(-90 * time.Second), "new": base}
	pruneFollowSeen(seen, base)
	if len(seen) != 2 || seen["edge"].IsZero() || seen["new"].IsZero() {
		t.Fatal(seen)
	}
}

func TestFollowRetryKeepsWindowAndBackfills(t *testing.T) {
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := calls.Add(1)
		q := r.URL.Query()
		want := base.Add(-100 * time.Second).Format(time.RFC3339)
		if q.Get("created_after") != want {
			t.Errorf("cursor advanced on retry: %v want %s", q, want)
		}
		w.Header().Set("Date", base.Format(http.TimeFormat))
		if i == 2 || i == 3 {
			// Longer than the transport budget, so the follow loop must wait.
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"code":"RATE_LIMITED","message":"slow down","remediation":{"retry":"same_request"}}}`)
			return
		}
		token := ""
		rows := []any{followRow("backfill", base.Add(-50*time.Second))}
		if i == 1 || i == 4 {
			token = "next"
		}
		if i == 5 {
			rows = []any{followRow("new", base)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows, "next_page_token": token})
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	delays := fakeFollowClock(app, base, 6)
	opts := defaultOptions()
	opts.Output, opts.MaxEvents, opts.Timeout = "json", 2, 2*time.Second
	value, e := app.executeFollow(context.Background(), opts, preparedRequest{Method: "GET", Path: "/v1/api-request-logs?created_after=" + url.QueryEscape(base.Add(-100*time.Second).Format(time.RFC3339))}, "flint_test_fake", server.URL)
	if e != nil {
		t.Fatal(e)
	}
	if len(*delays) != 2 || (*delays)[0] != 7*time.Second || (*delays)[1] != 7*time.Second {
		t.Fatal(*delays)
	}
	body := value.(map[string]any)
	if len(body["data"].([]any)) != 2 || body["cursor"] != base.Format(time.RFC3339) {
		t.Fatal(body)
	}
}

func TestFollowOptionsAndNormalList(t *testing.T) {
	r := NewRegistry()
	for _, flags := range [][]string{{}, {"--created-before", "2026-10-31T00:00:00Z"}, {"--all"}, {"--limit", "1"}, {"--page-token", "x"}} {
		args := append([]string{"api-request-logs", "list", "--follow", "--output", "json"}, flags...)
		if len(flags) > 0 {
			args = append(args, "--for", "1s")
		}
		_, _, _, e := parseInvocation(r, args)
		want := "CONFLICTING_FLAGS"
		if len(flags) == 0 {
			want = "STREAM_BOUND_REQUIRED"
		}
		if e == nil || e.Code != want {
			t.Fatalf("%v: %v want %s", args, e, want)
		}
	}
	for _, args := range [][]string{{"customers", "list", "--follow"}, {"api-request-logs", "list", "--max-events", "1"}, {"api-request-logs", "list", "--follow=false", "--for", "1s"}} {
		if _, _, _, e := parseInvocation(r, args); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		calls.Add(1)
		fmt.Fprint(w, `{"data":[],"next_page_token":"next"}`)
	}))
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	if exit := app.Run([]string{"api-request-logs", "list", "--output", "json"}); exit != ExitOK || calls.Load() != 1 || !strings.Contains(out.String(), `"next_page_token":"next"`) {
		t.Fatalf("exit=%d calls=%d out=%s stderr=%s", exit, calls.Load(), out, stderr)
	}
	out.Reset()
	calls.Store(0)
	fakeFollowClock(app, time.Now(), 0)
	if exit := app.Run([]string{"api-request-logs", "list", "--follow", "--for", "1s", "--output", "json"}); exit != ExitOK || calls.Load() != 1 || !strings.Contains(out.String(), `"cursor":`) || strings.Contains(out.String(), "next_page_token") {
		t.Fatalf("follow exit=%d calls=%d out=%s stderr=%s", exit, calls.Load(), out, stderr)
	}
	cmd, _ := r.ByName("api-request-logs.list")
	if cmd.Stream || !cmd.Follow || !strings.Contains(strings.Join(cmd.Examples, " "), "--follow") {
		t.Fatal(cmd)
	}
}

func TestFollowBoundsCancelAndFatal(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 404, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int64
			base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Date", base.Format(http.TimeFormat))
				if status != 200 {
					w.Header().Set("Retry-After", "100")
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":{"code":"TEST_ERROR","message":"unchanged","remediation":{"retry":"none"}}}`)
					return
				}
				fmt.Fprint(w, `{"data":[]}`)
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			delays := fakeFollowClock(app, base, 0)
			opts := defaultOptions()
			opts.Output, opts.For = "ndjson", 7*time.Second
			_, e := app.executeFollow(context.Background(), opts, preparedRequest{Method: "GET", Path: "/v1/api-request-logs"}, "flint_test_fake", server.URL)
			if status >= 400 && status < 500 {
				if e == nil || e.Code != "TEST_ERROR" || e.HTTPStatus != status || calls.Load() != 1 {
					t.Fatalf("e=%v calls=%d", e, calls.Load())
				}
			} else if e != nil || !strings.Contains(out.String(), `"type":"end"`) {
				t.Fatalf("e=%v out=%s", e, out)
			}
			if status == 503 && (len(*delays) != 3 || (*delays)[0] != 2*time.Second || (*delays)[1] != 4*time.Second || strings.Count(out.String(), `"type":"throttled"`) != 1) {
				t.Fatalf("delays=%v out=%s", *delays, out)
			}
		})
	}
	app, out, _ := testApp(t, "http://127.0.0.1:9")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := defaultOptions()
	opts.Output = "ndjson"
	if _, e := app.executeFollow(ctx, opts, preparedRequest{}, "", ""); e != nil || !strings.Contains(out.String(), `"seen":0`) {
		t.Fatalf("e=%v out=%s", e, out)
	}
}
