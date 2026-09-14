package cli

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestFeedbackRemovedAndSupportRetained(t *testing.T) {
	app, _, _ := testApp(t, "")
	for _, name := range []string{"feedback.report", "feedback.configure", "feedback-reports.create", "feedback-reports.get", "feedback-reports.list"} {
		if _, ok := app.Registry.ByName(name); ok {
			t.Errorf("removed command %s is registered", name)
		}
		response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{"name": name}}, defaultOptions())
		if _, ok := response["error"]; !ok {
			t.Errorf("removed tool %s accepted: %v", name, response)
		}
	}
	for _, name := range []string{"support.open", "help.search"} {
		if _, ok := app.Registry.ByName(name); !ok {
			t.Errorf("%s must remain available", name)
		}
	}
}

func TestFeedbackAbsentFromCLIDiscovery(t *testing.T) {
	app, out, stderr := testApp(t, "")
	for _, argv := range [][]string{
		{"feedback", "report"}, {"feedback", "configure", "enabled"},
		{"feedback-reports", "create"}, {"feedback-reports", "get", "fbr_old"}, {"feedback-reports", "list"},
	} {
		out.Reset()
		stderr.Reset()
		if exit := app.Run(argv); exit != ExitUsage {
			t.Errorf("%v exit=%d, want usage error", argv, exit)
		}
	}
	for _, argv := range [][]string{{"--help"}, {"schema", "commands", "--output", "json"}} {
		out.Reset()
		stderr.Reset()
		if exit := app.Run(argv); exit != ExitOK {
			t.Fatalf("%v exit=%d: %s", argv, exit, stderr)
		}
		if strings.Contains(strings.ToLower(out.String()), "feedback") {
			t.Errorf("%v still advertises feedback", argv)
		}
	}
}

func TestMCPStartsAndListsToolsWithoutFeedbackConsent(t *testing.T) {
	app, out, stderr := testApp(t, "")
	app.IsTTY = func() bool { return true }
	app.LoadCredential = func(string) (string, error) { t.Error("tool discovery read credentials"); return "", nil }
	app.Stdin = strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"test","version":"1"},"capabilities":{"elicitation":{"form":{}}}}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	if exit := app.Run([]string{"mcp", "serve"}); exit != ExitOK {
		t.Fatalf("exit=%d: %s", exit, stderr)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected startup prompt: %s", stderr)
	}
	decoder := json.NewDecoder(out)
	var initialized, listing map[string]any
	if err := decoder.Decode(&initialized); err != nil {
		t.Fatal(err)
	}
	if _, ok := initialized["result"]; !ok {
		t.Fatalf("initialize failed: %v", initialized)
	}
	if err := decoder.Decode(&listing); err != nil {
		t.Fatal(err)
	}
	result, ok := listing["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list failed: %v", listing)
	}
	tools := result["tools"].([]any)
	foundHelp := false
	for _, raw := range tools {
		name := raw.(map[string]any)["name"].(string)
		if strings.Contains(name, "feedback") {
			t.Errorf("feedback tool still listed: %s", name)
		}
		if name == "help.search" {
			foundHelp = true
		}
	}
	if !foundHelp {
		t.Error("help.search missing from MCP")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("unexpected additional MCP message: %v (%v)", extra, err)
	}
}

func TestRetiredFeedbackConfigIsDiscarded(t *testing.T) {
	var cfg Config
	if err := strictJSON([]byte(`{"default_profile":"default","profiles":{"default":{"merchant_guard":"mer_kept","agent_feedback_submission":"enabled"}}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Profiles["default"].MerchantGuard != "mer_kept" {
		t.Fatal("profile context was lost")
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "feedback") {
		t.Fatalf("retired setting persisted: %s", raw)
	}
	if err := strictJSON([]byte(`{"profiles":{"default":{"unknown_setting":true}}}`), &cfg); err == nil {
		t.Fatal("unknown profile settings must still fail validation")
	}
}
