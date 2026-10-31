package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTerminalPublishedIrreversible(t *testing.T) {
	for _, scenario := range []struct {
		name, input          string
		tty, confirm, raw    bool
		wantExit, wantWrites int
	}{
		{name: "headless", wantExit: ExitConfirmation},
		{name: "raw headless", raw: true, wantExit: ExitConfirmation},
		{name: "raw approved", raw: true, tty: true, input: "yes\n", wantWrites: 1},
		{name: "declined", tty: true, input: "n\n", wantExit: ExitConfirmation},
		{name: "approved", tty: true, input: "yes\n", wantWrites: 1},
		{name: "explicit", confirm: true, wantWrites: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				writes++
				if r.Method != "POST" || r.URL.Path != "/v1/invoices/inv_fixture/payment-attempts/ipa_fixture/cancel" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprint(w, `{"data":{"invoice_payment_attempt_id":"ipa_fixture","status":"canceled"}}`)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			app.IsTTY = func() bool { return scenario.tty }
			app.Stdin = strings.NewReader(scenario.input)
			cmd, _ := app.Registry.ByName("invoices.payment-attempts.cancel")
			index, err := app.operationIndex()
			if err != nil {
				t.Fatal(err)
			}
			entry, ok := index.Lookup(cmd.CanonicalName)
			if !ok || !entry.Irreversible || !cmd.Destructive {
				t.Fatalf("published destructive metadata missing: %+v %+v", cmd, entry)
			}
			argv := []string{"invoices", "payment-attempts", "cancel", "inv_fixture", "ipa_fixture"}
			if scenario.raw {
				argv = []string{"api", "post", "/v1/invoices/inv_fixture/payment-attempts/ipa_fixture/cancel"}
			}
			if scenario.confirm {
				argv = append(argv, "--confirm")
			}
			if !scenario.tty && !scenario.confirm {
				argv = append(argv, "--output", "json")
			}
			exit := app.Run(argv)
			if exit != scenario.wantExit || writes != scenario.wantWrites {
				t.Fatalf("exit=%d writes=%d out=%s stderr=%s", exit, writes, out, stderr)
			}
			if !scenario.tty && !scenario.confirm && !strings.Contains(out.String(), "CONFIRMATION_REQUIRED") {
				t.Fatal(stderr)
			}
			if scenario.tty && !strings.Contains(stderr.String(), "Continue? [y/N]") {
				t.Fatal(stderr)
			}
		})
	}
}
