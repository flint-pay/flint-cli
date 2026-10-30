package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPDiscoveryBridgeUsesPublicArguments(t *testing.T) {
	for _, name := range []string{"customers.create", "customers create", "customers_create"} {
		t.Run(name, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				if r.Method != "POST" || r.URL.Path != "/v1/customers" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				writes++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["email"] != "fixture@example.com" || body["name"] != "Fixture" || len(body) != 2 {
					t.Errorf("body=%#v", body)
				}
				if r.Header.Get("Idempotency-Key") != "mcp-public-arguments" {
					t.Error("key was not bridged")
				}
				fmt.Fprint(w, `{"data":{"customer_id":"cus_fixture","name":"Fixture"}}`)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			result := mcpCall(t, app, defaultOptions(), "flint_write", map[string]any{"operation": name, "arguments": `{"email":"fixture@example.com","name":"Fixture"}`, "idempotency_key": "mcp-public-arguments"})
			if result["isError"] == true || writes != 1 {
				t.Fatalf("writes=%d result=%s", writes, mcpResultText(result))
			}
		})
	}
}

func TestMCPDiscoveryDispatchRefusesBeforeTransport(t *testing.T) {
	for _, tc := range []struct {
		name, tool, operation, code string
		opts                        []string
		arguments                   map[string]any
		controls                    map[string]any
	}{
		{name: "wrong tool", tool: "flint_read", operation: "customers.create", code: "wrong_tool"},
		{name: "read only", tool: "flint_write", operation: "customers.create", code: "read_only_server", opts: []string{"--read-only"}},
		{name: "buyer credential", tool: "flint_read", operation: "me.get", code: "OPERATION_NOT_AVAILABLE"},
		{name: "no raw api", tool: "flint_read", operation: "api", code: "OPERATION_NOT_AVAILABLE"},
		{name: "no config writes", tool: "flint_write", operation: "config.set", code: "OPERATION_NOT_AVAILABLE"},
		{name: "unknown argument", tool: "flint_write", operation: "customers.create", arguments: map[string]any{"customer": "cus_bad"}, code: "INVALID_REQUEST_BODY"},
		{name: "removed controls", tool: "flint_read", operation: "customers.list", controls: map[string]any{"_flint": map[string]any{"live": true}}, code: "INVALID_REQUEST"},
		{name: "required key on dry run", tool: "flint_write", operation: "refunds.create", controls: map[string]any{"dry_run": true}, code: "idempotency_key"},
		{name: "all only on list", tool: "flint_read", operation: "customers.get", arguments: map[string]any{"customer_id": "cus_fixture"}, controls: map[string]any{"all": true}, code: "not allowed"},
		{name: "trigger wait cap", tool: "flint_write", operation: "trigger", arguments: map[string]any{"event_type": "order.paid", "max_seconds": 61}, code: "max_seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				t.Errorf("unexpected request %s", r.URL.Path)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			input := map[string]any{"operation": tc.operation, "arguments": tc.arguments}
			if tc.arguments == nil {
				delete(input, "arguments")
			}
			for key, value := range tc.controls {
				input[key] = value
			}
			result := mcpCall(t, app, mcpServeOptions(t, tc.opts...), tc.tool, input)
			if result["isError"] != true || requests != 0 || !strings.Contains(mcpResultText(result), tc.code) {
				t.Fatalf("requests=%d result=%s", requests, mcpResultText(result))
			}
		})
	}
}

func TestMCPDiscoveryQueryBridgeAndScopeError(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, `{"data":{"auth_type":"api_key","environment":"sandbox","merchant_id":"mer_fixture","scopes":[]}}`)
			return
		}
		requests++
		if r.URL.Query().Get("email") != "fixture@example.com" || r.URL.Query().Get("page_size") != "3" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":{"type":"authorization_error","code":"INSUFFICIENT_SCOPE","message":"Read access is required.","details":[{"missing_scopes":["commerce.customers.read"]}],"remediation":{"retry":"none"}}}`)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	result := mcpCall(t, app, defaultOptions(), "flint_read", map[string]any{"operation": "customers.list", "arguments": map[string]any{"email": "fixture@example.com", "page_size": 3}})
	if result["isError"] != true || requests != 1 || !strings.Contains(mcpResultText(result), "INSUFFICIENT_SCOPE") || strings.Contains(mcpResultText(result), "MISSING_REQUIRED_SCOPES") {
		t.Fatalf("requests=%d result=%s", requests, mcpResultText(result))
	}
}

func TestMCPDiscoveryReadOnlyTypedWriteRefusal(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": "customers_create", "arguments": map[string]any{"email": "fixture@example.com"}}}, mcpServeOptions(t, "--tools", "customers", "--read-only"))
	result, ok := response["result"].(map[string]any)
	if !ok || result["isError"] != true || !strings.Contains(mcpResultText(result), "read_only_server") {
		t.Fatalf("response=%v", response)
	}
}

func TestMCPDiscoveryClassNoneKeysIgnored(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	result := mcpCall(t, app, mcpServeOptions(t, "--allow-destructive"), "flint_write", map[string]any{
		"operation": "history.clear", "idempotency_key": "clear-history-1", "confirm": true,
	})
	if result["isError"] == true {
		t.Fatalf("irrelevant key was not ignored: %s", mcpResultText(result))
	}
}
