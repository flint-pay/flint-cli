package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTerminalConfirmationMatchesSharedIndex(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range app.Registry.Commands {
		if cmd.Local {
			continue
		}
		entry, found := index.Lookup(cmd.CanonicalName)
		if !found {
			t.Fatal(cmd.Name)
		}
		for _, environment := range []string{"sandbox", "live", environmentUnverified} {
			for _, raw := range []bool{false, true} {
				candidate := *cmd
				// Legacy hints must neither add nor remove a confirmation gate.
				candidate.Sensitive = !entry.Sensitive
				candidate.Destructive = !entry.Irreversible
				if raw {
					candidate.CanonicalName = "api"
				}
				e := app.confirmCommand(&candidate, AuthContext{Environment: environment}, Options{NoInput: true})
				want := entry.Kind == "write" && entry.ConfirmRequired()
				if (e != nil) != want || e != nil && e.ExitCode != ExitConfirmation {
					t.Fatalf("%s env=%s raw=%v entry=%+v error=%v", cmd.Name, environment, raw, entry, e)
				}
				for _, opts := range []Options{{Confirm: true}, {DryRun: "client"}, {Preview: true}} {
					if e := app.confirmCommand(&candidate, AuthContext{Environment: environment}, opts); e != nil {
						t.Fatalf("%s approved/dry run: %v", cmd.Name, e)
					}
				}
			}
		}
	}
}

func TestTerminalConfirmationParityDispatch(t *testing.T) {
	for _, scenario := range []struct {
		name, environment, input string
		args                     []string
		tty                      bool
		wantExit, wantWrites     int
	}{
		{name: "sandbox sensitive", environment: "sandbox", args: []string{"payment-intents", "create", "--input", `{"amount_money":{"amount":500,"currency":"USD"},"payment_options":["card"]}`, "--idempotency-key", "parity-test"}, wantExit: ExitConfirmation},
		{name: "sandbox sensitive accepted", environment: "sandbox", args: []string{"payment-intents", "create", "--input", `{"amount_money":{"amount":500,"currency":"USD"},"payment_options":["card"]}`, "--idempotency-key", "parity-test"}, tty: true, input: "yes\n", wantWrites: 1},
		{name: "sandbox irreversible", environment: "sandbox", args: []string{"invoices", "void", "inv_test"}, wantExit: ExitConfirmation},
		{name: "close stays sensitive", environment: "sandbox", args: []string{"orders", "close", "ord_test"}, wantExit: ExitConfirmation},
		{name: "ordinary raw live create", environment: "live", args: []string{"api", "post", "/v1/customers", "--input", `{"name":"Parity"}`, "--live"}, wantWrites: 1},
		{name: "ordinary raw live overwrite", environment: "live", args: []string{"api", "patch", "/v1/customers/cus_test", "--input", `{"name":"Parity"}`, "--live"}, wantWrites: 1},
		{name: "raw irreversible", environment: "sandbox", args: []string{"api", "post", "/v1/invoices/inv_test/void"}, wantExit: ExitConfirmation},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var writes atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON(scenario.environment))
					return
				}
				writes.Add(1)
				fmt.Fprint(w, `{"data":{"customer_id":"cus_test","status":"succeeded"}}`)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			if scenario.environment == "live" {
				t.Setenv("FLINT_API_KEY", "flint_live_test")
			}
			app.IsTTY = func() bool { return scenario.tty }
			app.Stdin = strings.NewReader(scenario.input)
			args := append([]string{}, scenario.args...)
			if !scenario.tty {
				args = append(args, "--output", "json")
			}
			exit := app.Run(args)
			if exit != scenario.wantExit || int(writes.Load()) != scenario.wantWrites {
				t.Fatalf("exit=%d writes=%d out=%s stderr=%s", exit, writes.Load(), out, stderr)
			}
		})
	}
}

// Gate checks have no transport effects even for callers that prepare raw routes.
func TestTerminalConfirmationDoesNotSendRequests(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("gate sent HTTP"); return nil, nil })}
	cmd, _ := app.Registry.ByName("refunds.create")
	if e := app.confirmCommand(cmd, AuthContext{Environment: "sandbox"}, Options{NoInput: true}); e == nil {
		t.Fatal("missing confirmation")
	}
}

func TestUnknownRawConfirmationFallback(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	for _, environment := range []string{"sandbox", "live", environmentUnverified} {
		for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
			raw, _ := app.Registry.ByName("api")
			cmd := resolveAPICommand(raw, Options{Positionals: []string{method, "/v1/future-resources"}})
			want := method == "DELETE" || method != "GET" && environment != "sandbox"
			e := app.confirmCommand(cmd, AuthContext{Environment: environment}, Options{NoInput: true})
			if (e != nil) != want {
				t.Fatalf("%s %s error=%v want=%v", environment, method, e, want)
			}
			if e := app.confirmCommand(cmd, AuthContext{Environment: environment}, Options{Confirm: true}); e != nil {
				t.Fatal(e)
			}
		}
	}
}
