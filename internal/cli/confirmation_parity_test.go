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
				collectionRoute := strings.HasPrefix(entry.Path, "/v1/orders/") || strings.HasPrefix(entry.Path, "/v1/payment-intents/") || strings.HasPrefix(entry.Path, "/v1/invoices/") || strings.HasPrefix(entry.Path, "/v1/gift-card-redemptions/")
				collection := collectionRoute && (strings.HasSuffix(entry.Path, "/pay") || strings.HasSuffix(entry.Path, "/confirm") || strings.HasSuffix(entry.Path, "/capture") || strings.HasSuffix(entry.Path, "/collect"))
				want := entry.Kind == "write" && (environment == "sandbox" && entry.Irreversible && !collection || environment != "sandbox" && entry.ConfirmRequired())
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
		{name: "sandbox reversible create", environment: "sandbox", args: []string{"payment-intents", "create", "--input", `{"amount_money":{"amount":500,"currency":"USD"},"payment_options":["card"]}`, "--idempotency-key", "parity-test"}, wantWrites: 1},
		{name: "live sensitive accepted", environment: "live", args: []string{"payment-intents", "create", "--input", `{"amount_money":{"amount":500,"currency":"USD"},"payment_options":["card"]}`, "--idempotency-key", "parity-test", "--live"}, tty: true, input: "yes\n", wantWrites: 1},
		{name: "sandbox irreversible", environment: "sandbox", args: []string{"invoices", "void", "inv_test"}, wantExit: ExitConfirmation},
		{name: "close is destructive in sandbox", environment: "sandbox", args: []string{"orders", "close", "ord_test"}, wantExit: ExitConfirmation},
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
	cmd, _ := app.Registry.ByName("invoices.void")
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

func TestRawLiveConfirmationRemovalsAreUngated(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	for _, environment := range []string{"live", environmentUnverified} {
		removed := 0
		for path, methods := range doc.Paths {
			for method, operation := range methods {
				if operation.OperationID == "" {
					continue
				}
				entry, found := index.OperationSafety(operation.OperationID)
				if !found {
					t.Fatal(operation.OperationID)
				}
				mutation := method != "get" && method != "head" || operation.OperationID == "authorizePartnerInstall"
				oldRequired := mutation || operation.FlintDestructive
				cmd := &Command{CanonicalName: "api", OperationID: operation.OperationID, Method: strings.ToUpper(method), APIPath: path, Mutation: mutation, Sensitive: mutation, Destructive: method == "delete" || operation.FlintDestructive}
				e := app.confirmCommand(cmd, AuthContext{Environment: environment}, Options{NoInput: true})
				if entry.Kind == "write" && entry.ConfirmRequired() && (e == nil || e.ExitCode != ExitConfirmation) {
					t.Fatalf("gated operation lost %s confirmation: %s %s %+v error=%v", environment, method, path, entry, e)
				}
				if oldRequired && e == nil {
					removed++
					if entry.Sensitive || entry.Irreversible {
						t.Fatalf("unsafe removal: %s %s %+v", method, path, entry)
					}
				}
			}
		}
		if removed == 0 {
			t.Fatal("removal proof did not exercise any routes")
		}
		t.Logf("%s: all %d raw confirmation removals are ungated", environment, removed)
	}
}

func TestMoneyOutPublishedIrreversiblePinned(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	// Pin both operation IDs and routes, including the existing cash-out action.
	for _, item := range []struct{ name, id, path string }{
		{"refunds.create", "createRefund", "/v1/refunds"},
		{"payouts.create", "createPayout", "/v1/payouts"},
		{"credit-notes.refunds.create", "createCreditNoteRefund", "/v1/credit-notes/{credit_note_id}/refunds"},
		{"gift-cards.cash-out", "cashOutGiftCard", "/v1/gift-cards/{gift_card_id}/cash-out"},
	} {
		entry, found := index.Lookup(item.name)
		if !found || entry.OperationID != item.id || entry.Path != item.path || entry.Method != "POST" || !entry.Irreversible || !entry.ConfirmRequired() {
			t.Fatalf("money-out safety changed: %s %+v", item.name, entry)
		}
		raw, found := index.OperationSafety(item.id)
		if !found || !raw.Irreversible {
			t.Fatalf("raw money-out safety changed: %s %+v", item.id, raw)
		}
		for _, environment := range []string{"sandbox", "live", environmentUnverified} {
			if !terminalConfirmationRequired(entry, environment) {
				t.Fatalf("%s escaped %s confirmation", item.name, environment)
			}
		}
	}
	for _, name := range []string{"orders.create", "checkout-sessions.create", "payment-intents.create"} {
		entry, found := index.Lookup(name)
		if !found || entry.Irreversible || terminalConfirmationRequired(entry, "sandbox") || !terminalConfirmationRequired(entry, "live") || !terminalConfirmationRequired(entry, environmentUnverified) {
			t.Fatalf("reversible create policy changed: %s %+v", name, entry)
		}
	}
}
