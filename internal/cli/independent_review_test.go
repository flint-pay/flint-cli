package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMixedAuthAccessTokenMutationSafety(t *testing.T) {
	for _, test := range []struct {
		name, environment, code string
		status, exit            int
		flags                   []string
		write                   bool
	}{
		{"live acknowledgement", "live", "LIVE_ACKNOWLEDGEMENT_REQUIRED", 200, ExitAuth, nil, false},
		{"live confirmation", "live", "PRODUCTION_CONFIRMATION_REQUIRED", 200, ExitConfirmation, []string{"--live"}, false},
		{"live confirmed", "live", "", 200, ExitOK, []string{"--live", "--confirm"}, true},
		{"sandbox", "sandbox", "", 200, ExitOK, nil, true},
		{"merchant guard", "live", "MERCHANT_GUARD_MISMATCH", 200, ExitAuth, []string{"--live", "--confirm", "--merchant", "mer_other"}, false},
		{"unverified", "", "UNVERIFIED_ENVIRONMENT_CONFIRMATION_REQUIRED", 401, ExitConfirmation, nil, false},
		{"unverified with live", "", "UNVERIFIED_ENVIRONMENT_CONFIRMATION_REQUIRED", 401, ExitConfirmation, []string{"--live"}, false},
		{"unverified confirmed", "", "", 401, ExitOK, []string{"--confirm"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			auths, writes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					auths++
					w.WriteHeader(test.status)
					if test.status == 200 {
						fmt.Fprint(w, strings.ReplaceAll(authContextJSON(test.environment), `"api_key"`, `"oauth"`))
					} else {
						fmt.Fprint(w, `{"error":{"type":"authentication_error","code":"UNAUTHORIZED","message":"unsupported token"}}`)
					}
					return
				}
				writes++
				fmt.Fprint(w, `{"data":{"sandbox_id":"test_created"}}`)
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_ACCESS_TOKEN", "synthetic-mixed-route-token")
			args := append([]string{"sandboxes", "create", "--input", `{"name":"Review sandbox"}`}, test.flags...)
			exit, result := runJSON(t, app, out, args...)
			if exit != test.exit || auths != 1 || (writes == 1) != test.write || writes > 1 {
				t.Fatalf("exit=%d auths=%d writes=%d result=%v", exit, auths, writes, result)
			}
			if test.code != "" && lookupPathValue(result, "error.code") != test.code {
				t.Fatalf("want %s: %v", test.code, result)
			}
		})
	}
}

func TestMCPMixedAuthAccessTokenSafety(t *testing.T) {
	for _, test := range []struct {
		name, code string
		flags      []string
		write      bool
	}{
		{"live disabled", "LIVE_ACCESS_DISABLED", nil, false},
		{"confirmation required", "PRODUCTION_CONFIRMATION_REQUIRED", []string{"--allow-live"}, false},
		{"live confirmed", "", []string{"--allow-live", "--allow-destructive"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			auths, writes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					auths++
					fmt.Fprint(w, strings.ReplaceAll(authContextJSON("live"), `"api_key"`, `"oauth"`))
					return
				}
				writes++
				fmt.Fprint(w, `{"data":{"sandbox_id":"test_created"}}`)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			t.Setenv("FLINT_ACCESS_TOKEN", "synthetic-live-oauth")
			args := map[string]any{"name": "Review sandbox"}
			if len(test.flags) > 0 {
				controls := map[string]any{"live": true}
				if test.write {
					controls["confirm"] = true
				}
				args["_flint"] = controls
			}
			result := mcpCall(t, app, mcpAllToolsOptions(t, test.flags...), "sandboxes_create", args)
			if auths != 1 || (writes == 1) != test.write || writes > 1 || (result["isError"] == true) == test.write {
				t.Fatalf("auths=%d writes=%d result=%v", auths, writes, result)
			}
			if test.code != "" && !strings.Contains(mcpResultText(result), test.code) {
				t.Fatalf("want %s: %v", test.code, result)
			}
		})
	}
}

func TestMCPUnverifiedEnvironmentElicitsConfirmation(t *testing.T) {
	for _, approve := range []bool{false, true} {
		t.Run(fmt.Sprint(approve), func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(w, `{"error":{"type":"authentication_error","code":"UNAUTHORIZED","message":"unsupported token"}}`)
					return
				}
				writes++
				fmt.Fprint(w, `{"data":{"payment_intent_id":"pi_created"}}`)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			t.Setenv("FLINT_ACCESS_TOKEN", "synthetic-partner-token")
			conn := startMCPTestConn(t, app, defaultOptions())
			conn.initialize(`{"elicitation":{"form":{}}}`)
			conn.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"payment-intents_create","arguments":{"amount_money":{"amount":500,"currency":"USD"},"payment_options":["card"],"_flint":{"idempotency_key":"saved-test-action-1"}}}}`)
			request := conn.next()
			if request["method"] != "elicitation/create" || writes != 0 || !strings.Contains(fmt.Sprint(lookupPathValue(request, "params.message")), "cannot verify") {
				t.Fatalf("writes=%d request=%v", writes, request)
			}
			id, _ := json.Marshal(request["id"])
			conn.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"action":"accept","content":{"confirm":%t}}}`, id, approve))
			response := conn.next()
			if (lookupPathValue(response, "result.isError") == true) == approve || (writes == 1) != approve || writes > 1 {
				t.Fatalf("writes=%d response=%v", writes, response)
			}
			if exit := conn.close(); exit != ExitOK {
				t.Fatal(exit)
			}
		})
	}
}

func TestMCPOptionalFieldProjectionsMatchCLI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		fmt.Fprint(w, `{"data":{"customer_id":"cus_review"}}`)
	}))
	defer server.Close()
	for _, test := range []struct {
		name     string
		flags    []string
		controls map[string]any
		want     string
	}{
		{"field", []string{"--field", "email"}, map[string]any{"field": "email"}, "null"},
		{"select", []string{"--select", "email"}, map[string]any{"select": []any{"email"}}, `{"data":{"email":null}}`},
		{"jq", []string{"--jq", ".data.email"}, map[string]any{"jq": ".data.email"}, "null"},
		{"select and jq", []string{"--select", "email", "--jq", ".data | has(\"email\")"}, map[string]any{"select": []any{"email"}, "jq": `.data | has("email")`}, "true"},
		{"unknown field", []string{"--field", "emial"}, map[string]any{"field": "emial"}, "UNKNOWN_OUTPUT_FIELD"},
		{"unknown select", []string{"--select", "emial"}, map[string]any{"select": []any{"emial"}}, "UNKNOWN_OUTPUT_FIELD"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, out, _ := testApp(t, server.URL)
			args := append([]string{"customers", "get", "cus_review", "--output", "json"}, test.flags...)
			exit := app.Run(args)
			result := mcpCall(t, app, defaultOptions(), "customers_get", map[string]any{"customer_id": "cus_review", "_flint": test.controls})
			text := mcpResultText(result)
			if exit == ExitOK {
				value, _ := json.Marshal(lookupPathValue(result, "structuredContent.value"))
				if result["isError"] == true || strings.TrimSpace(out.String()) != test.want || string(value) != test.want {
					t.Fatalf("CLI=%s MCP=%v", out, result)
				}
			} else if result["isError"] != true || !strings.Contains(out.String(), test.want) || !strings.Contains(text, test.want) {
				t.Fatalf("CLI=%s MCP=%v", out, result)
			}
		})
	}
}

func TestPendingLiveLoginPreservesAcknowledgement(t *testing.T) {
	devices, polls, revokes := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loginDevicePath:
			devices++
			writeTestDevice(w)
		case loginTokenPath:
			polls++
			writeTestTokens(w, testOAuthAccess, testOAuthRefresh)
		case "/v1/auth-context":
			fmt.Fprint(w, oauthAuthJSON("live"))
		case oauthRevokePath:
			revokes++
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	projectDir := filepath.Join(app.WorkingDir, ".flint")
	if err := os.MkdirAll(projectDir, 0700); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(projectDir, "config.json")
	if err := os.WriteFile(projectPath, []byte(`{"sandbox":"test_123"}`), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"login", "--live", "--profile", "review", "--merchant", "mer_123", "--scope", "commerce.subscriptions.read"}
	exit, result := runJSON(t, app, out, append(append([]string{}, args...), "--no-wait")...)
	next := "flint login --live --profile review --credential-store keyring --merchant mer_123 --scope commerce.subscriptions.read"
	if exit != ExitOK || lookupPathValue(result, "data.next_command") != next || devices != 1 || polls != 0 {
		t.Fatalf("exit=%d devices=%d polls=%d result=%v", exit, devices, polls, result)
	}
	path, _ := app.pendingLoginPath("review")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pending pendingLogin
	if err := json.Unmarshal(before, &pending); err != nil || !pending.Live || pending.MerchantGuard != "mer_123" || pending.SandboxGuard != "test_123" {
		t.Fatalf("pending options not saved: %v", err)
	}
	for _, test := range []struct {
		flags   []string
		project string
	}{
		{[]string{"--profile", "review", "--merchant", "mer_123"}, `{"sandbox":"test_123"}`},
		{[]string{"--live", "--profile", "review", "--merchant", "mer_other"}, `{"sandbox":"test_123"}`},
		{[]string{"--live", "--profile", "review"}, `{"sandbox":"test_123"}`},
		{args[1:], `{"sandbox":"test_other"}`},
		{args[1:], `{"sandbox":"test_123","context":"ctx_other"}`},
	} {
		if err := os.WriteFile(projectPath, []byte(test.project), 0600); err != nil {
			t.Fatal(err)
		}
		exit, result := runJSON(t, app, out, append([]string{"login"}, test.flags...)...)
		after, err := os.ReadFile(path)
		if exit != ExitUsage || lookupPathValue(result, "error.code") != "PENDING_LOGIN_OPTIONS_MISMATCH" || err != nil || string(after) != string(before) || polls != 0 || revokes != 0 || devices != 1 {
			t.Fatalf("exit=%d devices=%d polls=%d revokes=%d err=%v result=%v", exit, devices, polls, revokes, err, result)
		}
	}
	if err := os.WriteFile(projectPath, []byte(`{"sandbox":"test_123"}`), 0600); err != nil {
		t.Fatal(err)
	}
	stored := ""
	app.StoreCredential = func(profile, credential string) error {
		if profile != "review" {
			t.Error(profile)
		}
		stored = credential
		return nil
	}
	exit, result = runJSON(t, app, out, strings.Fields(next)[1:]...)
	if exit != ExitOK || stored == "" || devices != 1 || polls != 1 || revokes != 0 {
		t.Fatalf("exit=%d devices=%d polls=%d revokes=%d result=%v", exit, devices, polls, revokes, result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("completed pending login remains: %v", err)
	}
}

func TestLimitContinuationUsesRemainingPageSize(t *testing.T) {
	for _, limit := range []int{5, 100, 105, 205} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			returned := 0
			var sizes []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				size, err := strconv.Atoi(r.URL.Query().Get("page_size"))
				if err != nil || size != min(limit-returned, 100) {
					t.Errorf("page_size=%d returned=%d err=%v", size, returned, err)
				}
				if returned > 0 && r.URL.Query().Get("page_token") != fmt.Sprint(returned) {
					t.Error("incorrect continuation token", r.URL.String())
				}
				sizes = append(sizes, size)
				data := []any{}
				for i := 0; i < size; i++ {
					data = append(data, map[string]any{"customer_id": fmt.Sprintf("cus_%d", returned+i)})
				}
				returned += size
				writeJSON(w, map[string]any{"data": data, "next_page_token": fmt.Sprint(returned)})
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_ACCESS_TOKEN", "synthetic-read-token")
			exit, result := runJSON(t, app, out, "customers", "list", "--limit", fmt.Sprint(limit))
			if exit != ExitOK || len(result["data"].([]any)) != limit || result["next_page_token"] != fmt.Sprint(limit) || len(sizes) != (limit+99)/100 {
				t.Fatalf("exit=%d page sizes=%v result=%v", exit, sizes, result)
			}
		})
	}
}
