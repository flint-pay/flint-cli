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
	for _, required := range []string{"listener", "ready", "gap", "withheld", "forward", "disconnect", "checkpoint"} {
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

func TestMCPListenRequiresAndCapsStreamBounds(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	listen, ok := registry.ByName("listen")
	if !ok {
		t.Fatal("listen command is missing")
	}
	for _, test := range []struct {
		name      string
		arguments map[string]any
		wantError string
	}{
		{name: "missing", arguments: map[string]any{"forward_to": "http://localhost:8080/webhooks"}, wantError: "_flint"},
		{name: "too many records", arguments: map[string]any{"forward_to": "http://localhost:8080/webhooks", "_flint": map[string]any{"max_events": float64(mcpMaxStreamEvents + 1)}}, wantError: "maximum"},
		{name: "too long", arguments: map[string]any{"forward_to": "http://localhost:8080/webhooks", "_flint": map[string]any{"for": "3m"}}, wantError: "must not exceed"},
		{name: "bounded by records", arguments: map[string]any{"forward_to": "http://localhost:8080/webhooks", "_flint": map[string]any{"max_events": float64(10)}}},
		{name: "bounded by duration", arguments: map[string]any{"forward_to": "http://localhost:8080/webhooks", "_flint": map[string]any{"for": "30s"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateMCPArguments(listen, test.arguments)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("validateMCPArguments() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.wantError)) {
				t.Fatalf("validateMCPArguments() error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestMCPClientCanCancelInFlightListen(t *testing.T) {
	streamStarted := make(chan struct{})
	streamClosed := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
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
	app.Stdin = stdinReader
	app.Stdout = &stdout
	app.Stderr = &bytes.Buffer{}
	app.IsTTY = func() bool { return false }
	app.ConfigDir = t.TempDir()
	app.WorkingDir = t.TempDir()
	app.BaseURL = api.URL
	app.LoadCredential = func(string) (string, error) { return "flint_test_test", nil }

	done := make(chan int, 1)
	go func() {
		done <- app.serveMCP(defaultOptions())
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
				"forward_to": "http://127.0.0.1:1/webhooks",
				"_flint":     map[string]any{"for": "2m"},
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
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if exit := <-done; exit != ExitOK {
		t.Fatalf("serveMCP() exit = %d", exit)
	}

	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	cancelled := false
	for scanner.Scan() {
		var response map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("decode MCP response: %v", err)
		}
		if response["id"] == float64(7) {
			code, _ := lookupPath(response, "error.code")
			cancelled = code == float64(-32800)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Fatalf("MCP responses did not include cancellation: %s", stdout.String())
	}
}
