package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPStreamOutputSchemaIncludesListenRecords(t *testing.T) {
	t.Parallel()
	schema := mcpStreamOutputSchema()
	properties := schema["properties"].(map[string]any)
	records := properties["records"].(map[string]any)
	items := records["items"].(map[string]any)
	itemProperties := items["properties"].(map[string]any)
	typeSchema := itemProperties["type"].(map[string]any)
	values := typeSchema["enum"].([]string)
	available := make(map[string]struct{}, len(values))
	for _, value := range values {
		available[value] = struct{}{}
	}
	for _, required := range []string{"event", "listener", "ready", "gap", "withheld", "forward", "reconnect", "disconnect", "checkpoint"} {
		if _, ok := available[required]; !ok {
			t.Fatalf("MCP stream output schema is missing %q: %#v", required, values)
		}
	}
}

func TestMCPDoesNotExposeCredentialImport(t *testing.T) {
	t.Parallel()
	app := New(BuildInfo{Version: "test"})
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}, defaultOptions())
	rawTools, ok := lookupPath(response, "result.tools")
	if !ok {
		t.Fatalf("tools/list response = %#v", response)
	}
	tools := rawTools.([]map[string]any)
	for _, tool := range tools {
		if tool["name"] == "auth.import" {
			t.Fatal("tools/list exposed auth.import")
		}
	}

	call := app.handleMCP(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params:  map[string]any{"name": "auth.import", "arguments": map[string]any{"stdin": true}},
	}, defaultOptions())
	message, _ := lookupPath(call, "error.message")
	if !strings.Contains(strings.ToLower(message.(string)), "unknown tool") {
		t.Fatalf("auth.import call response = %#v", call)
	}
}

func TestMCPSparseDeliveryConfigurationUpdates(t *testing.T) {
	for _, family := range []struct {
		command string
		idField string
		id      string
		valid   []map[string]any
		invalid []any
	}{
		{
			command: "delivery-methods.update", idField: "delivery_method_id", id: "dmet_123",
			valid: []map[string]any{
				{"taxable": false}, {"taxable": nil}, {"charge_tax_category": nil},
				{"eligibility": nil}, {"recipient_requirements": []any{}},
			},
			invalid: []any{
				nil, map[string]any{}, map[string]any{"taxable": "false"},
				map[string]any{"origin": nil}, map[string]any{"pricing": nil},
				map[string]any{"recipient_requirements": nil}, map[string]any{"unknown": true},
			},
		},
		{
			command: "delivery-rate-callbacks.update", idField: "delivery_rate_callback_id", id: "dcb_123",
			valid: []map[string]any{
				{"preview_enabled": false}, {"preview_enabled": nil}, {"request_timeout_seconds": nil},
				{"maximum_request_bytes": nil}, {"maximum_response_bytes": nil}, {"redirect_policy": nil},
			},
			invalid: []any{
				nil, map[string]any{}, map[string]any{"url": nil},
				map[string]any{"preview_enabled": "false"}, map[string]any{"request_timeout_seconds": -1},
				map[string]any{"redirect_policy": "follow"}, map[string]any{"unknown": true},
			},
		},
	} {
		t.Run(family.command, func(t *testing.T) {
			check := func(t *testing.T, configuration any, version any, valid bool) {
				t.Helper()
				app, _, _ := testApp(t, "")
				for _, name := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_CHECKOUT_SESSION_SECRET", "FLINT_CONTEXT"} {
					t.Setenv(name, "")
				}
				credentialReads := 0
				app.LoadCredential = func(string) (string, error) {
					credentialReads++
					return "", nil
				}
				arguments := map[string]any{family.idField: family.id, "configuration": configuration}
				if version != nil {
					arguments["expected_version"] = version
				}
				response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{
					"name": mcpToolName(family.command), "arguments": arguments,
				}}, mcpAllToolsOptions(t))
				result := response["result"].(map[string]any)
				raw, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				if valid {
					if credentialReads != 1 || !strings.Contains(string(raw), "API_KEY_REQUIRED") || strings.Contains(string(raw), "INVALID_REQUEST") {
						t.Fatalf("valid sparse update did not reach authentication: reads=%d result=%s", credentialReads, raw)
					}
				} else if credentialReads != 0 || !strings.Contains(string(raw), "INVALID_REQUEST") {
					t.Fatalf("invalid update was not rejected before authentication: reads=%d result=%s", credentialReads, raw)
				}
			}
			for index, configuration := range family.valid {
				t.Run(fmt.Sprintf("valid/%d", index), func(t *testing.T) { check(t, configuration, float64(1), true) })
			}
			for index, configuration := range family.invalid {
				t.Run(fmt.Sprintf("invalid/%d", index), func(t *testing.T) { check(t, configuration, float64(1), false) })
			}
			for _, version := range []any{nil, float64(0), "1"} {
				t.Run(fmt.Sprintf("invalid version/%v", version), func(t *testing.T) { check(t, family.valid[0], version, false) })
			}
		})
	}
}

func TestMCPListenRequiresAndCapsStreamBounds(t *testing.T) {
	t.Parallel()
	listen, ok := NewRegistry().ByName("listen")
	if !ok {
		t.Fatal("missing listen")
	}
	for _, test := range []struct {
		name      string
		arguments map[string]any
		wantError string
	}{
		{"default bounds", map[string]any{}, ""},
		{"too many records", map[string]any{"max_events": float64(mcpMaxStreamEvents + 1)}, "maximum"},
		{"too long", map[string]any{"max_seconds": float64(121)}, "maximum"},
		{"bounded by records", map[string]any{"max_events": float64(10)}, ""},
		{"bounded by duration", map[string]any{"max_seconds": float64(30)}, ""},
		{"old private controls", map[string]any{"_flint": map[string]any{"for": "30s"}}, "additional properties"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateMCPArguments(listen, test.arguments)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error=%v want=%s", err, test.wantError)
			}
		})
	}
}

func TestMCPClientCanCancelInFlightListen(t *testing.T) {
	streamStarted := make(chan struct{})
	streamClosed := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":"sandbox","merchant_id":"mer_test","sandbox_id":"test_test","scopes":["webhooks.read","customers.read"]},"meta":{"api_version":"2026-02-01"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(streamStarted)
		<-r.Context().Done()
		close(streamClosed)
	}))
	defer api.Close()

	stdinReader, stdinWriter := io.Pipe()
	var stdout bytes.Buffer
	app := New(BuildInfo{Version: "test", APIVersion: "2026-02-01"})
	app.Registry = NewRegistry()
	app.Stdin = stdinReader
	app.Stdout = &stdout
	app.Stderr = &bytes.Buffer{}
	app.IsTTY = func() bool { return false }
	app.ConfigDir = t.TempDir()
	app.WorkingDir = t.TempDir()
	app.BaseURL = api.URL
	app.LoadCredential = func(string) (string, error) { return "flint_test_test", nil }
	if err := app.saveConfig(Config{DefaultProfile: "default", Profiles: map[string]Profile{"default": {}}}); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		done <- app.serveMCP(mcpServeOptions(t, "--tools", "listen"))
	}()
	requests := json.NewEncoder(stdinWriter)
	if err := requests.Encode(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": latestMCPProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := requests.Encode(jsonRPCRequest{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		t.Fatal(err)
	}
	if err := requests.Encode(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      7,
		Method:  "tools/call",
		Params: map[string]any{
			"name": "listen",
			"arguments": map[string]any{
				"forward_to":  "http://127.0.0.1:1/webhooks",
				"max_seconds": 120,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("MCP listen did not start")
	}
	if err := requests.Encode(jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/cancelled",
		Params:  map[string]any{"requestId": 7},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("MCP cancellation did not close the webhook stream")
	}
	if err := requests.Encode(jsonRPCRequest{JSONRPC: "2.0", ID: 8, Method: "ping"}); err != nil {
		t.Fatal(err)
	}
	// Give a stray response to the cancelled call time to appear.
	time.Sleep(200 * time.Millisecond)
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if exit := <-done; exit != ExitOK {
		t.Fatalf("serveMCP() exit = %d", exit)
	}

	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	pinged := false
	for scanner.Scan() {
		var response map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("decode MCP response: %v", err)
		}
		if response["id"] == float64(7) {
			t.Fatalf("server responded to a cancelled request: %s", scanner.Text())
		}
		pinged = pinged || response["id"] == float64(8)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !pinged {
		t.Fatalf("server stopped serving after the cancellation: %s", stdout.String())
	}
}

func TestMCPListenPrintSecretMatchesOutputSchema(t *testing.T) {
	server, api := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	result := mcpCall(t, app, mcpAllToolsOptions(t), "listen", map[string]any{"print_secret": true})
	if result["isError"] == true {
		t.Fatalf("print_secret failed: %s", mcpResultText(result))
	}
	cmd, _ := app.Registry.ByName("listen")
	schema, err := mcpOutputSchema(cmd)
	if err != nil {
		t.Fatal(err)
	}
	assertMCPOutputMatchesSchema(t, schema, mcpResultJSON(t, result))
	if len(api.seen()) != 0 {
		t.Fatalf("print_secret opened a stream: %v", api.seen())
	}
}

func TestMCPListenDurationAndExplicitEventCounts(t *testing.T) {
	for _, test := range []struct {
		name      string
		events    int
		controls  map[string]any
		wantError bool
	}{
		{"duration only uses the default event cap", 100, map[string]any{"for": "1s"}, false},
		{"explicit 100 is an expected count", 1, map[string]any{"for": "1s", "max_events": 100}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := listenAPI(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"\"}\n\n")
				for i := 0; i < test.events; i++ {
					fmt.Fprint(w, listenEventFrame(fmt.Sprintf("whev_%026d", i), "customer.created", `{}`))
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			app, _, _ := testApp(t, server.URL)
			result := mcpCall(t, app, mcpAllToolsOptions(t), "listen", map[string]any{"_flint": test.controls})
			if test.wantError {
				if result["isError"] != true || !strings.Contains(mcpResultText(result), "EXPECTED_EVENTS_NOT_RECEIVED") {
					t.Fatalf("missing expected-count error: %s", mcpResultText(result))
				}
				return
			}
			if result["isError"] == true {
				t.Fatalf("duration-only listen failed: %s", mcpResultText(result))
			}
			records := mcpResultJSON(t, result).(map[string]any)["records"].([]any)
			events := 0
			for _, record := range records {
				if record.(map[string]any)["type"] == "event" {
					events++
				}
			}
			if events != test.events {
				t.Fatalf("received %d events, want %d", events, test.events)
			}
		})
	}
}
