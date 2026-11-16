package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

const admissionFixture = "0123456789AB1234"

func TestTicketDryRunsRedactAdmissionCodes(t *testing.T) {
	for _, operation := range []string{"lookup", "check-in"} {
		t.Run(operation, func(t *testing.T) {
			app, out, _ := testApp(t, "")
			app.Stdin = strings.NewReader(`{"admission_code":"` + admissionFixture + `"}`)
			args := []string{"tickets", operation}
			body := map[string]any{"admission_code": admissionFixture}
			if operation == "check-in" {
				args = append(args, "tkt_fixture")
				body["ticket_id"] = "tkt_fixture"
			}
			args = append(args, "--input", "-", "--dry-run=client", "--no-input")
			exit, value := runJSON(t, app, out, args...)
			if exit != ExitOK || lookupPathValue(value, "data.body.admission_code") != "[REDACTED]1234" || strings.Contains(out.String(), admissionFixture) {
				t.Fatalf("exit=%d result=%v", exit, value)
			}
			result := mcpCall(t, app, defaultOptions(), "flint_write", map[string]any{"operation": "tickets." + operation, "arguments": body, "dry_run": true})
			value = mcpResultJSON(t, result)
			if result["isError"] == true || lookupPathValue(value, "data.body.admission_code") != "[REDACTED]1234" || lookupPathValue(value, "data.effect.sensitive") != true || strings.Contains(mcpResultText(result), admissionFixture) {
				t.Fatalf("MCP=%v", result)
			}
			if body["admission_code"] != admissionFixture {
				t.Fatal("display changed the supplied credential")
			}
		})
	}
}

func TestTicketExecutionRetainsAdmissionCode(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		writes++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["admission_code"] != admissionFixture {
			t.Errorf("admission credential changed in execution: %v", err)
		}
		fmt.Fprint(w, `{"data":{"ticket_id":"tkt_fixture","status":"checked_in","admission_code_last4":"1234"}}`)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	app.Stdin = strings.NewReader(`{"admission_code":"` + admissionFixture + `"}`)
	exit, _ := runJSON(t, app, out, "tickets", "check-in", "tkt_fixture", "--input", "-", "--no-input")
	if exit != ExitOK || writes != 1 || strings.Contains(out.String(), admissionFixture) {
		t.Fatalf("exit=%d writes=%d", exit, writes)
	}
}

func TestAdmissionCodeConfirmationDisplayAndFallback(t *testing.T) {
	app, _, _ := testApp(t, "")
	cmd := mustMCPTool(t, app, "tickets.check-in")
	body := map[string]any{"admission_code": admissionFixture}
	raw, _ := json.Marshal(body)
	req := preparedRequest{Method: cmd.Method, Path: "/v1/tickets/tkt_fixture/check-in", Body: raw, BodyValue: body}
	prompt := ""
	var session *mcpSession
	session = newMCPSession(func(message map[string]any) error {
		if message["method"] == "elicitation/create" {
			prompt = fmt.Sprint(lookupPathValue(message, "params.message"))
			session.deliver(message["id"], map[string]any{"result": map[string]any{"action": "decline"}})
		}
		return nil
	})
	session.configure(map[string]any{"elicitation": map[string]any{}}, latestMCPProtocolVersion)
	gate := &mcpCallGate{entry: opindex.Entry{ID: "tickets.check-in", Title: "Check in a ticket"}, session: session, environment: "live", approvalTimeout: time.Second}
	if e := gate.ask(context.Background(), req); e == nil || e.Code != "CONFIRMATION_DECLINED" {
		t.Fatalf("approval result=%v", e)
	}
	if strings.Contains(prompt, admissionFixture) || !strings.Contains(prompt, "[REDACTED]1234") {
		t.Fatal("confirmation failed to mask admission credential")
	}
	command, err := app.cliCommandFor(cmd, req, ResolvedConfig{})
	if err != nil || strings.Contains(command, admissionFixture) || !strings.Contains(command, "--input=-") {
		t.Fatalf("fallback=%s err=%v", command, err)
	}
	if string(req.Body) != string(raw) || req.BodyValue["admission_code"] != admissionFixture {
		t.Fatal("confirmation changed prepared request")
	}
}

func TestAdmissionCodeDisplayShortAndFormattedCodes(t *testing.T) {
	for _, tc := range []struct{ code, want string }{{"1234", "[REDACTED]"}, {"", "[REDACTED]"}, {"0123-4567-89AB-1234 ", "[REDACTED]1234"}} {
		body := map[string]any{"tickets": []any{map[string]any{"admission_code": tc.code}}}
		display, sensitive := displayRequestBody(body)
		if !sensitive || lookupPathValue(display, "tickets.0.admission_code") != tc.want || lookupPathValue(body, "tickets.0.admission_code") != tc.code {
			t.Fatalf("masked display incorrect for fixture length=%d", len(tc.code))
		}
	}
}
