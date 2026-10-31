package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestArrayEditsMergeNoopAndConflict(t *testing.T) {
	for _, tc := range []struct {
		name          string
		flags         []string
		current       []string
		conflicts     int
		want          []string
		gets, patches int
	}{
		{"add preserves", []string{"--add-enabled-events", "refund.succeeded"}, []string{"payment_intent.succeeded"}, 0, []string{"payment_intent.succeeded", "refund.succeeded"}, 1, 1},
		{"remove preserves", []string{"--remove-enabled-events", "refund.succeeded"}, []string{"payment_intent.succeeded", "refund.succeeded"}, 0, []string{"payment_intent.succeeded"}, 1, 1},
		{"add existing noop", []string{"--add-enabled-events", "payment_intent.succeeded"}, []string{"payment_intent.succeeded"}, 0, nil, 1, 0},
		{"remove missing noop", []string{"--remove-enabled-events", "refund.succeeded"}, []string{"payment_intent.succeeded"}, 0, nil, 1, 0},
		{"noop with scalar change", []string{"--add-enabled-events", "payment_intent.succeeded", "--status", "active"}, []string{"payment_intent.succeeded"}, 0, []string{"payment_intent.succeeded"}, 1, 1},
		{"mixed", []string{"--remove-enabled-events", "payment_intent.succeeded", "--add-enabled-events", "refund.succeeded"}, []string{"payment_intent.succeeded"}, 0, []string{"refund.succeeded"}, 1, 1},
		{"stdin conflict preserves input", []string{"--input", "-", "--add-enabled-events", "refund.succeeded"}, []string{"payment_intent.succeeded"}, 1, []string{"payment_intent.succeeded", "order.created", "refund.succeeded"}, 2, 2},
		{"version conflict once", []string{"--add-enabled-events", "refund.succeeded"}, []string{"payment_intent.succeeded"}, 1, []string{"payment_intent.succeeded", "order.created", "refund.succeeded"}, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gets, patches := 0, 0
			var last []string
			var keys []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
					return
				}
				if r.URL.Path != "/v1/webhook-endpoints/whep_test" {
					t.Errorf("path=%s", r.URL.Path)
				}
				if r.Method == "GET" {
					gets++
					values := append([]string(nil), tc.current...)
					if gets > 1 {
						values = append(values, "order.created")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"webhook_endpoint_id": "whep_test", "version": gets + 2, "enabled_events": values}})
					return
				}
				patches++
				keys = append(keys, r.Header.Get("Idempotency-Key"))
				var body struct {
					Version     int      `json:"expected_version"`
					Events      []string `json:"enabled_events"`
					Description string   `json:"description"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Version != gets+2 {
					t.Errorf("version=%d gets=%d", body.Version, gets)
				}
				if slices.Contains(tc.flags, "--input") && body.Description != "keep" {
					t.Errorf("lost stdin description %q", body.Description)
				}
				last = body.Events
				if patches <= tc.conflicts {
					w.WriteHeader(409)
					fmt.Fprint(w, `{"error":{"code":"VERSION_CONFLICT","message":"Changed"}}`)
					return
				}
				fmt.Fprint(w, `{"data":{"webhook_endpoint_id":"whep_test","version":5}}`)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			app.Stdin = strings.NewReader(`{"description":"keep"}`)
			args := append([]string{"webhook-endpoints", "update", "whep_test", "--idempotency-key", "array-edit", "--confirm"}, tc.flags...)
			exit, result := runJSON(t, app, out, args...)
			if exit != 0 || gets != tc.gets || patches != tc.patches || !reflect.DeepEqual(last, tc.want) {
				t.Fatalf("exit=%d GET=%d PATCH=%d values=%v want=%v result=%v stderr=%s", exit, gets, patches, last, tc.want, result, stderr)
			}
			if len(keys) == 2 && keys[0] == keys[1] {
				t.Fatal("different request versions reused an idempotency key", keys)
			}
		})
	}
}

func TestArrayEditsStopAfterSecondConflict(t *testing.T) {
	gets, patches := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
			return
		}
		if r.Method == "GET" {
			gets++
			fmt.Fprintf(w, `{"data":{"version":%d,"enabled_events":[]}}`, gets)
			return
		}
		patches++
		w.WriteHeader(409)
		fmt.Fprint(w, `{"error":{"code":"VERSION_CONFLICT","message":"Changed"}}`)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	exit, result := runJSON(t, app, out, "webhook-endpoints", "update", "whep_test", "--add-enabled-events", "refund.succeeded", "--confirm")
	code, _ := lookupPath(result, "error.code")
	if exit != ExitAPI || code != "VERSION_CONFLICT" || gets != 2 || patches != 2 {
		t.Fatal(exit, result, gets, patches)
	}
}

func TestArrayEditsRefuseConflictsAndOfflineReads(t *testing.T) {
	for _, flags := range [][]string{
		{"--add-enabled-events", "refund.succeeded", "--dry-run=client"},
		{"--add-enabled-events", "refund.succeeded", "--expected-version", "3"},
		{"--add-enabled-events", "refund.succeeded", "--enabled-events", "order.created"},
		{"--add-enabled-events", "refund.succeeded", "--remove-enabled-events", "refund.succeeded"},
	} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/auth-context" {
				fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
				return
			}
			calls++
		}))
		app, out, _ := testApp(t, server.URL)
		exit, result := runJSON(t, app, out, append([]string{"webhook-endpoints", "update", "whep_test", "--confirm"}, flags...)...)
		server.Close()
		if exit != 2 || calls != 0 {
			t.Fatal(flags, exit, result, calls)
		}
	}
}

func TestArrayEditsInventoryAndReplacementHelp(t *testing.T) {
	doc, _ := loadOpenAPI()
	count := 0
	for _, cmd := range NewRegistry().Commands {
		for _, arg := range cmd.Arguments {
			if arg.ArrayEdit == "" {
				continue
			}
			count++
			if cmd.Method != "PATCH" {
				t.Fatal(cmd.Name)
			}
			if _, exists := doc.Paths[cmd.APIPath]["get"]; !exists {
				t.Fatal("no GET", cmd.APIPath)
			}
			if arg.Type == "boolean" || !arg.Repeat {
				t.Fatal(arg)
			}
		}
	}
	if count < 10 {
		t.Fatal(count)
	}
	for _, name := range []string{"orders.line-items.update", "bundles.update", "invoices.update", "payment-links.update"} {
		cmd, _ := NewRegistry().ByName(name)
		for _, arg := range cmd.Arguments {
			if slices.Contains([]string{"--add-line-items", "--remove-line-items", "--add-components", "--remove-modifiers"}, arg.Flag) {
				t.Fatal(name, arg.Flag)
			}
		}
		found := false
		for _, arg := range cmd.Arguments {
			if arg.Name == "input" && strings.Contains(arg.Description, "replaces the whole list") {
				found = true
			}
		}
		if !found {
			t.Fatal("missing replacement help", name)
		}
	}
	t.Logf("%d add/remove flags", count)
	if !arrayContains([]any{json.Number("1.0")}, int64(1)) {
		t.Fatal("numeric item identity differs")
	}
}

func TestArrayEditsResolveIDHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/payment-links/pl_test" {
			t.Errorf("unexpected read %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"data":{"version":3,"delivery_method_ids":["dmet_existing"]}}`)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	resolved := ResolvedConfig{ProfileName: "default", Environment: "sandbox"}
	if err := app.saveHistory(History{Entries: []HistoryEntry{{ID: "dmet_history", ResourceType: "delivery_method", Profile: "default", Environment: "sandbox", CreatedAt: time.Now()}}}); err != nil {
		t.Fatal(err)
	}
	cmd, opts, _, e := parseInvocation(app.Registry, []string{"payment-links", "update", "pl_test", "--add-delivery-method-ids", "@last.dmet"})
	if e != nil {
		t.Fatal(e)
	}
	req, e := app.prepareRequest(context.Background(), cmd, opts, resolved, "", server.URL)
	if e != nil {
		t.Fatal(e)
	}
	got := req.BodyValue["delivery_method_ids"]
	if !reflect.DeepEqual(got, []any{"dmet_existing", "dmet_history"}) {
		t.Fatal(got)
	}
}

func TestArrayEditsRefuseIncompleteRead(t *testing.T) {
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			patches++
		}
		fmt.Fprint(w, `{"data":{"version":3}}`)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	cmd, opts, _, e := parseInvocation(app.Registry, []string{"webhook-endpoints", "update", "whep_test", "--add-enabled-events", "refund.succeeded"})
	if e != nil {
		t.Fatal(e)
	}
	_, e = app.prepareRequest(context.Background(), cmd, opts, ResolvedConfig{}, "", server.URL)
	if e == nil || e.Code != "ARRAY_EDIT_READ_INVALID" || patches != 0 {
		t.Fatal(e, patches)
	}
}

func TestArrayEditsCategoryHandleReadProjection(t *testing.T) {
	for _, group := range []string{"products", "bundles"} {
		t.Run(group, func(t *testing.T) {
			var patch map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, exhaustiveAuthContextJSON("sandbox"))
					return
				}
				if r.Method == "GET" {
					fmt.Fprint(w, `{"data":{"version":3,"categories":[{"category_id":"ctg_existing","handle":"old-category"}]}}`)
					return
				}
				_ = json.NewDecoder(r.Body).Decode(&patch)
				fmt.Fprint(w, `{"data":{"version":4}}`)
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			id := "prod_test"
			if group == "bundles" {
				id = "bun_test"
			}
			exit, result := runJSON(t, app, out, group, "update", id, "--add-category-handles", "new-category")
			if exit != 0 || !reflect.DeepEqual(patch["category_handles"], []any{"old-category", "new-category"}) {
				t.Fatal(exit, result, patch)
			}
		})
	}
}

func TestArrayEditsReadFieldsMatchEmbeddedSpec(t *testing.T) {
	doc, _ := loadOpenAPI()
	for _, cmd := range NewRegistry().Commands {
		for _, arg := range cmd.Arguments {
			if arg.ArrayEdit != "add" {
				continue
			}
			schema := responseSchema(doc.Paths[cmd.APIPath]["get"].Responses)
			path := "data." + arg.BodyPath
			if projection, ok := publicOperationOverlays[cmd.OperationID].ArrayReads[arg.BodyPath]; ok {
				path = "data." + projection.Field + ".0." + projection.Item
			}
			if _, err := argumentReader().Lookup(schema, path); err != nil {
				t.Errorf("%s %s read %s: %v", cmd.Name, arg.Flag, path, err)
			}
			if _, err := argumentReader().Lookup(schema, "data.version"); err != nil {
				t.Errorf("%s missing read version: %v", cmd.Name, err)
			}
		}
	}
}
