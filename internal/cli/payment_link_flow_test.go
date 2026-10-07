package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestPaymentLinkAndCheckoutOpen(t *testing.T) {
	const target = "https://checkout.example.test/test-payment"
	for _, command := range []struct {
		name, path, response string
		args                 []string
	}{
		{"payment link", "/v1/payment-links", `{"data":{"payment_link_id":"pl_test","url":"` + target + `"}}`, []string{"payment-links", "create", "--name", "Test payment", "--item-name", "Test payment", "--amount", "1000", "--currency", "USD"}},
		{"checkout", "/v1/checkout-sessions", `{"data":{"checkout_session":{"checkout_session_id":"cs_test","url":"` + target + `"},"checkout_access":{"checkout_auth_token":"csauth_test"}}}`, []string{"checkout", "create", "--quick-pay-name", "Test payment", "--amount", "1000", "--currency", "USD"}},
	} {
		for _, tc := range []struct {
			name            string
			tty, open, fail bool
			browserErr      error
			flags           []string
		}{
			{name: "interactive", tty: true, open: true, flags: []string{"--open"}},
			{name: "without flag", tty: true},
			{name: "false flag", tty: true, flags: []string{"--open=false"}},
			{name: "non-interactive", flags: []string{"--open"}},
			{name: "json", tty: true, flags: []string{"--open", "--output", "json"}},
			{name: "no input", tty: true, flags: []string{"--open", "--no-input"}},
			{name: "dry run", tty: true, flags: []string{"--open", "--dry-run", "client"}},
			{name: "API failure", tty: true, fail: true, flags: []string{"--open"}},
			{name: "browser failure", tty: true, open: true, browserErr: errors.New("browser unavailable"), flags: []string{"--open"}},
		} {
			t.Run(command.name+"/"+tc.name, func(t *testing.T) {
				creates, opened := 0, 0
				app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v1/developer/auth-context":
						fmt.Fprint(w, authContextJSON("sandbox"))
					case command.path:
						creates++
						if r.Method != http.MethodPost {
							t.Fatalf("method=%s", r.Method)
						}
						if tc.fail {
							w.WriteHeader(http.StatusBadRequest)
							fmt.Fprint(w, `{"error":{"type":"validation_error","code":"INVALID_REQUEST","message":"Cannot create payment."}}`)
							return
						}
						fmt.Fprint(w, command.response)
					default:
						t.Fatalf("unexpected request: %s", r.URL.Path)
					}
				})
				t.Setenv("FLINT_API_KEY", "flint_test_synthetic")
				app.IsTTY = func() bool { return tc.tty }
				app.OpenBrowser = func(url string) error {
					opened++
					if url != target {
						t.Errorf("opened=%q want=%q", url, target)
					}
					return tc.browserErr
				}
				args := append(append([]string{}, command.args...), tc.flags...)
				exit := app.Run(args)
				if tc.fail {
					if exit == ExitOK || strings.Contains(out.String(), target) {
						t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
					}
				} else if exit != ExitOK {
					t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
				}
				wantOpened := 0
				if tc.open {
					wantOpened = 1
				}
				if opened != wantOpened {
					t.Fatalf("browser calls=%d want=%d", opened, wantOpened)
				}
				if tc.name == "dry run" {
					if creates != 0 {
						t.Fatalf("preview created %d resources", creates)
					}
				} else {
					if creates != 1 {
						t.Fatalf("creates=%d", creates)
					}
					if !tc.fail && strings.Count(out.String(), target) != 1 {
						t.Fatalf("URL missing or repeated: %s", out)
					}
					if !tc.fail && tc.name != "json" && !strings.HasPrefix(out.String(), target+"\n") {
						t.Fatalf("URL is not the first line: %s", out)
					}
				}
				if tc.browserErr != nil && !strings.Contains(stderr.String(), "warning: could not open browser: browser unavailable") {
					t.Fatalf("browser warning missing: %s", stderr)
				}
				if tc.name == "json" && !json.Valid(out.Bytes()) {
					t.Fatalf("invalid JSON: %s", out)
				}
			})
		}
	}
}

func TestSignupNextPaymentLinkCommand(t *testing.T) {
	args := []string{"payment-links", "create", "--name", "Test payment", "--item-name", "Test payment", "--amount", "1000", "--currency", "USD", "--open"}
	const want = `flint payment-links create --name "Test payment" --item-name "Test payment" --amount 1000 --currency USD --open`
	if signupNextCommand != want {
		t.Fatalf("signup next command=%q want=%q", signupNextCommand, want)
	}
	app, out, stderr := testApp(t, "")
	app.OpenBrowser = func(string) error { t.Fatal("dry run opened browser"); return nil }
	args = append(args, "--dry-run", "client", "--no-input", "--output", "json")
	if exit := app.Run(args); exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if valueAt(result, "data.path") != "/v1/payment-links" || valueAt(result, "data.body.name") != "Test payment" {
		t.Fatalf("payment link request=%s", out)
	}
	items, ok := valueAt(result, "data.body.line_items").([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("line items=%#v", items)
	}
	item := items[0]
	if valueAt(item, "name") != "Test payment" || valueAt(item, "unit_price_money.amount") != float64(1000) || valueAt(item, "unit_price_money.currency") != "USD" {
		t.Fatalf("line item=%#v", item)
	}
}

func TestPaymentLinkOpenHelp(t *testing.T) {
	app, out, stderr := testApp(t, "")
	if exit := app.Run([]string{"payment-links", "create", "--help"}); exit != ExitOK {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	for _, want := range []string{"--open", "interactive terminal", "flint payment-links create --name T-shirt --item-name T-shirt --amount 2500 --currency USD --open"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in help: %s", want, out)
		}
	}
	if exit := app.Run([]string{"payment-links", "list", "--open"}); exit != ExitUsage {
		t.Fatalf("list accepts --open: exit=%d", exit)
	}
}

func TestInitAndDoctorRecommendLogin(t *testing.T) {
	for _, command := range []string{"init", "doctor"} {
		for _, output := range []string{"human", "json"} {
			t.Run(command+"/"+output, func(t *testing.T) {
				app, out, stderr := testApp(t, "")
				t.Setenv("FLINT_API_KEY", "")
				exit := app.Run([]string{command, "--output", output})
				wantExit := ExitOK
				if command == "doctor" {
					wantExit = ExitAuth
				}
				if exit != wantExit || !strings.Contains(out.String(), "flint login") || strings.Contains(out.String(), "flint auth login") {
					t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
				}
			})
		}
	}
}

func TestDoctorRejectedCredentialRecommendsLogin(t *testing.T) {
	for _, source := range []string{"keychain", "environment_access_token"} {
		t.Run(source, func(t *testing.T) {
			app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/developer/auth-context" {
					t.Fatalf("unexpected request: %s", r.URL.Path)
				}
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"code":"INVALID_API_KEY","message":"Credential rejected."}}`)
			})
			app.LoadCredential = func(string) (string, error) { return "flint_test_synthetic", nil }
			if err := app.updateConfig(func(cfg *Config) error {
				cfg.Profiles["development"] = Profile{}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if source == "environment_access_token" {
				t.Setenv("FLINT_ACCESS_TOKEN", testOAuthAccess)
			}
			if exit := app.Run([]string{"doctor", "--profile", "development", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), "flint login --profile development") || strings.Contains(out.String(), "flint auth login") {
				t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
			}
		})
	}
}
