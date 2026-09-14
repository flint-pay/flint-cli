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

func TestMCPPreviewResultsMatchAdvertisedSchemas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/developer/auth-context" {
			t.Errorf("preview sent unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, authContextJSON("sandbox"))
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	catalog := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}, defaultOptions())
	rawTools, ok := lookupPath(catalog, "result.tools")
	if !ok {
		t.Fatalf("tools/list: %#v", catalog)
	}
	schemas := map[string]map[string]any{}
	for _, tool := range rawTools.([]map[string]any) {
		schemas[tool["name"].(string)] = tool["outputSchema"].(map[string]any)
	}
	for _, test := range []struct {
		name, tool string
		arguments  map[string]any
		path       string
		want       any
	}{
		{"dry run", "organizations.create", map[string]any{"name": "Audit", "_flint": map[string]any{"dry_run": "client"}}, "data.method", "POST"},
		{"destructive preview", "payment-intents.cancel", map[string]any{"payment_intent_id": "pi_test", "_flint": map[string]any{"preview": true}}, "data.preview.reversibility.reversible", false},
		{"transformed dry run", "organizations.create", map[string]any{"name": "Audit", "_flint": map[string]any{"dry_run": "client", "field": "data.method"}}, "value", "POST"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{"name": test.tool, "arguments": test.arguments}}, defaultOptions())
			value, ok := lookupPath(response, "result.structuredContent")
			if !ok {
				t.Fatalf("tool failed: %#v", response)
			}
			if got, ok := lookupPath(value, test.path); !ok || got != test.want {
				t.Fatalf("%s = %#v, want %#v", test.path, got, test.want)
			}
			assertMCPOutputMatchesSchema(t, schemas[test.tool], value)
		})
	}
}

func TestTimelineAllPreservesEnvelopeAndCollectsEntries(t *testing.T) {
	for _, viaMCP := range []bool{false, true} {
		t.Run(fmt.Sprintf("mcp=%t", viaMCP), func(t *testing.T) {
			var tokens []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/developer/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				if r.URL.Path != "/v1/developer/resource-timelines/pi_test" || r.URL.Query().Get("page_size") != "1" {
					t.Errorf("unexpected timeline request: %s", r.URL)
				}
				token := r.URL.Query().Get("page_token")
				tokens = append(tokens, token)
				next := "page_two"
				if token == "page_two" {
					next = ""
				}
				fmt.Fprintf(w, `{"data":{"resource_id":"pi_test","resource_type":"payment_intent","test":true,"environment_id":"env_test","entries":[{"entry_type":"event","occurred_at":"2026-09-13T00:00:00Z","resource_timeline_entry_id":"entry_%d","test":true}]},"next_page_token":%q,"request_id":"req_%d"}`, len(tokens), next, len(tokens))
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			var result any
			if viaMCP {
				response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{
					"name": "timeline", "arguments": map[string]any{"resource_id": "pi_test", "page_size": float64(1), "_flint": map[string]any{"all": true}},
				}}, defaultOptions())
				var ok bool
				result, ok = lookupPath(response, "result.structuredContent")
				if !ok {
					t.Fatalf("timeline tool failed: %#v", response)
				}
			} else {
				if exit := app.Run([]string{"timeline", "pi_test", "--all", "--page-size", "1", "--output", "json"}); exit != ExitOK {
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
			for path, want := range map[string]any{"data.resource_id": "pi_test", "data.resource_type": "payment_intent", "data.environment_id": "env_test", "data.test": true, "next_page_token": "", "request_id": "req_2"} {
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
				if exit != ExitNetwork || code != "REQUEST_CANCELED" {
					t.Fatalf("exit=%d result=%#v", exit, value)
				}
			} else if exit := app.Run([]string{"version", "--jq", "until(false; .)", "--output", "json"}); exit != ExitNetwork || !strings.Contains(out.String(), "REQUEST_CANCELED") {
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
