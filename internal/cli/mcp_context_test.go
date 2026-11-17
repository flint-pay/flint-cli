package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMCPContextSwitchRequiresRestart(t *testing.T) {
	for _, target := range []string{"ctx_b", "ctx_live"} {
		t.Run(target, func(t *testing.T) {
			app, _, _, _, _ := newContextTestApp(t)
			other := *app
			other.Stdout, other.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
			conn := startMCPTestConn(t, app, defaultOptions())
			conn.initialize(`{}`)
			conn.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flint_read","arguments":{"operation":"auth.status"}}}`)
			if got := fmt.Sprint(conn.next()); !strings.Contains(got, "test_ctx_a") {
				t.Fatalf("startup context: %s", got)
			}
			args := []string{"context", "switch", target, "--output", "json"}
			if target == "ctx_live" {
				args = append(args, "--live")
			}
			if exit := other.Run(args); exit != ExitOK {
				t.Fatalf("switch: %d %s %s", exit, other.Stdout, other.Stderr)
			}
			for _, tool := range []string{"flint_read", "flint_write", "flint_describe"} {
				operation := "auth.status"
				if tool == "flint_write" {
					operation = "customers.create"
				}
				conn.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":{"operation":%q,"dry_run":true}}}`, tool, operation))
				got := fmt.Sprint(conn.next())
				if !strings.Contains(got, "MCP_CONTEXT_CHANGED") || !strings.Contains(got, "Restart flint mcp serve") {
					t.Fatalf("changed context accepted by %s: %s", tool, got)
				}
			}
			if exit := conn.close(); exit != ExitOK {
				t.Fatal(exit)
			}
			if exit := other.Run([]string{"auth", "status", "--output", "json"}); exit != ExitOK {
				t.Fatalf("terminal credential no longer usable: %d", exit)
			}
		})
	}
}

func TestMCPContextPinsConfigAndCredentialDuringCall(t *testing.T) {
	app, _, _, state, _ := newContextTestApp(t)
	app.mcpContext = app.pinMCPContext(defaultOptions())
	other := *app
	other.mcpContext = nil
	if exit := other.Run([]string{"context", "switch", "ctx_b", "--output", "json"}); exit != ExitOK {
		t.Fatal(exit)
	}
	// Simulate the change happening after tool admission. Command children must
	// still request the original context, even though the cached token moved.
	resolved, _, err := app.resolveConfig(defaultOptions())
	if err != nil || resolved.ContextID != "ctx_a" {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	cmd, _ := app.Registry.ByName("auth.status")
	_, _, envelope, e := app.authenticateCommand(context.Background(), cmd, defaultOptions(), resolved)
	if e != nil || envelope.Data.ContextID != "ctx_a" || envelope.Data.MerchantID != "mer_ctx_a" || envelope.Data.SandboxID != "test_ctx_a" {
		t.Fatalf("authentication shifted: %+v %v", envelope, e)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if got := strings.Join(state.requested, ","); got != "ctx_b,ctx_a" {
		t.Fatalf("refresh contexts=%s", got)
	}
}

func TestMCPContextCredentialReplacement(t *testing.T) {
	for _, store := range []string{"keyring", "file"} {
		t.Run(store, func(t *testing.T) {
			app, _, _ := testApp(t, "")
			t.Setenv("FLINT_API_KEY", "")
			if err := app.updateConfig(func(cfg *Config) error {
				p := cfg.Profiles["default"]
				p.CredentialStore = store
				cfg.Profiles["default"] = p
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			stored := "flint_test_test"
			app.LoadCredential = func(string) (string, error) { return stored, nil }
			if store == "file" {
				if err := app.storeSavedCredential("default", stored); err != nil {
					t.Fatal(err)
				}
			}
			app.mcpContext = app.pinMCPContext(defaultOptions())
			stored = "flint_live_test"
			if store == "file" {
				if err := app.storeSavedCredential("default", stored); err != nil {
					t.Fatal(err)
				}
			}
			if e := app.checkMCPContext(); e == nil || e.Code != "MCP_CONTEXT_CHANGED" || strings.Contains(e.Message, stored) {
				t.Fatalf("replacement check=%v", e)
			}
			// The atomic credential read must also refuse a late replacement.
			if raw, err := app.loadSavedCredential("default"); err == nil || raw != "" {
				t.Fatal("changed credential was returned")
			}
		})
	}
}

func TestMCPContextSelectionChanges(t *testing.T) {
	for _, setting := range []string{"default profile", "merchant", "sandbox", "mode", "project"} {
		t.Run(setting, func(t *testing.T) {
			app, _, _ := testApp(t, "")
			app.mcpContext = app.pinMCPContext(defaultOptions())
			if setting == "project" {
				dir := filepath.Join(app.WorkingDir, ".flint")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"sandbox_id":"test_other"}`), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := app.updateConfig(func(cfg *Config) error {
				p := cfg.Profiles["default"]
				switch setting {
				case "default profile":
					cfg.DefaultProfile = "other"
					cfg.Profiles["other"] = Profile{}
				case "merchant":
					p.MerchantGuard = "mer_other"
				case "sandbox":
					p.SandboxID = "test_other"
				case "mode":
					p.Environment = "live"
				}
				cfg.Profiles["default"] = p
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if e := app.checkMCPContext(); e == nil || e.Code != "MCP_CONTEXT_CHANGED" {
				t.Fatalf("%s accepted: %v", setting, e)
			}
		})
	}
}

func TestMCPContextOAuthRefreshAndRevocation(t *testing.T) {
	app, _, _, state, stored := newContextTestApp(t)
	app.mcpContext = app.pinMCPContext(defaultOptions())
	// Force an ordinary rotating refresh inside the pinned session.
	c, _ := decodeOAuthCredential(stored())
	c.ExpiresAt = time.Now().Add(-time.Minute)
	raw, _ := json.Marshal(c)
	if err := app.storeSavedCredential("default", string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, e := app.oauthAccess(withOAuthContext(context.Background(), "ctx_a"), "default"); e != nil {
		t.Fatal(e)
	}
	if e := app.checkMCPContext(); e != nil {
		t.Fatalf("same-session refresh rejected: %v", e)
	}
	state.mu.Lock()
	state.unauthorizedContext = "ctx_a"
	state.mu.Unlock()
	result := mcpCall(t, app, defaultOptions(), "flint_read", map[string]any{"operation": "auth.status"})
	if result["isError"] != true || !strings.Contains(mcpResultText(result), "CONTEXT_ACCESS_DENIED") {
		t.Fatalf("revoked context accepted: %v", result)
	}
}

func TestMCPContextLiveStillRequiresAllowLive(t *testing.T) {
	app, _, _, _, _ := newContextTestApp(t)
	if exit := app.Run([]string{"context", "switch", "ctx_live", "--live", "--output", "json"}); exit != ExitOK {
		t.Fatal(exit)
	}
	conn := startMCPTestConn(t, app, defaultOptions())
	conn.initialize(`{}`)
	conn.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flint_read","arguments":{"operation":"auth.status","live":true}}}`)
	if got := fmt.Sprint(conn.next()); !strings.Contains(got, "LIVE_ACCESS_DISABLED") {
		t.Fatalf("live access accepted: %s", got)
	}
	if exit := conn.close(); exit != ExitOK {
		t.Fatal(exit)
	}
}

func TestMCPContextWriteCannotUseReplacedKey(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		writes++
		fmt.Fprint(w, `{"data":{"customer_id":"cus_fixture"}}`)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	stored := "flint_test_test"
	app.LoadCredential = func(string) (string, error) { return stored, nil }
	app.mcpContext = app.pinMCPContext(defaultOptions())
	stored = "flint_live_test"
	result := mcpCall(t, app, defaultOptions(), "flint_write", map[string]any{"operation": "customers.create", "arguments": map[string]any{"email": "fixture@example.com"}, "idempotency_key": "pin-test"})
	if writes != 0 || result["isError"] != true || !strings.Contains(mcpResultText(result), "MCP_CONTEXT_CHANGED") {
		t.Fatalf("writes=%d result=%v", writes, result)
	}
}
