package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReportingTimeCalendarAndDST(t *testing.T) {
	for _, tc := range []struct{ zone, now, value, want string }{
		{"America/New_York", "2026-03-09T16:00:00Z", "yesterday", "2026-03-08T05:00:00Z"},
		{"America/New_York", "2026-03-09T16:00:00Z", "today", "2026-03-09T04:00:00Z"},
		{"America/New_York", "2026-11-02T16:00:00Z", "yesterday", "2026-11-01T04:00:00Z"},
		{"America/New_York", "2026-03-08T16:00:00Z", "this_week", "2026-03-02T05:00:00Z"},
		{"America/New_York", "2026-03-09T16:00:00Z", "this_week", "2026-03-09T04:00:00Z"},
		{"Asia/Kathmandu", "2026-10-30T01:00:00Z", "today", "2026-10-29T18:15:00Z"},
		{"Pacific/Auckland", "2026-10-30T18:00:00Z", "today", "2026-10-30T11:00:00Z"},
		{"America/New_York", "2026-03-09T16:00:00Z", "2026-03-08", "2026-03-08T05:00:00Z"},
		{"America/New_York", "2026-03-09T16:00:00Z", "2h", "2026-03-09T14:00:00Z"},
		{"America/New_York", "2026-03-09T16:00:00Z", "7d", "2026-03-02T16:00:00Z"},
		{"UTC", "2026-10-30T01:00:00Z", "2026-10-30T03:00:00+02:00", "2026-10-30T01:00:00Z"},
	} {
		t.Run(tc.zone+tc.now+tc.value, func(t *testing.T) {
			location, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			now, _ := time.Parse(time.RFC3339, tc.now)
			got, err := resolveTime(tc.value, now, location)
			if err != nil || got != tc.want {
				t.Fatalf("got %s, %v; want %s", got, err, tc.want)
			}
		})
	}
	for _, value := range []string{"soon", "2026-02-30", "0d", "9999999999999999w"} {
		if _, err := resolveTime(value, time.Now()); err == nil {
			t.Errorf("accepted %s", value)
		}
	}
}

func TestReportingTimezoneCacheAndOfflineFallback(t *testing.T) {
	calls := 0
	zone := "America/New_York"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/merchant" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		calls++
		if zone == "" {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":"INSUFFICIENT_SCOPE","message":"No profile read scope"}}`)
			return
		}
		fmt.Fprintf(w, `{"data":{"reporting_timezone":%q}}`, zone)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	app.Now = func() time.Time { return now }
	opts := defaultOptions()
	resolved := ResolvedConfig{MerchantID: "mer_one"}
	get := func() (*time.Location, string) {
		return app.reportingTimezone(context.Background(), opts, resolved, "synthetic-timezone-token", server.URL)
	}
	opts.DryRun = "client"
	if loc, note := get(); loc != time.UTC || note == "" || calls != 0 {
		t.Fatalf("offline = %v %s, calls %d", loc, note, calls)
	}
	opts.DryRun = ""
	if loc, note := get(); loc.String() != zone || note != "" || calls != 1 {
		t.Fatalf("fetch = %v %s, calls %d", loc, note, calls)
	}
	opts.DryRun = "client"
	if loc, note := get(); loc.String() != zone || note != "" || calls != 1 {
		t.Fatalf("cached offline = %v %s, calls %d", loc, note, calls)
	}
	resolved.ContextID = "different-context"
	if loc, note := get(); loc != time.UTC || note == "" {
		t.Fatalf("cross-context cache reused: %v %s", loc, note)
	}
	resolved.ContextID = ""
	now = now.Add(time.Hour)
	if loc, note := get(); loc != time.UTC || note == "" {
		t.Fatalf("stale cache reused: %v %s", loc, note)
	}
	opts.DryRun = ""
	zone = ""
	if loc, note := get(); loc != time.UTC || note == "" || calls != 2 {
		t.Fatalf("missing scope = %v %s calls %d", loc, note, calls)
	}
}

func TestReportingTimeQueryAndLocalEnums(t *testing.T) {
	app, out, stderr := testApp(t, "")
	if exit := app.Run([]string{"orders", "list", "--status", "bogus", "--output", "json"}); exit != ExitUsage || !strings.Contains(out.String(), "INVALID_ARGUMENT") {
		t.Fatalf("exit %d out %s stderr %s", exit, out, stderr)
	}
	cmd := mustMCPTool(t, app, "orders.list")
	if err := validateMCPArguments(cmd, map[string]any{"status": "open", "created_after": "today"}); err != nil {
		t.Fatal(err)
	}
	if err := validateMCPArguments(cmd, map[string]any{"status": "bogus"}); err == nil {
		t.Fatal("invalid enum accepted")
	}
	opts := defaultOptions()
	opts.Raw["created-after"] = []string{"today"}
	opts.DryRun = "client"
	app.Now = func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }
	req, e := app.prepareRequest(context.Background(), cmd, opts, ResolvedConfig{}, "", "http://127.0.0.1:9")
	if e != nil || !strings.Contains(req.Path, "created_after=2026-10-30T00%3A00%3A00Z") || !strings.Contains(stderr.String(), "using UTC") {
		t.Fatalf("request %+v err %v notes %s", req, e, stderr)
	}
}
