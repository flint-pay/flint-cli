package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPGatePreparedRequestIsApprovedOnce(t *testing.T) {
	for _, tc := range []struct {
		name, environment string
		reply             map[string]any
		writes, asks      int
	}{
		{"sandbox", "sandbox", map[string]any{"result": map[string]any{"action": "accept", "content": map[string]any{"approve": true}}}, 1, 0},
		{"live approved", "live", map[string]any{"result": map[string]any{"action": "accept", "content": map[string]any{"approve": true}}}, 1, 1},
		{"live unchecked", "live", map[string]any{"result": map[string]any{"action": "accept", "content": map[string]any{"approve": false}}}, 0, 1},
		{"legacy confirm is not approval", "live", map[string]any{"result": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}}, 0, 1},
		{"live declined", "live", map[string]any{"result": map[string]any{"action": "decline"}}, 0, 1},
		{"unverified approved", "unverified", map[string]any{"result": map[string]any{"action": "accept", "content": map[string]any{"approve": true}}}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var auths, writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					auths.Add(1)
					if tc.environment == "unverified" {
						w.WriteHeader(403)
						fmt.Fprint(w, `{"error":{"code":"FORBIDDEN","message":"Cannot introspect this token.","remediation":{"retry":"none"}}}`)
					} else {
						fmt.Fprint(w, authContextJSON(tc.environment))
					}
					return
				}
				writes.Add(1)
				if r.Method != "POST" || r.URL.Path != "/v1/refunds" || r.Header.Get("Idempotency-Key") != "one-prepared-key" {
					t.Errorf("unexpected request: %s %s key=%q", r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"))
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if lookupPathValue(body, "amount_money.amount") != float64(2500) {
					t.Errorf("request changed after approval: %#v", body)
				}
				fmt.Fprint(w, `{"data":{"refund_id":"ref_fixture","status":"pending"}}`)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			if tc.environment == "unverified" {
				t.Setenv("FLINT_API_KEY", "")
				t.Setenv("FLINT_ACCESS_TOKEN", "partner_synthetic")
			}
			cmd, _ := app.Registry.ByName("refunds.create")
			args := map[string]any{"order_id": "ord_fixture", "refund_type": "amount", "amount_money": map[string]any{"amount": 2500, "currency": "USD"}, "_flint": map[string]any{"confirm": true, "idempotency_key": "one-prepared-key"}}
			if tc.environment == "live" {
				args["_flint"].(map[string]any)["live"] = true
			}
			asks := 0
			var session *mcpSession
			session = newMCPSession(func(message map[string]any) error {
				if message["method"] != "elicitation/create" {
					return nil
				}
				asks++
				prompt := fmt.Sprint(lookupPathValue(message, "params.message"))
				for _, want := range []string{"refunds.create", "25.00 USD (amount_money)", "ord_fixture", "POST /v1/refunds", "Idempotency key: one-prepared-key"} {
					if !strings.Contains(prompt, want) {
						t.Errorf("prompt lacks %q: %s", want, prompt)
					}
				}
				if lookupPathValue(message, "params.requestedSchema.properties.approve.type") != "boolean" {
					t.Errorf("approval schema=%#v", message)
				}
				if writes.Load() != 0 {
					t.Error("mutation dispatched before approval")
				}
				// The operation must use the already prepared request, not rebuild from
				// input or execute the command a second time after the user accepts.
				args["amount_money"].(map[string]any)["amount"] = 9999
				session.deliver(message["id"], tc.reply)
				return nil
			})
			session.configure(map[string]any{"elicitation": map[string]any{}}, latestMCPProtocolVersion)
			ctx := context.WithValue(context.Background(), mcpSessionKey{}, session)
			result := app.runMCPTool(ctx, cmd, args, defaultOptions(), mcpPolicy{allowLive: tc.environment == "live"}, false)
			if writes.Load() != int32(tc.writes) || asks != tc.asks || auths.Load() != 1 {
				t.Fatalf("writes=%d asks=%d auths=%d result=%s", writes.Load(), asks, auths.Load(), mcpResultText(result))
			}
			if tc.writes == 0 {
				envelope := mcpErrorJSON(t, result)
				if lookupPathValue(envelope, "error.code") != "CONFIRMATION_DECLINED" {
					t.Fatal(envelope)
				}
			} else if result["isError"] == true {
				t.Fatal(mcpResultText(result))
			}
		})
	}
}

func TestMCPGatePublicControlsPrecedeInheritedFlags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		controls map[string]any
		code     string
		auths    int
	}{
		{"invalid confirm", map[string]any{"confirm": "true"}, "INVALID_REQUEST", 0},
		{"invalid live", map[string]any{"live": "true"}, "INVALID_REQUEST", 0},
		{"server live is not call acknowledgement", map[string]any{"confirm": true}, "LIVE_ACKNOWLEDGEMENT_REQUIRED", 1},
		{"server confirm is not call confirmation", map[string]any{"live": true}, "CONFIRMATION_REQUIRED", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads, writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					reads.Add(1)
					fmt.Fprint(w, authContextJSON("live"))
					return
				}
				writes.Add(1)
				fmt.Fprint(w, `{"data":{}}`)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			cmd, _ := app.Registry.ByName("refunds.create")
			tc.controls["idempotency_key"] = "public-control-key"
			args := map[string]any{"refund_type": "full", "payment_intent_id": "pi_fixture", "_flint": tc.controls}
			opts := mcpServeOptions(t, "--live")
			// Internal callers can supply parsed options directly. These values
			// still cannot stand in for the tool call's public controls.
			opts.Confirm = true
			opts.Raw["confirm"] = []string{"true"}
			result := app.runMCPTool(context.Background(), cmd, args, opts, mcpPolicy{allowLive: true, allowDestructive: true}, false)
			envelope := mcpErrorJSON(t, result)
			if lookupPathValue(envelope, "error.code") != tc.code || reads.Load() != int32(tc.auths) || writes.Load() != 0 {
				t.Fatalf("reads=%d writes=%d error=%#v", reads.Load(), writes.Load(), envelope)
			}
			if len(opts.Raw["live"]) == 0 {
				t.Fatal("child changed the server options")
			}
		})
	}
}

func TestMCPGateAuthenticatedLocalToolsUseActualEnvironment(t *testing.T) {
	for _, name := range []string{"auth.status", "listen"} {
		t.Run(name, func(t *testing.T) {
			server, api := newMCPTestAPI(t, "live")
			app, _, _ := testApp(t, server.URL)
			cmd := mustMCPTool(t, app, name)
			result := app.runMCPTool(context.Background(), cmd, map[string]any{}, defaultOptions(), mcpPolicy{}, false)
			envelope := mcpErrorJSON(t, result)
			if lookupPathValue(envelope, "error.code") != "LIVE_ACCESS_DISABLED" || lookupPathValue(envelope, "cli.environment") != "live" || len(api.seen()) != 0 {
				t.Fatalf("authenticated local tool bypassed live policy: %#v requests=%v", envelope, api.seen())
			}
		})
	}
}

func TestMCPGateHistoryClearUsesSandboxConfirmation(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			app, _, _ := testApp(t, "")
			if err := app.saveHistory(History{Entries: []HistoryEntry{{ID: "cus_fixture", CreatedAt: time.Now()}}}); err != nil {
				t.Fatal(err)
			}
			index, err := app.operationIndex()
			if err != nil {
				t.Fatal(err)
			}
			app.mcpAuth = &mcpAuthState{index: index, known: true, auth: AuthContext{AuthType: "api_key", Environment: "live"}}
			session := newMCPSession(func(message map[string]any) error {
				t.Errorf("history requested human approval: %#v", message)
				return errors.New("unexpected elicitation")
			})
			session.configure(map[string]any{"elicitation": map[string]any{}}, latestMCPProtocolVersion)
			ctx := context.WithValue(context.Background(), mcpSessionKey{}, session)
			cmd := mustMCPTool(t, app, "history.clear")
			result := app.runMCPTool(ctx, cmd, map[string]any{"_flint": map[string]any{"confirm": confirmed}}, defaultOptions(), mcpPolicy{}, false)
			history, err := app.loadHistory()
			if err != nil {
				t.Fatal(err)
			}
			if confirmed {
				if result["isError"] == true || len(history.Entries) != 0 {
					t.Fatalf("history not cleared: %s %#v", mcpResultText(result), history)
				}
			} else {
				envelope := mcpErrorJSON(t, result)
				if lookupPathValue(envelope, "error.code") != "CONFIRMATION_REQUIRED" || len(history.Entries) != 1 {
					t.Fatal(envelope)
				}
			}
			if meta, _ := result["_meta"].(map[string]any); meta[mcpEnvironmentMetaKey] != "sandbox" {
				t.Fatal(result)
			}
		})
	}
}

func TestMCPGateApprovalFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		reply        map[string]any
		writeError   bool
	}{
		{name: "no answer", reason: "no_answer"},
		{name: "client error", reason: "elicitation_failed", reply: map[string]any{"error": map[string]any{"code": -32603}}},
		{name: "transport error", reason: "elicitation_failed", writeError: true},
		{name: "declined", reason: "declined", reply: map[string]any{"result": map[string]any{"action": "decline"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _, _ := testApp(t, "")
			cmd := mustMCPTool(t, app, "api-keys.revoke")
			var session *mcpSession
			session = newMCPSession(func(message map[string]any) error {
				if message["method"] != "elicitation/create" {
					return nil
				}
				if tc.writeError {
					return errors.New("test transport failure")
				}
				if tc.reply != nil {
					session.deliver(message["id"], tc.reply)
				}
				return nil
			})
			session.configure(map[string]any{"elicitation": map[string]any{}}, latestMCPProtocolVersion)
			gate, e := app.newMCPCallGate(context.WithValue(context.Background(), mcpSessionKey{}, session), cmd, map[string]any{"_flint": map[string]any{"confirm": true, "live": true}}, mcpPolicy{allowLive: true})
			if e != nil {
				t.Fatal(e)
			}
			if gate.approvalTimeout != 10*time.Minute {
				t.Fatalf("approval timeout=%s", gate.approvalTimeout)
			}
			gate.environment = "live"
			gate.approvalTimeout = time.Millisecond
			e = gate.ask(context.Background(), preparedRequest{Method: "POST", Path: "/v1/api-keys/key_fixture/revoke"})
			if e == nil || e.Code != "CONFIRMATION_DECLINED" || mcpErrorFact(e.Details, "reason") != tc.reason {
				t.Fatalf("error=%#v", e)
			}
		})
	}
}
