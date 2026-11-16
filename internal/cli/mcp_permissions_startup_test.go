package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPColdDiscoveryWaitsForPermissions(t *testing.T) {
	var authReads atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth-context" {
			t.Errorf("unexpected request %s", r.URL.Path)
			return
		}
		if authReads.Add(1) == 1 {
			close(entered)
			<-release
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(authContextJSON("sandbox")), &envelope); err != nil {
			t.Error(err)
		}
		envelope["data"].(map[string]any)["scopes"] = []string{"commerce.orders.read", "customers.read"}
		json.NewEncoder(w).Encode(envelope)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	conn := startMCPTestConn(t, app, defaultOptions())
	conn.initialize(`{}`)
	<-entered
	conn.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flint_search","arguments":{"query":"refunds.create","limit":10}}}`)
	select {
	case response := <-conn.messages:
		close(release)
		t.Fatalf("discovery ran before permissions were loaded: %v", response)
	case <-time.After(30 * time.Millisecond):
		close(release)
	}
	response := conn.next()
	text := fmt.Sprint(response)
	if !strings.Contains(text, "commerce.refunds.write") || !strings.Contains(text, "missing_scopes") {
		t.Fatalf("cold search lacks permissions: %v", response)
	}
	conn.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"flint_describe","arguments":{"operation":"refunds.create"}}}`)
	if text := fmt.Sprint(conn.next()); !strings.Contains(text, "commerce.refunds.write") || !strings.Contains(text, "missing_scopes") {
		t.Fatal(text)
	}
	if authReads.Load() != 1 {
		t.Fatalf("auth reads=%d", authReads.Load())
	}
	if exit := conn.close(); exit != ExitOK {
		t.Fatalf("exit=%d", exit)
	}
}

func TestMCPRepeatedDryRunsRetainStartupPermissions(t *testing.T) {
	server, _ := newMCPTestAPI(t, "sandbox")
	app, _, _ := testApp(t, server.URL)
	conn := startMCPTestConn(t, app, defaultOptions())
	conn.initialize(`{}`)
	conn.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flint_describe","arguments":{"operation":"refunds.create"}}}`)
	conn.next() // Completes startup permission loading without an auth.status read.
	for id := 2; id < 5; id++ {
		conn.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"flint_write","arguments":{"operation":"refunds.create","arguments":{"order_id":"ord_effect","refund_type":"amount","amount_money":{"amount":500,"currency":"USD"}},"dry_run":true,"idempotency_key":"repeated-permissions"}}}`, id))
		response := conn.next()
		result, _ := response["result"].(map[string]any)
		value := mcpResultJSON(t, result)
		if lookupPathValue(value, "data.permission_check.status") != "pass" {
			t.Fatalf("run=%d response=%v", id, response)
		}
	}
	if exit := conn.close(); exit != ExitOK {
		t.Fatalf("exit=%d", exit)
	}
}

func TestMCPGuardMismatchRecoveryIsUserOwned(t *testing.T) {
	for _, tc := range []struct{ code, flag string }{{"MERCHANT_GUARD_MISMATCH", "--merchant"}, {"SANDBOX_GUARD_MISMATCH", "--sandbox"}} {
		t.Run(tc.code, func(t *testing.T) {
			app, _, _ := testApp(t, "")
			cmd := mustMCPTool(t, app, "orders.list")
			actions := app.mcpNextActions(cmd, nil, defaultOptions(), mcpPolicy{}, configError(tc.code, "Guard does not match the credential", nil), "sandbox")
			if len(actions) != 1 || actions[0]["actor"] != "user" {
				t.Fatalf("actions=%v", actions)
			}
			text := fmt.Sprint(actions[0]["text"])
			for _, want := range []string{tc.flag, ".flint/config.json", "credential", "Restart", "do not remove a guard"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %s: %v", want, actions)
				}
			}
		})
	}
}

func TestMCPDiscoveryUnknownPermissionIsExplicit(t *testing.T) {
	app, _, _ := testApp(t, "")
	index, e := app.operationIndex()
	if e != nil {
		t.Fatal(e)
	}
	entry, ok := index.Lookup("refunds.create")
	if !ok {
		t.Fatal("missing refund")
	}
	result := mcpDiscoverySummary(entry, index, nil, false)
	if lookupPathValue(result, "permission_check.status") != "unknown" {
		t.Fatal(result)
	}
}
