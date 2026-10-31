package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOutputPathsUseResourceSchemaBeforeRequests(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; t.Errorf("unexpected request %s", r.URL.Path) }))
	defer server.Close()
	for _, flags := range [][]string{{"--field", "emial"}, {"--select", "emial"}, {"--field", "data.email"}} {
		app, out, _ := testApp(t, server.URL)
		args := append([]string{"customers", "create", "--email", "test@example.com", "--output", "json"}, flags...)
		if exit := app.Run(args); exit != ExitUsage || !strings.Contains(out.String(), "email") {
			t.Fatalf("exit %d output %s", exit, out)
		}
	}
	app, out, _ := testApp(t, server.URL)
	if exit := app.Run([]string{"customers", "get", "cus_test", "--wait-for", "data.status=active", "--output", "json"}); exit != ExitUsage || !strings.Contains(out.String(), "OUTPUT_FIELD_ROOT") {
		t.Fatalf("exit %d output %s", exit, out)
	}
	if calls != 0 {
		t.Fatalf("sent %d requests", calls)
	}
}

func TestOutputPathsFieldValuesAndEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, command, field, want string
		value                      any
	}{
		{"single", "customers.get", "email", "a@example.com\n", map[string]any{"data": map[string]any{"email": "a@example.com"}}},
		{"optional", "customers.get", "phone", "null\n", map[string]any{"data": map[string]any{"customer_id": "cus_test"}}},
		{"null", "customers.get", "phone", "null\n", map[string]any{"data": map[string]any{"phone": nil}}},
		{"object", "customers.get", "metadata", "{\"key\":\"value\"}\n", map[string]any{"data": map[string]any{"metadata": map[string]any{"key": "value"}}}},
		{"list", "customers.list", "customer_id", "cus_one\ncus_two\n", map[string]any{"data": []any{map[string]any{"customer_id": "cus_one"}, map[string]any{"customer_id": "cus_two"}}}},
		{"empty list", "customers.list", "customer_id", "", map[string]any{"data": []any{}}},
		{"checkout", "checkout-sessions.create", "checkout_auth_token", "synthetic-token\n", map[string]any{"data": map[string]any{"checkout_auth_token": "synthetic-token"}}},
		{"event data", "webhook-events.get", "data.object.id", "obj_test\n", map[string]any{"data": map[string]any{"data": map[string]any{"object": map[string]any{"id": "obj_test"}}}}},
		{"resource-timelines.get", "resource-timelines.get", "entries", "[]\n", map[string]any{"data": map[string]any{"entries": []any{}}}},
		{"metadata", "customers.get", "metadata.custom", "null\n", map[string]any{"data": map[string]any{"metadata": map[string]any{}}}},
		{"index", "orders.get", "line_items.0.name", "Item\n", map[string]any{"data": map[string]any{"line_items": []any{map[string]any{"name": "Item"}}}}},
		{"local", "version", "cli_version", "test\n", map[string]any{"data": map[string]any{"cli_version": "test"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, out, _ := testApp(t, "")
			cmd, ok := app.Registry.ByName(tc.command)
			if !ok {
				t.Fatal(tc.command)
			}
			opts := defaultOptions()
			opts.Field = tc.field
			if e := app.writeResult(tc.value, cmd, opts); e != nil || out.String() != tc.want {
				t.Fatalf("err %v output %q want %q", e, out, tc.want)
			}
		})
	}
	app, out, _ := testApp(t, "")
	cmd, _ := app.Registry.ByName("customers.list")
	opts := defaultOptions()
	opts.Output = "json"
	opts.Select = []string{"customer_id", "phone"}
	value := map[string]any{"data": []any{map[string]any{"customer_id": "cus_test"}}, "next_page_token": "next", "request_id": "req_test", "meta": map[string]any{"api_version": "2026-09-07"}}
	if e := app.writeResult(value, cmd, opts); e != nil {
		t.Fatal(e)
	}
	for _, part := range []string{`"phone":null`, `"next_page_token":"next"`, `"request_id":"req_test"`, `"meta":`} {
		if !strings.Contains(out.String(), part) {
			t.Fatalf("missing %s in %s", part, out)
		}
	}
	out.Reset()
	opts.Select = nil
	opts.JQ = ".next_page_token"
	if e := app.writeResult(value, cmd, opts); e != nil || strings.TrimSpace(out.String()) != `"next"` {
		t.Fatalf("jq err %v out %s", e, out)
	}
}

func TestOutputPathsUnknownAndRootCodes(t *testing.T) {
	app, _, _ := testApp(t, "")
	cmd, _ := app.Registry.ByName("customers.get")
	for _, param := range []string{"field", "select", "wait-for"} {
		e := validateOutputPaths(cmd, []outputPath{{"emial", param}})
		if e == nil || e.Code != "UNKNOWN_OUTPUT_FIELD" || e.Param != param || !strings.Contains(fmt.Sprint(e.Details), "email") {
			t.Fatalf("%s: %+v", param, e)
		}
		e = validateOutputPaths(cmd, []outputPath{{"data.email", param}})
		if e == nil || e.Code != "OUTPUT_FIELD_ROOT" {
			t.Fatalf("%s: %+v", param, e)
		}
	}
}
