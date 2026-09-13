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

func TestMCPFeedbackToolsFollowConsentAndCredentialScopes(t *testing.T) {
	tests := []struct {
		name, state string
		scopes      []string
		wantCreate  bool
		wantRead    bool
	}{
		{name: "unset consent advertises gated create", scopes: nil, wantCreate: true},
		{name: "enabled write-only", state: "enabled", scopes: []string{"developer.feedback_reports.write"}, wantCreate: true},
		{name: "enabled without write", state: "enabled", scopes: []string{"developer.feedback_reports.read"}, wantRead: true},
		{name: "disabled with both scopes", state: "disabled", scopes: []string{"developer.feedback_reports.write", "developer.feedback_reports.read"}, wantRead: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
					"auth_type": "api_key", "api_key_id": "key_test", "environment": "sandbox",
					"merchant_id": "mer_test", "sandbox_id": "test_test", "scopes": test.scopes,
				}})
			}))
			defer api.Close()
			app, _, _ := testApp(t, api.URL)
			if test.state != "" {
				if err := app.updateConfig(func(cfg *Config) error {
					if cfg.Profiles == nil {
						cfg.Profiles = map[string]Profile{}
					}
					profile := cfg.Profiles["default"]
					profile.AgentFeedbackSubmission = test.state
					cfg.Profiles["default"] = profile
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"}, defaultOptions())
			result := response["result"].(map[string]any)
			tools := result["tools"].([]map[string]any)
			names := map[string]bool{}
			for _, tool := range tools {
				names[tool["name"].(string)] = true
			}
			if names["feedback-reports.create"] != test.wantCreate {
				t.Fatalf("create exposed=%v, want %v", names["feedback-reports.create"], test.wantCreate)
			}
			if names["feedback-reports.get"] != test.wantRead || names["feedback-reports.list"] != test.wantRead {
				t.Fatalf("read tools exposed get=%v list=%v, want %v", names["feedback-reports.get"], names["feedback-reports.list"], test.wantRead)
			}
		})
	}
}

func TestMCPFeedbackElicitationAcceptPersistsConsentAndContinuesCall(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":"sandbox","merchant_id":"mer_test","sandbox_id":"test_test","scopes":["developer.feedback_reports.write"]}}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/feedback-reports" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"data":{"feedback_report_id":"fbr_01ARZ3NDEKTSV4RRFFQ69G5FAV","kind":"papercut","surface":"mcp","summary":"The tool hid the failing field","created_at":"2026-08-21T12:00:00Z"}}`)
	}))
	defer api.Close()

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{"form":{}}},"clientInfo":{"name":"test-client","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"feedback-reports.create","arguments":{"kind":"papercut","surface":"mcp","summary":"The tool hid the failing field"}}}`,
		`{"jsonrpc":"2.0","id":"flint-feedback-consent-2","result":{"action":"accept","content":{"enable_agent_feedback":true}}}`,
	}, "\n") + "\n"
	app, _, stderr := testApp(t, api.URL)
	var stdout bytes.Buffer
	app.Stdin = strings.NewReader(input)
	app.Stdout = &stdout
	app.Stderr = stderr
	app.IsTTY = func() bool { return false }
	if exit := app.serveMCP(defaultOptions()); exit != ExitOK {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}

	sawElicitation := false
	sawSuccess := false
	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	for scanner.Scan() {
		var response map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response["method"] == "elicitation/create" {
			sawElicitation = true
		}
		if response["id"] == float64(2) {
			result, _ := response["result"].(map[string]any)
			sawSuccess = result["isError"] != true
		}
	}
	if !sawElicitation || !sawSuccess {
		t.Fatalf("elicitation=%v success=%v responses=%s stderr=%s", sawElicitation, sawSuccess, stdout.String(), stderr.String())
	}
	resolved, _, err := app.resolveConfig(defaultOptions())
	if err != nil || resolved.AgentFeedbackSubmission != "enabled" {
		t.Fatalf("saved consent=%q error=%v", resolved.AgentFeedbackSubmission, err)
	}
}

func TestMCPFeedbackCancellationWhileConsentIsPendingDoesNotRunTool(t *testing.T) {
	resourceCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":"sandbox","merchant_id":"mer_test","sandbox_id":"test_test","scopes":["developer.feedback_reports.write"]}}`)
			return
		}
		resourceCalls++
		fmt.Fprint(w, `{"data":{"feedback_report_id":"fbr_01ARZ3NDEKTSV4RRFFQ69G5FAV","kind":"papercut","surface":"mcp","summary":"The tool hid the failing field","created_at":"2026-08-21T12:00:00Z"}}`)
	}))
	defer api.Close()

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{"form":{}}},"clientInfo":{"name":"test-client","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"feedback-reports.create","arguments":{"kind":"papercut","surface":"mcp","summary":"The tool hid the failing field"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`,
		`{"jsonrpc":"2.0","id":"flint-feedback-consent-2","result":{"action":"accept","content":{"enable_agent_feedback":true}}}`,
	}, "\n") + "\n"
	app, _, stderr := testApp(t, api.URL)
	var stdout bytes.Buffer
	app.Stdin = strings.NewReader(input)
	app.Stdout = &stdout
	if exit := app.serveMCP(defaultOptions()); exit != ExitOK {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}

	if resourceCalls != 0 {
		t.Fatalf("resource calls=%d output=%s", resourceCalls, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"code":-32800`) || !strings.Contains(stdout.String(), `"requestId":"flint-feedback-consent-2"`) {
		t.Fatalf("missing cancellation responses: %s", stdout.String())
	}
	resolved, _, err := app.resolveConfig(defaultOptions())
	if err != nil || resolved.AgentFeedbackSubmission != "" {
		t.Fatalf("saved consent=%q error=%v", resolved.AgentFeedbackSubmission, err)
	}
}

func TestMCPFeedbackStaleCallsFailClosed(t *testing.T) {
	tests := []struct {
		name, state, tool, code string
		scopes                  []string
		arguments               map[string]any
	}{
		{name: "disabled create", state: "disabled", tool: "feedback-reports.create", code: "AGENT_FEEDBACK_DISABLED", arguments: map[string]any{"kind": "bug", "surface": "mcp", "summary": "Specific feedback"}},
		{name: "disabled raw create", state: "disabled", tool: "api", code: "AGENT_FEEDBACK_DISABLED", arguments: map[string]any{"method": "post", "path": "/v1/feedback-reports", "kind": "bug", "surface": "mcp", "summary": "Specific feedback"}},
		{name: "create without write scope", state: "enabled", tool: "feedback-reports.create", code: "INSUFFICIENT_SCOPE", scopes: []string{"developer.feedback_reports.read"}, arguments: map[string]any{"kind": "bug", "surface": "mcp", "summary": "Specific feedback"}},
		{name: "raw create without write scope", state: "enabled", tool: "api", code: "INSUFFICIENT_SCOPE", scopes: []string{"developer.feedback_reports.read"}, arguments: map[string]any{"method": "POST", "path": "/v1/feedback-reports?source=mcp", "kind": "bug", "surface": "mcp", "summary": "Specific feedback"}},
		{name: "get without read scope", state: "enabled", tool: "feedback-reports.get", code: "INSUFFICIENT_SCOPE", scopes: []string{"developer.feedback_reports.write"}, arguments: map[string]any{"feedback_report_id": "fbr_01ARZ3NDEKTSV4RRFFQ69G5FAV"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resourceCalls := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/developer/auth-context" {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"auth_type": "api_key", "api_key_id": "key_test", "environment": "sandbox", "merchant_id": "mer_test", "sandbox_id": "test_test", "scopes": test.scopes}})
					return
				}
				resourceCalls++
			}))
			defer api.Close()
			app, _, _ := testApp(t, api.URL)
			if err := app.setAgentFeedbackSubmission(defaultOptions(), test.state); err != nil {
				t.Fatal(err)
			}
			response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": test.tool, "arguments": test.arguments}}, defaultOptions())
			raw, _ := json.Marshal(response)
			if !strings.Contains(string(raw), test.code) || resourceCalls != 0 {
				t.Fatalf("response=%s resource_calls=%d", raw, resourceCalls)
			}
		})
	}
}

func TestMCPFeedbackElicitationDeclineAndCancel(t *testing.T) {
	for _, test := range []struct {
		name, action, saved string
		secondCall          bool
		wantDisabledErrors  int
	}{
		{name: "decline", action: "decline", saved: "disabled", wantDisabledErrors: 1},
		{name: "cancel fails later calls without repeating", action: "cancel", secondCall: true, wantDisabledErrors: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			resourceCalls := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				resourceCalls++
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":"sandbox","merchant_id":"mer_test","sandbox_id":"test_test","scopes":["developer.feedback_reports.write"]}}`)
			}))
			defer api.Close()
			lines := []string{
				`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{"form":{}}},"clientInfo":{"name":"test-client","version":"1"}}}`,
				`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"feedback-reports.create","arguments":{"kind":"bug","surface":"mcp","summary":"Specific feedback"}}}`,
				`{"jsonrpc":"2.0","id":"flint-feedback-consent-2","result":{"action":"` + test.action + `"}}`,
			}
			if test.secondCall {
				lines = append(lines, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"feedback-reports.create","arguments":{"kind":"bug","surface":"mcp","summary":"Second feedback"}}}`)
			}
			app, _, stderr := testApp(t, api.URL)
			var stdout bytes.Buffer
			app.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
			app.Stdout = &stdout
			if exit := app.serveMCP(defaultOptions()); exit != ExitOK {
				t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
			}
			if count := strings.Count(stdout.String(), `"method":"elicitation/create"`); count != 1 {
				t.Fatalf("elicitation count=%d output=%s", count, stdout.String())
			}
			if count := strings.Count(stdout.String(), "AGENT_FEEDBACK_DISABLED"); count != test.wantDisabledErrors {
				t.Fatalf("disabled errors=%d want=%d output=%s", count, test.wantDisabledErrors, stdout.String())
			}
			if resourceCalls != 0 {
				t.Fatalf("resource calls=%d output=%s", resourceCalls, stdout.String())
			}
			resolved, _, err := app.resolveConfig(defaultOptions())
			if err != nil || resolved.AgentFeedbackSubmission != test.saved {
				t.Fatalf("saved=%q want=%q error=%v", resolved.AgentFeedbackSubmission, test.saved, err)
			}
		})
	}
}

func TestMCPFeedbackUnsupportedElicitationFallsBackWithoutPrompt(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":"sandbox","merchant_id":"mer_test","sandbox_id":"test_test","scopes":["developer.feedback_reports.write"]}}`)
	}))
	defer api.Close()
	app, _, _ := testApp(t, api.URL)
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{"name": "feedback-reports.create", "arguments": map[string]any{"kind": "bug", "surface": "mcp", "summary": "Specific feedback"}}}, defaultOptions())
	raw, _ := json.Marshal(response)
	if strings.Contains(string(raw), "elicitation/create") || !strings.Contains(string(raw), "AGENT_FEEDBACK_DISABLED") {
		t.Fatalf("response=%s", raw)
	}
}

func TestMCPFeedbackEnabledConsentWithoutWriteScopeDoesNotSubmit(t *testing.T) {
	resourceCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_test","environment":"sandbox","merchant_id":"mer_test","sandbox_id":"test_test","scopes":[]}}`)
			return
		}
		resourceCalls++
	}))
	defer api.Close()
	app, _, _ := testApp(t, api.URL)
	if err := app.setAgentFeedbackSubmission(defaultOptions(), "enabled"); err != nil {
		t.Fatal(err)
	}
	response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{"name": "feedback-reports.create", "arguments": map[string]any{"kind": "bug", "surface": "mcp", "summary": "Specific feedback"}}}, defaultOptions())
	raw, _ := json.Marshal(response)
	if !strings.Contains(string(raw), "INSUFFICIENT_SCOPE") || resourceCalls != 0 {
		t.Fatalf("resource_calls=%d response=%s", resourceCalls, raw)
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
