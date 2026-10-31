package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDoctorWithoutCredentialContinuesPublicChecks(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/openapi.json" || r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Flint-Version") != "" {
			t.Errorf("unexpected public check %s %s", r.Method, r.URL)
		}
		w.Header().Set("Flint-Version", "2026-02-01")
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	path, _ := app.configPath()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	app.LoadCredential = func(string) (string, error) { t.Error("fresh profile queried keyring"); return "", nil }
	if exit := app.Run([]string{"doctor", "--output", "json"}); exit != ExitAuth {
		t.Fatalf("exit=%d out=%s", exit, out)
	}
	var result struct {
		Data struct {
			Checks []map[string]any `json:"checks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, check := range result.Data.Checks {
		names[check["name"].(string)] = true
	}
	for _, name := range []string{"credential", "connectivity", "api_version", "cli_version"} {
		if !names[name] {
			t.Errorf("missing %s: %s", name, out)
		}
	}
	if requests.Load() != 1 || !strings.Contains(out.String(), missingCredentialMessage("default")) {
		t.Fatalf("requests=%d out=%s", requests.Load(), out)
	}
}

func TestMCPSetupToolsDoNotProbeFreshKeyring(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_API_KEY", "")
	path, _ := app.configPath()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	app.LoadCredential = func(string) (string, error) { t.Error("fresh MCP profile queried keyring"); return "", nil }
	for _, name := range []string{"init", "doctor"} {
		cmd, ok := app.mcpTool(name, mcpPolicy{allTools: true})
		if !ok {
			t.Fatalf("setup tool %s unavailable", name)
		}
		schema, e := schemaForMCPTool(cmd, mcpPolicyFromOptions(defaultOptions()))
		if e != nil {
			t.Fatal(e)
		}
		if _, ok := lookupPath(schema, "properties.credential_store"); ok {
			t.Error("agent can change credential store")
		}
		if _, ok := lookupPath(schema, "properties.fix"); ok {
			t.Error("doctor can rewrite configuration")
		}
	}
	for i := 0; i < 2; i++ {
		result := mcpCall(t, app, defaultOptions(), "customers_list", map[string]any{})
		if result["isError"] != true || !strings.Contains(mcpResultText(result), "No Flint credential") {
			t.Fatalf("result=%v", result)
		}
	}
}
