package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func assertMCPOutputMatchesSchema(t *testing.T, schema map[string]any, value any) {
	t.Helper()
	// Normalize typed Go slices/maps as they appear on the JSON wire.
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	const uri = "https://example.com/tool-output.json"
	if err := compiler.AddResource(uri, document); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(uri)
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.Validate(value); err != nil {
		t.Fatalf("output violates advertised schema: %v", err)
	}
}

func TestMCPDryRunsReturnJSONEnvelopeWithoutNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Errorf("dry run sent %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	for _, test := range []struct {
		name, operation, path string
		arguments             map[string]any
		irreversible          bool
	}{
		{"create dry run", "organizations.create", "/v1/organizations", map[string]any{"name": "Audit", "parent_organization_id": "org_parent"}, false},
		{"destructive dry run", "payment-intents.cancel", "/v1/payment-intents/pi_test/cancel", map[string]any{"payment_intent_id": "pi_test"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := mcpCall(t, app, defaultOptions(), "flint_write", map[string]any{"operation": test.operation, "arguments": test.arguments, "dry_run": true, "idempotency_key": "preview-fixture"})
			if result["isError"] == true {
				t.Fatal(mcpResultText(result))
			}
			value := mcpResultJSON(t, result)
			for path, want := range map[string]any{"data.dry_run": true, "data.operation": test.operation, "data.method": "POST", "data.path": test.path, "data.effect.irreversible": test.irreversible} {
				if got := lookupPathValue(value, path); got != want {
					t.Errorf("%s=%#v want=%#v", path, got, want)
				}
			}
		})
	}
	if requests != 0 {
		t.Fatalf("dry runs sent %d requests", requests)
	}
}

func TestTimelineAllPreservesEnvelopeAndCollectsEntries(t *testing.T) {
	for _, mode := range []string{"cli", "mcp", "raw-cli"} {
		t.Run(mode, func(t *testing.T) {
			viaMCP := strings.Contains(mode, "mcp")
			raw := strings.HasPrefix(mode, "raw")
			var tokens []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				if r.URL.Path != "/v1/resource-timelines/pi_test" || r.URL.Query().Get("page_size") != "1" {
					t.Errorf("unexpected timeline request: %s", r.URL)
				}
				token := r.URL.Query().Get("page_token")
				tokens = append(tokens, token)
				next := "page_two"
				if token == "page_two" {
					next = ""
				}
				fmt.Fprintf(w, `{"data":{"resource_id":"pi_test","resource_type":"payment_intent","test":true,"environment_id":"env_test","entries":[{"entry_type":"webhook_event","occurred_at":"2026-09-13T00:00:00Z","resource_timeline_entry_id":"entry_%d","event_type":"payment_intent.succeeded","webhook_event_id":"evt_test"}]},"next_page_token":%q,"request_id":"req_%026d"}`, len(tokens), next, len(tokens))
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			var result any
			if viaMCP {
				result = mcpResultJSON(t, mcpCall(t, app, defaultOptions(), "flint_read", map[string]any{"operation": "timeline", "arguments": map[string]any{"resource_id": "pi_test", "page_size": 1}, "all": true}))
			} else {
				argv := []string{"timeline", "pi_test"}
				if raw {
					argv = []string{"api", "get", "/v1/resource-timelines/pi_test"}
				}
				argv = append(argv, "--all", "--page-size", "1", "--output", "json")
				if exit := app.Run(argv); exit != ExitOK {
					t.Fatalf("exit=%d: %s", exit, out)
				}
				if err := json.Unmarshal(out.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(tokens, []string{"", "page_two"}) {
				t.Fatalf("page tokens: %v", tokens)
			}
			entries, _ := lookupPath(result, "data.entries")
			items, ok := entries.([]any)
			if !ok || len(items) != 2 {
				t.Fatalf("entries: %#v", entries)
			}
			for i, entry := range items {
				if id, _ := lookupPath(entry, "resource_timeline_entry_id"); id != fmt.Sprintf("entry_%d", i+1) {
					t.Fatalf("entry order: %#v", entries)
				}
			}
			for path, want := range map[string]any{"data.resource_id": "pi_test", "data.resource_type": "payment_intent", "data.environment_id": "env_test", "data.test": true, "next_page_token": "", "request_id": "req_00000000000000000000000002"} {
				if got, _ := lookupPath(result, path); got != want {
					t.Errorf("%s = %#v, want %#v", path, got, want)
				}
			}
			cmd, _ := app.Registry.ByName("timeline")
			schema, err := mcpOutputSchema(cmd)
			if err != nil {
				t.Fatal(err)
			}
			assertMCPOutputMatchesSchema(t, schema, result)
		})
	}
}

func TestJQTransformHonorsCancellation(t *testing.T) {
	for _, viaMCP := range []bool{false, true} {
		t.Run(fmt.Sprintf("mcp=%t", viaMCP), func(t *testing.T) {
			app, out, _ := testApp(t, "")
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			app.Context = ctx
			if viaMCP {
				cmd, _ := app.Registry.ByName("version")
				value, _, exit := app.callMCPCommand(ctx, cmd, map[string]any{"_flint": map[string]any{"jq": "until(false; .)"}}, defaultOptions())
				code, _ := lookupPath(value, "error.code")
				if exit != ExitNetwork || code != "CLIENT_CANCELED" {
					t.Fatalf("exit=%d result=%#v", exit, value)
				}
			} else if exit := app.Run([]string{"version", "--jq", "until(false; .)", "--output", "json"}); exit != ExitNetwork || !strings.Contains(out.String(), "CLIENT_CANCELED") {
				t.Fatalf("exit=%d output=%s", exit, out)
			}
		})
	}
}

func TestJQTransformBoundsMCPResultAccumulation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := applyOutputTransforms(ctx, nil, Options{JQ: "repeat(1)", outputLimit: 100})
	if err == nil || err.Code != "OUTPUT_LIMIT_EXCEEDED" {
		t.Fatalf("unbounded jq output: %v", err)
	}
}

func TestMCPJQTransformEnforcesOutputLimit(t *testing.T) {
	app, _, _ := testApp(t, "")
	cmd, _ := app.Registry.ByName("version")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	value, _, exit := app.callMCPCommand(ctx, cmd, map[string]any{
		"_flint": map[string]any{"jq": `repeat("x" * 1048576)`},
	}, defaultOptions())
	code, _ := lookupPath(value, "error.code")
	if exit != ExitNetwork || code != "OUTPUT_LIMIT_EXCEEDED" {
		t.Fatalf("exit=%d result=%#v", exit, value)
	}
}
