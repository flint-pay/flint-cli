package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDailyOpsDraftInvoiceConfirmation(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/invoices" {
			writes.Add(1)
		}
		fmt.Fprint(w, `{"data":{"invoice_id":"inv_fixture","status":"draft"}}`)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	for _, tc := range []struct {
		name, fields string
		draft        bool
	}{
		{"draft", "", true},
		{"explicit null schedule", `,"scheduled_issue_at":null`, true},
		{"issue now", `,"issue":{}`, false},
		{"schedule issue", `,"scheduled_issue_at":"2099-01-01T00:00:00Z"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := writes.Load()
			body := `{"type":"order","order_id":"ord_fixture"` + tc.fields + "}"
			exit, _ := runJSON(t, app, out, "invoices", "create", "--input", body, "--idempotency-key", "invoice-test")
			if tc.draft && exit != ExitOK || !tc.draft && exit != ExitConfirmation {
				t.Fatalf("terminal exit %d, draft %v: %s", exit, tc.draft, out)
			}
			previewExit, preview := runJSON(t, app, out, "invoices", "create", "--input", body, "--idempotency-key", "invoice-preview", "--preview")
			if previewExit != ExitOK || lookupPathValue(preview, "data.preview.reversibility.reversible") != tc.draft {
				t.Fatalf("preview exit %d, result %v", previewExit, preview)
			}
			args := map[string]any{"operation": "invoices.create", "arguments": body, "idempotency_key": "invoice-mcp", "dry_run": true}
			dry := mcpResultJSON(t, mcpCall(t, app, defaultOptions(), "flint_write", args))
			if lookupPathValue(dry, "data.effect.confirm_required") != !tc.draft {
				t.Fatalf("effect confirmation mismatch: %v", dry)
			}
			delete(args, "dry_run")
			result := mcpCall(t, app, defaultOptions(), "flint_write", args)
			wantWrites := int32(0)
			if tc.draft {
				wantWrites = 2
			}
			if (result["isError"] == true) == tc.draft || writes.Load()-before != wantWrites {
				t.Fatalf("MCP result %s, writes %d", mcpResultText(result), writes.Load()-before)
			}
		})
	}
}

func TestDailyOpsActiveKeyRevocationWarnsAndRequiresConfirmation(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		writes.Add(1)
		fmt.Fprint(w, `{"data":{"api_key_id":"key_123","status":"revoked"}}`)
	}))
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	exit, _ := runJSON(t, app, out, "api-keys", "revoke", "key_123")
	if exit != ExitConfirmation || !strings.Contains(out.String(), selfRevocationWarning) {
		t.Fatalf("terminal exit %d, output %s", exit, out)
	}
	exit, _ = runJSON(t, app, out, "api-keys", "revoke", "key_other")
	if exit != ExitConfirmation || strings.Contains(out.String(), selfRevocationWarning) {
		t.Fatalf("other key exit %d, output %s", exit, out)
	}
	args := map[string]any{"operation": "api-keys.revoke", "arguments": map[string]any{"api_key_id": "key_123"}}
	result := mcpCall(t, app, defaultOptions(), "flint_write", args)
	if result["isError"] != true || !strings.Contains(mcpResultText(result), selfRevocationWarning) {
		t.Fatalf("MCP confirmation %s", mcpResultText(result))
	}
	args["dry_run"] = true
	result = mcpCall(t, app, defaultOptions(), "flint_write", args)
	if lookupPathValue(mcpResultJSON(t, result), "data.effect.revokes_active_api_key") != true || writes.Load() != 0 {
		t.Fatalf("MCP dry run %s, writes %d", mcpResultText(result), writes.Load())
	}
	delete(args, "dry_run")
	args["confirm"] = true
	result = mcpCall(t, app, defaultOptions(), "flint_write", args)
	if result["isError"] == true || !strings.Contains(fmt.Sprint(result["content"]), selfRevocationWarning) || writes.Load() != 1 {
		t.Fatalf("confirmed MCP result %v, writes %d, stderr %s", result, writes.Load(), stderr)
	}
}

func TestDailyOpsPayoutFlagsRespectRequiredIdempotencyKey(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		writes.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.Method != "POST" || r.URL.Path != "/v1/payouts" || r.Header.Get("Idempotency-Key") != "fixture-payout" ||
			lookupPathValue(body, "amount_money.amount") != float64(500) || lookupPathValue(body, "amount_money.currency") != "USD" {
			t.Errorf("unexpected payout request: %s %s %v", r.Method, r.URL.Path, body)
		}
		fmt.Fprint(w, `{"data":{"payout_id":"po_fixture"}}`)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	args := []string{"payouts", "create", "--amount", "500", "--currency", "USD", "--confirm"}
	exit, _ := runJSON(t, app, out, args...)
	if exit != ExitUsage || !strings.Contains(out.String(), "IDEMPOTENCY_KEY_REQUIRED") || writes.Load() != 0 {
		t.Fatalf("missing key exit %d, writes %d, output %s", exit, writes.Load(), out)
	}
	exit, _ = runJSON(t, app, out, append(args, "--idempotency-key", "fixture-payout")...)
	if exit != ExitOK || writes.Load() != 1 {
		t.Fatalf("payout exit %d, writes %d, output %s", exit, writes.Load(), out)
	}
}

func TestDailyOpsProductHelpAndSimplePriceFlags(t *testing.T) {
	app, out, _ := testApp(t, "http://127.0.0.1:9")
	if exit := app.Run([]string{"products", "create", "--help"}); exit != ExitOK ||
		!strings.Contains(out.String(), "requires default_variant") || !strings.Contains(out.String(), "requires options and variants") {
		t.Fatalf("help exit %d: %s", exit, out)
	}
	exit, result := runJSON(t, app, out, "products", "create", "--name", "Consultation", "--product-type", "service",
		"--variant-mode", "simple", "--price", "5000", "--currency", "USD", "--dry-run=client")
	if exit != ExitOK || fmt.Sprint(lookupPathValue(result, "data.body.default_variant.unit_price_money.amount")) != "5000" ||
		lookupPathValue(result, "data.body.default_variant.unit_price_money.currency") != "USD" {
		t.Fatalf("simple product exit %d, result %v", exit, result)
	}
}
