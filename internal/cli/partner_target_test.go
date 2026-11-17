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
)

func partnerTestApp(t *testing.T, url string) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	app, out, err := testApp(t, url)
	t.Setenv("FLINT_API_KEY", "")
	t.Setenv("FLINT_ACCESS_TOKEN", "fpat_"+strings.Repeat("A", 43))
	t.Setenv("FLINT_MERCHANT", "")
	t.Setenv("FLINT_SANDBOX", "")
	return app, out, err
}

func TestPartnerTargetRequests(t *testing.T) {
	for _, source := range []string{"flags", "environment", "profile", "project", "raw"} {
		t.Run(source, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("Flint-Merchant-Id") != "mer_target" || r.Header.Get("Flint-Sandbox-Id") != "test_target" {
					t.Errorf("missing target on %s: %v", r.URL, r.Header)
				}
				if r.URL.Path != "/v1/customers" {
					t.Errorf("unexpected request: %s", r.URL)
				}
				if requests == 1 {
					fmt.Fprint(w, `{"data":[{"customer_id":"cus_one"}],"next_page_token":"page_two"}`)
				} else {
					fmt.Fprint(w, `{"data":[{"customer_id":"cus_two"}],"next_page_token":""}`)
				}
			}))
			defer server.Close()
			app, out, err := partnerTestApp(t, server.URL)
			argv := []string{"customers", "list", "--all", "--output", "json"}
			switch source {
			case "flags":
				argv = append(argv, "--merchant", "mer_target", "--sandbox", "test_target")
			case "raw":
				argv = []string{"api", "get", "/v1/customers", "--paginate", "--merchant", "mer_target", "--sandbox", "test_target", "--output", "json"}
			case "environment":
				t.Setenv("FLINT_MERCHANT", "mer_target")
				t.Setenv("FLINT_SANDBOX", "test_target")
			case "profile":
				cfg, e := app.loadConfig()
				if e != nil {
					t.Fatal(e)
				}
				p := cfg.Profiles["default"]
				p.MerchantGuard = "mer_target"
				cfg.Profiles["default"] = p
				if e := app.saveConfig(cfg); e != nil {
					t.Fatal(e)
				}
				t.Setenv("FLINT_SANDBOX", "test_target")
			case "project":
				if e := os.MkdirAll(filepath.Join(app.WorkingDir, ".flint"), 0700); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(filepath.Join(app.WorkingDir, ".flint", "config.json"), []byte(`{"merchant_id":"mer_target","sandbox_id":"test_target"}`), 0600); e != nil {
					t.Fatal(e)
				}
			}
			if code := app.Run(argv); code != ExitOK || requests != 2 {
				t.Fatalf("exit=%d requests=%d out=%s err=%s", code, requests, out.String(), err.String())
			}
		})
	}
}

func TestPartnerTargetDryRunAndConfirmation(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprint(dry), func(t *testing.T) {
			app, out, err := partnerTestApp(t, "http://127.0.0.1:1")
			argv := []string{"categories", "delete", "cat_target", "--merchant", "mer_target", "--sandbox", "test_target", "--output", "json"}
			if dry {
				argv = append(argv, "--dry-run", "client")
			}
			code := app.Run(argv)
			if dry {
				if code != ExitOK {
					t.Fatalf("exit=%d out=%s err=%s", code, out.String(), err.String())
				}
				var value map[string]any
				if e := json.Unmarshal([]byte(out.String()), &value); e != nil {
					t.Fatal(e)
				}
				if lookupPathValue(value, "data.headers.Flint-Merchant-Id") != "mer_target" || lookupPathValue(value, "data.headers.Flint-Sandbox-Id") != "test_target" {
					t.Fatal(value)
				}
			} else if code != ExitConfirmation || !strings.Contains(out.String(), "mer_target") {
				t.Fatalf("exit=%d out=%s err=%s", code, out.String(), err.String())
			}
		})
	}
}

func TestPartnerMissingMerchantGuidance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"type":"validation_error","code":"INVALID_REQUEST","param":"Flint-Merchant-Id","message":"Flint-Merchant-Id is required with an app-level partner app token.","details":[{"param":"Flint-Merchant-Id","code":"REQUIRED"}]}}`)
	}))
	defer server.Close()
	app, out, err := partnerTestApp(t, server.URL)
	if code := app.Run([]string{"orders", "list", "--output", "json"}); code != ExitAPI || !strings.Contains(out.String(), "Pass --merchant") {
		t.Fatalf("exit=%d out=%s err=%s", code, out.String(), err.String())
	}
	var envelope map[string]any
	if e := json.Unmarshal(out.Bytes(), &envelope); e != nil {
		t.Fatal(e)
	}
	if lookupPathValue(envelope, "error.message") != "Flint-Merchant-Id is required with an app-level partner app token." || !strings.Contains(fmt.Sprint(lookupPathValue(envelope, "cli.guidance")), "Pass --merchant") {
		t.Fatal(envelope)
	}
	result := mcpCall(t, app, defaultOptions(), "flint_read", map[string]any{"operation": "orders.list"})
	if result["isError"] != true || !strings.Contains(mcpResultText(result), "merchant: <mer_id>") {
		t.Fatal(mcpResultText(result))
	}
}

func TestPartnerMCPSelection(t *testing.T) {
	for _, typed := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			t.Run(fmt.Sprintf("typed=%v/pinned=%v", typed, pinned), func(t *testing.T) {
				targets := []string{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					targets = append(targets, r.Header.Get("Flint-Merchant-Id"))
					if r.Header.Get("Flint-Sandbox-Id") != "test_target" {
						t.Error("missing sandbox")
					}
					fmt.Fprint(w, `{"data":{"merchant_id":"mer_target"}}`)
				}))
				defer server.Close()
				app, _, _ := partnerTestApp(t, server.URL)
				flags := []string{"--sandbox", "test_target"}
				if pinned {
					flags = append(flags, "--merchant", "mer_target")
				}
				if typed {
					flags = append(flags, "--tools", "merchant.get")
				}
				opts := mcpServeOptions(t, flags...)
				tool := "flint_read"
				input := map[string]any{"operation": "merchant.get", "merchant": "mer_target"}
				if typed {
					tool = "merchant_get"
					delete(input, "operation")
				}
				result := mcpCall(t, app, opts, tool, input)
				if result["isError"] == true || len(targets) != 1 || targets[0] != "mer_target" {
					t.Fatalf("targets=%v result=%s", targets, mcpResultText(result))
				}
				input["merchant"] = "mer_second"
				result = mcpCall(t, app, opts, tool, input)
				if pinned {
					if result["isError"] != true || len(targets) != 1 {
						t.Fatalf("pin bypass: %v %s", targets, mcpResultText(result))
					}
				} else if result["isError"] == true || len(targets) != 2 || targets[1] != "mer_second" {
					t.Fatalf("targets=%v result=%s", targets, mcpResultText(result))
				}
			})
		}
	}
}

func TestNonPartnerMerchantGuardsUnchanged(t *testing.T) {
	for _, token := range []string{"", "fcat_synthetic"} {
		for _, merchant := range []string{"mer_123", "mer_other"} {
			t.Run(token+merchant, func(t *testing.T) {
				writes := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Flint-Merchant-Id") != "" || r.Header.Get("Flint-Sandbox-Id") != "" {
						t.Error("non-partner sent target headers")
					}
					if r.URL.Path == "/v1/auth-context" {
						fmt.Fprint(w, authContextJSON("sandbox"))
						return
					}
					writes++
					fmt.Fprint(w, `{"data":{"customer_id":"cus_target"}}`)
				}))
				defer server.Close()
				app, out, err := testApp(t, server.URL)
				t.Setenv("FLINT_ACCESS_TOKEN", token)
				code := app.Run([]string{"customers", "create", "--email", "test@example.com", "--merchant", merchant, "--output", "json"})
				if merchant == "mer_123" {
					if code != ExitOK || writes != 1 {
						t.Fatalf("exit=%d writes=%d %s %s", code, writes, out.String(), err.String())
					}
				} else if code != ExitAuth || writes != 0 || !strings.Contains(out.String(), "MERCHANT_GUARD_MISMATCH") {
					t.Fatalf("exit=%d writes=%d %s", code, writes, out.String())
				}
			})
		}
	}
	// Unverifiable non-partner sessions still cannot bypass merchant guards.
	app, _, _ := partnerTestApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_ACCESS_TOKEN", "unknown_session")
	command, _ := app.Registry.ByName("orders.list")
	_, _, _, e := app.authenticateCommand(context.Background(), command, defaultOptions(), ResolvedConfig{MerchantGuard: "mer_target"})
	if e == nil || e.Code != "SESSION_MERCHANT_GUARD_UNSUPPORTED" {
		t.Fatalf("%#v", e)
	}
}

func TestPartnerListenReconnectTarget(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Flint-Merchant-Id") != "mer_target" || r.Header.Get("Flint-Sandbox-Id") != "test_target" {
			t.Errorf("stream target missing: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			return
		}
		fmt.Fprint(w, "id: whev_01KEZF7C00MPP0T9S0KH5BB4CC\nevent: webhook.event\ndata: {\"webhook_event_id\":\"whev_01KEZF7C00MPP0T9S0KH5BB4CC\",\"event_type\":\"order.paid\",\"payload\":{}}\n\n")
	}))
	defer server.Close()
	app, out, err := partnerTestApp(t, server.URL)
	if code := app.Run([]string{"listen", "--merchant", "mer_target", "--sandbox", "test_target", "--max-events", "1", "--for", "5s", "--output", "ndjson"}); code != ExitOK || requests != 2 {
		t.Fatalf("exit=%d requests=%d %s %s", code, requests, out.String(), err.String())
	}
}

func TestPartnerMCPUnknownPermissions(t *testing.T) {
	app, _, _ := partnerTestApp(t, "http://127.0.0.1:1")
	index, e := app.operationIndex()
	if e != nil {
		t.Fatal(e)
	}
	app.mcpAuth = &mcpAuthState{index: index}
	app.mcpPrefetchAuth(context.Background(), defaultOptions())
	for _, tool := range []string{"flint_search", "flint_describe"} {
		input := map[string]any{"operation": "orders.list"}
		if tool == "flint_search" {
			input = map[string]any{"query": "orders.list"}
		}
		result := mcpCall(t, app, defaultOptions(), tool, input)
		text := mcpResultText(result)
		if result["isError"] == true || !strings.Contains(text, "unknown") || strings.Contains(text, "missing_scopes") {
			t.Fatalf("%s: %s", tool, text)
		}
	}
}

func TestPartnerMCPRecoveryKeepsMerchant(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(fmt.Sprint(typed), func(t *testing.T) {
			app, _, _ := partnerTestApp(t, "http://127.0.0.1:1")
			flags := []string{}
			if typed {
				flags = []string{"--tools", "all"}
			}
			policy := app.mcpPolicy(mcpServeOptions(t, flags...))
			for _, name := range []string{"refunds.create", "customers.update"} {
				cmd := mustMCPTool(t, app, name)
				args := map[string]any{"customer_id": "cus_target", "order_id": "ord_target", "_flint": map[string]any{"merchant": "mer_target"}}
				err := networkError("CLIENT_TIMEOUT", "Timed out", nil)
				err.Details = map[string]any{"outcome": "unknown"}
				err.idempotencyKey = "partner-recovery-key"
				err.requestSent = true
				actions := app.mcpNextActions(cmd, args, defaultOptions(), policy, err, "unverified")
				calls := 0
				for _, action := range actions {
					if call, ok := action["call"].(map[string]any); ok {
						calls++
						if lookupPathValue(call, "arguments.merchant") != "mer_target" {
							t.Fatalf("target lost: %v", call)
						}
					}
				}
				if calls == 0 {
					t.Fatalf("no recovery calls: %v", actions)
				}
			}
		})
	}
}
