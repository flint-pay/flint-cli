package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Serve API fixtures entirely in memory. These tests need neither network
// access nor a local listener socket.
func signupFlowApp(t *testing.T, handler http.HandlerFunc) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	app, out, stderr := testApp(t, "https://api.example.test")
	t.Setenv("FLINT_API_KEY", "")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
		return w.Result(), nil
	})}
	return app, out, stderr
}

var signupFlowStartArgs = []string{"signup", "--email", "dev@example.com", "--first-name", "Ada", "--last-name", "Lovelace"}

func signupFlowSuccess(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch r.URL.Path {
	case "/v1/onboarding/start":
		fmt.Fprint(w, `{"data":{"verification_token":"synthetic-verification","expires_at":"2099-01-01T00:00:00Z"}}`)
	case "/v1/onboarding/verify-email":
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["verification_token"] != "synthetic-verification" || body["verification_code"] != "123456" {
			t.Fatalf("verify body = %#v", body)
		}
		fmt.Fprint(w, `{"data":{"onboarding_session_token":"synthetic-session"}}`)
	case "/v1/onboarding/state":
		fmt.Fprint(w, `{"data":{"can_issue_api_key":true,"default_sandbox_id":"test_new"}}`)
	case "/v1/onboarding/api-key":
		fmt.Fprint(w, `{"data":{"api_key_id":"key_new","merchant_id":"mer_new","secret_key":"flint_test_synthetic_new"}}`)
	case "/v1/developer/auth-context":
		fmt.Fprint(w, authContextJSON("sandbox"))
	case "/v1/payment-intents":
		fmt.Fprint(w, `{"data":[]}`)
	default:
		t.Fatalf("unexpected request: %s", r.URL.Path)
	}
}

func signupFlowReject(w http.ResponseWriter, code, message string) {
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `{"error":{"type":"authentication_error","code":%q,"message":%q}}`, code, message)
}

func TestSignupFlowTwoPhase(t *testing.T) {
	for _, output := range []string{"json", "human"} {
		t.Run(output, func(t *testing.T) {
			var paths []string
			app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				signupFlowSuccess(t, w, r)
			})
			var stored string
			app.StoreCredential = func(_ string, secret string) error { stored = secret; return nil }
			app.LoadCredential = func(string) (string, error) { return stored, nil }
			if err := app.updateConfig(func(cfg *Config) error {
				cfg.Profiles["default"] = Profile{ContextID: "ctx_previous", MerchantGuard: "mer_previous", MerchantID: "mer_previous"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			args := append(append([]string{}, signupFlowStartArgs...), "--no-input", "--output", output)
			if exit := app.Run(args); exit != ExitOK || stderr.Len() != 0 || len(paths) != 1 || stored != "" {
				t.Fatalf("start exit=%d paths=%v out=%s stderr=%s", exit, paths, out, stderr)
			}
			if !strings.Contains(out.String(), "flint signup --verification-code <code>") || strings.Contains(out.String(), "synthetic-verification") {
				t.Fatalf("start output = %s", out)
			}
			if output == "json" && !strings.Contains(out.String(), `"status":"verification_required"`) {
				t.Fatalf("missing verification_required: %s", out)
			}
			cfg, err := app.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			pending := cfg.Profiles["default"].PendingSignup
			if pending == nil || pending.Email != "dev@example.com" || pending.FirstName != "Ada" || pending.LastName != "Lovelace" || pending.VerificationToken != "synthetic-verification" {
				t.Fatalf("pending signup = %#v", pending)
			}
			path, _ := app.configPath()
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("config permissions: %v %v", info, err)
			}
			out.Reset()
			if exit := app.Run([]string{"config", "get", "--output", "json"}); exit != ExitOK || strings.Contains(out.String(), "synthetic-verification") {
				t.Fatalf("config exposed challenge: %s", out)
			}
			out.Reset()
			// A separate App represents a later process loading the saved profile.
			resumed := *app
			if exit := resumed.Run([]string{"signup", "--verification-code", "123456", "--no-input", "--output", output}); exit != ExitOK {
				t.Fatalf("finish exit=%d out=%s stderr=%s", exit, out, stderr)
			}
			if !reflect.DeepEqual(paths, []string{"/v1/onboarding/start", "/v1/onboarding/verify-email", "/v1/onboarding/state", "/v1/onboarding/api-key"}) {
				t.Fatalf("paths = %v", paths)
			}
			cfg, err = app.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.Profiles["default"]
			if p.ContextID != "" || p.MerchantGuard != "" || p.PendingSignup != nil || p.MerchantID != "mer_new" || p.APIKeyID != "key_new" || p.SandboxID != "test_new" || stored != "flint_test_synthetic_new" {
				t.Fatalf("signup did not activate new account: %#v", p)
			}
			if strings.Contains(out.String()+stderr.String(), "synthetic-verification") || strings.Contains(out.String(), stored) {
				t.Fatal("signup output exposed a token or credential")
			}
			if output == "human" {
				want := "You're signed up. Your sandbox is ready for test payments.\n\nMake a test payment:\n  " + "FLINT_BASE_URL='https://api.example.test' " + signupNextCommand + "\n\nPay with card 4242 4242 4242 4242, any future expiry date, and any CVC.\n"
				if out.String() != want {
					t.Fatalf("human output = %s", out)
				}
			} else {
				var result map[string]any
				if err := json.Unmarshal(out.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if valueAt(result, "data.next_command") != "FLINT_BASE_URL='https://api.example.test' "+signupNextCommand || valueAt(result, "data.credential_saved") != true || valueAt(result, "data.api_key.api_key_id") != "key_new" || valueAt(result, "data.onboarding.default_sandbox_id") != "test_new" {
					t.Fatalf("JSON output = %s", out)
				}
			}
			out.Reset()
			if exit := app.Run([]string{"payment-intents", "list", "--output", "json"}); exit != ExitOK {
				t.Fatalf("new account unusable: exit=%d out=%s stderr=%s", exit, out, stderr)
			}
		})
	}
}

func TestSignupFlowMCP(t *testing.T) {
	var starts int
	app, _, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/onboarding/start" {
			starts++
		}
		signupFlowSuccess(t, w, r)
	})
	for i, args := range []map[string]any{
		{"email": "dev@example.com", "first_name": "Ada", "last_name": "Lovelace"},
		{"verification_code": "123456"},
	} {
		response := app.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: i + 1, Method: "tools/call", Params: map[string]any{"name": "signup", "arguments": args}}, defaultOptions())
		if valueAt(response, "result.isError") == true || valueAt(response, "error") != nil {
			t.Fatalf("MCP response = %#v", response)
		}
		raw, _ := json.Marshal(response)
		if strings.Contains(string(raw), "synthetic-verification") || strings.Contains(string(raw), "flint_test_synthetic_new") || (i == 0 && !strings.Contains(string(raw), "verification_required")) || (i == 1 && !strings.Contains(string(raw), "credential_saved")) {
			t.Fatalf("MCP response = %s", raw)
		}
	}
	if starts != 1 {
		t.Fatalf("MCP restarted signup %d times", starts)
	}
}

func TestSignupFlowInteractiveRetries(t *testing.T) {
	for _, scenario := range []string{"correct second", "flag wrong", "five wrong", "expired resend", "expired decline", "unrelated error"} {
		t.Run(scenario, func(t *testing.T) {
			starts, verifies := 0, 0
			app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/onboarding/start" {
					starts++
				}
				if r.URL.Path == "/v1/onboarding/verify-email" {
					verifies++
					if scenario == "unrelated error" {
						signupFlowReject(w, "ACCOUNT_BLOCKED", "Contact support.")
						return
					}
					if strings.HasPrefix(scenario, "expired") && verifies == 1 {
						signupFlowReject(w, "VERIFICATION_EXPIRED", "The verification code has expired.")
						return
					}
					if scenario == "five wrong" || ((scenario == "correct second" || scenario == "flag wrong") && verifies == 1) {
						signupFlowReject(w, "INVALID_VERIFICATION", "The verification token or code is invalid.")
						return
					}
				}
				signupFlowSuccess(t, w, r)
			})
			app.IsTTY = func() bool { return true }
			inputs := map[string]string{"correct second": "111111\n123456\n", "flag wrong": "123456\n", "five wrong": strings.Repeat("111111\n", 6), "expired resend": "111111\ny\n123456\n", "expired decline": "111111\nn\n", "unrelated error": "111111\n123456\n"}
			app.Stdin = strings.NewReader(inputs[scenario])
			args := append(append([]string{}, signupFlowStartArgs...), "--output", "human")
			if scenario == "flag wrong" {
				args = append(args, "--verification-code", "111111")
			}
			exit := app.Run(args)
			switch scenario {
			case "correct second", "flag wrong":
				if exit != ExitOK || verifies != 2 || !strings.Contains(stderr.String(), "4 attempts remaining") {
					t.Fatalf("retry exit=%d verifies=%d out=%s stderr=%s", exit, verifies, out, stderr)
				}
			case "expired resend":
				if exit != ExitOK || verifies != 2 || starts != 2 || !strings.Contains(stderr.String(), "expired. Send a new code?") {
					t.Fatalf("resend exit=%d starts=%d verifies=%d out=%s stderr=%s", exit, starts, verifies, out, stderr)
				}
			default:
				want := 1
				if scenario == "five wrong" {
					want = 5
				}
				if exit == ExitOK || verifies != want || starts != 1 {
					t.Fatalf("failure exit=%d starts=%d verifies=%d out=%s stderr=%s", exit, starts, verifies, out, stderr)
				}
			}
		})
	}
}

func TestSignupFlowResumeErrors(t *testing.T) {
	for _, scenario := range []string{"missing", "expired", "server expired", "wrong", "exhausted", "server exhausted", "different server", "different email"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch scenario {
				case "server expired":
					signupFlowReject(w, "VERIFICATION_EXPIRED", "Verification code expired.")
				case "server exhausted":
					signupFlowReject(w, "VERIFICATION_ATTEMPTS_EXCEEDED", "Too many incorrect codes.")
				case "wrong":
					signupFlowReject(w, "INVALID_VERIFICATION", "Incorrect verification code.")
				default:
					t.Fatal("resume made an unexpected request")
				}
			})
			if scenario != "missing" {
				pending := &SignupChallenge{VerificationToken: "synthetic-verification", Email: "dev@example.com", FirstName: "Ada", LastName: "Lovelace", BaseURL: "https://api.example.test"}
				if scenario == "expired" {
					pending.ExpiresAt = time.Now().Add(-time.Minute)
				}
				if scenario == "exhausted" {
					pending.Attempts = 5
				}
				if e := app.replaceSignupChallenge("default", nil, pending); e != nil {
					t.Fatal(e)
				}
			}
			args := []string{"signup", "--verification-code", "111111", "--no-input", "--output", "json"}
			if scenario == "different server" {
				t.Setenv("FLINT_BASE_URL", "https://other.example.test")
			}
			if scenario == "different email" {
				args = append(args, "--email", "other@example.com")
			}
			exit := app.Run(args)
			wantCalls := 0
			if strings.HasPrefix(scenario, "server ") || scenario == "wrong" {
				wantCalls = 1
			}
			if exit == ExitOK || calls != wantCalls || strings.Contains(out.String(), "synthetic-verification") {
				t.Fatalf("resume exit=%d calls=%d out=%s", exit, calls, out)
			}
			if scenario != "wrong" && !strings.Contains(strings.ToLower(out.String()), "start again") {
				t.Fatalf("missing restart guidance: %s", out)
			}
			if wantCalls == 1 {
				cfg, err := app.loadConfig()
				if err != nil {
					t.Fatal(err)
				}
				p := cfg.Profiles["default"].PendingSignup
				if (scenario == "wrong" && p.Attempts != 1) || (scenario == "server exhausted" && p.Attempts != 5) || (scenario == "server expired" && (p.Attempts != 0 || p.ExpiresAt.IsZero())) || p.VerificationKey != "" || p.VerificationCode != "" {
					t.Fatalf("failure progress not saved: %#v", p)
				}
			}
		})
	}
}

func TestSignupFlowExistingAccount(t *testing.T) {
	for _, endpoint := range []string{"start", "verify-email"} {
		for _, output := range []string{"human", "json"} {
			t.Run(endpoint+"/"+output, func(t *testing.T) {
				calls := 0
				app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.URL.Path == "/v1/onboarding/"+endpoint {
						w.WriteHeader(http.StatusConflict)
						fmt.Fprint(w, `{"error":{"type":"conflict_error","code":"ACCOUNT_EXISTS","message":"This email already has an account. Run flint login.","remediation":{"next_actions":[{"reason_message":"retry signup"}]}}}`)
						return
					}
					signupFlowSuccess(t, w, r)
				})
				app.IsTTY = func() bool { return true }
				args := append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--output", output)
				exit := app.Run(args)
				wantCalls := 1
				if endpoint == "verify-email" {
					wantCalls = 2
				}
				text := out.String() + stderr.String()
				if exit == ExitOK || calls != wantCalls || !strings.Contains(text, "This email already has an account. Run flint login.") || strings.Contains(text, "retry signup") {
					t.Fatalf("existing account exit=%d calls=%d output=%s", exit, calls, text)
				}
			})
		}
	}
}

func TestSignupFlowLoginApprovalLinks(t *testing.T) {
	for _, args := range [][]string{{"login"}, {"auth", "login"}} {
		for _, complete := range []bool{false, true} {
			t.Run(strings.Join(args, " ")+fmt.Sprint(complete), func(t *testing.T) {
				app, _, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != loginDevicePath {
						t.Fatalf("unexpected login request: %s", r.URL.Path)
					}
					if complete {
						writeTestDevice(w)
					} else {
						fmt.Fprint(w, `{"device_code":"synthetic-device-secret","user_code":"ABCD-EFGH","verification_uri":"https://app.withflintpay.com/cli/activate","expires_in":600,"interval":1}`)
					}
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				app.Context = ctx
				var opened string
				app.OpenBrowser = func(target string) error { opened = target; cancel(); return nil }
				if exit := app.Run(args); exit != ExitAuth {
					t.Fatalf("canceled login exit=%d stderr=%s", exit, stderr)
				}
				want := "https://app.withflintpay.com/cli/activate?user_code=ABCD-EFGH"
				if opened != want || !strings.Contains(stderr.String(), "Open "+want+"\nConfirm this code on the website: ABCD-EFGH\n") {
					t.Fatalf("login opened=%s stderr=%s", opened, stderr)
				}
			})
		}
	}
}

func TestSignupFlowRootHelpOrder(t *testing.T) {
	app, out, _ := testApp(t, "")
	app.printRootHelp()
	start := strings.Split(strings.Split(out.String(), "Start here:\n")[1], "\n\n")[0]
	if !strings.HasPrefix(start, "  flint signup\n  flint login  (alias for flint auth login)\n  flint payment-links create\n  flint payment-intents\n") {
		t.Fatalf("start here = %s", start)
	}
	resources := strings.Split(strings.Split(out.String(), "API resource groups (run flint <resource> --help):\n")[1], "\n\n")[0]
	fields := strings.Fields(resources)
	if len(fields) < 2 || fields[0] != "checkout-sessions" || fields[1] != "payment-intents" {
		t.Fatalf("resource order = %s", resources)
	}
	for _, args := range [][]string{{"signup", "--help"}, {"login", "--help"}, {"payment-links", "create", "--help"}, {"payment-intents", "--help"}} {
		out.Reset()
		if exit := app.Run(args); exit != ExitOK {
			t.Fatalf("help for %v exit=%d", args, exit)
		}
	}
}

func TestSignupFlowAttemptBudgetAcrossRuns(t *testing.T) {
	starts, verifies := 0, 0
	app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/onboarding/start" {
			starts++
			signupFlowSuccess(t, w, r)
			return
		}
		if r.URL.Path != "/v1/onboarding/verify-email" {
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
		verifies++
		signupFlowReject(w, "INVALID_VERIFICATION", "Incorrect verification code.")
	})
	if exit := app.Run(append(append([]string{}, signupFlowStartArgs...), "--no-input", "--output", "json")); exit != ExitOK {
		t.Fatalf("start exit=%d output=%s", exit, out)
	}
	for i := 1; i <= 6; i++ {
		out.Reset()
		if exit := app.Run([]string{"signup", "--verification-code", "111111", "--no-input", "--output", "json"}); exit == ExitOK {
			t.Fatalf("incorrect code succeeded on attempt %d", i)
		}
		if i >= 5 && !strings.Contains(out.String(), "VERIFICATION_ATTEMPTS_EXCEEDED") {
			t.Fatalf("attempt limit missing: %s", out)
		}
	}
	if starts != 1 || verifies != 5 {
		t.Fatalf("starts=%d verifies=%d", starts, verifies)
	}
}

func TestSignupFlowNamedProfile(t *testing.T) {
	app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) { signupFlowSuccess(t, w, r) })
	if err := app.updateConfig(func(cfg *Config) error { cfg.Profiles["development"] = Profile{}; return nil }); err != nil {
		t.Fatal(err)
	}
	if exit := app.Run(append(append([]string{}, signupFlowStartArgs...), "--profile", "development", "--no-input", "--output", "json")); exit != ExitOK {
		t.Fatalf("start exit=%d out=%s", exit, out)
	}
	if !strings.Contains(out.String(), "flint signup --verification-code <code> --profile 'development'") {
		t.Fatalf("finish command lost the profile: %s", out)
	}
	out.Reset()
	// Resume with the original API origin even after the environment is cleared.
	t.Setenv("FLINT_BASE_URL", "")
	if exit := app.Run([]string{"signup", "--profile", "development", "--verification-code", "123456", "--no-input", "--output", "json"}); exit != ExitOK {
		t.Fatalf("finish exit=%d out=%s", exit, out)
	}
	cfg, err := app.loadConfig()
	if err != nil || cfg.Profiles["development"].PendingSignup != nil || cfg.Profiles["development"].APIKeyID != "key_new" || cfg.Profiles["default"].APIKeyID != "" {
		t.Fatalf("profile activation = %#v err=%v", cfg.Profiles, err)
	}
}

func TestSignupFlowNoOpenLoginLink(t *testing.T) {
	app, _, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == loginDevicePath {
			writeTestDevice(w)
			return
		}
		if r.URL.Path != loginTokenPath {
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"access_denied"}`)
	})
	app.OpenBrowser = func(string) error { t.Fatal("--no-open opened the browser"); return nil }
	if exit := app.Run([]string{"login", "--no-open"}); exit != ExitAuth || !strings.Contains(stderr.String(), "Open https://app.withflintpay.com/cli/activate?user_code=ABCD-EFGH\nConfirm this code on the website: ABCD-EFGH\n") {
		t.Fatalf("login exit=%d stderr=%s", exit, stderr)
	}
}

func TestSignupFlowHumanOutputTransforms(t *testing.T) {
	app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) { signupFlowSuccess(t, w, r) })
	if exit := app.Run(append(append([]string{}, signupFlowStartArgs...), "--no-input", "--field", "data.status")); exit != ExitOK || strings.TrimSpace(out.String()) != "verification_required" {
		t.Fatalf("start field exit=%d out=%s", exit, out)
	}
	out.Reset()
	if exit := app.Run([]string{"signup", "--verification-code", "123456", "--no-input", "--field", "data.next_command"}); exit != ExitOK || strings.TrimSpace(out.String()) != "FLINT_BASE_URL='https://api.example.test' "+signupNextCommand {
		t.Fatalf("finish field exit=%d out=%s", exit, out)
	}
}

func TestSignupFlowCancellationAndPersistence(t *testing.T) {
	for _, scenario := range []string{"canceled start", "config failure", "keychain failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			revoked := false
			var app *App
			app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/api-keys/key_new/revoke" {
					revoked = true
					fmt.Fprint(w, `{"data":{"revoked":true}}`)
					return
				}
				if scenario == "canceled start" && r.URL.Path == "/v1/onboarding/start" {
					cancel()
				}
				signupFlowSuccess(t, w, r)
			})
			app.Context = ctx
			stored := "flint_test_previous"
			app.LoadCredential = func(string) (string, error) { return stored, nil }
			app.StoreCredential = func(_ string, secret string) error {
				if scenario == "canceled start" {
					t.Fatal("canceled signup stored credentials")
				}
				stored = secret
				if secret != "flint_test_synthetic_new" {
					return nil
				}
				if scenario == "keychain failure" {
					return fmt.Errorf("synthetic keychain failure")
				}
				path, _ := app.configPath()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				return nil
			}
			args := append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input", "--output", "json")
			exit := app.Run(args)
			if scenario == "canceled start" {
				if exit != ExitNetwork || revoked || !strings.Contains(out.String(), "REQUEST_CANCELED") {
					t.Fatalf("cancellation exit=%d revoked=%t out=%s", exit, revoked, out)
				}
			} else if exit != ExitAuth || !revoked || stored != "flint_test_previous" {
				t.Fatalf("persistence exit=%d revoked=%t stored=%s out=%s", exit, revoked, stored, out)
			}
		})
	}
}

func TestSignupFlowStoredExpiryResend(t *testing.T) {
	starts, verifies := 0, 0
	app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/onboarding/start" {
			starts++
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["email"] != "dev@example.com" || body["first_name"] != "Ada" || body["last_name"] != "Lovelace" {
				t.Fatalf("resend lost signup details: %#v err=%v", body, err)
			}
		}
		if r.URL.Path == "/v1/onboarding/verify-email" {
			verifies++
		}
		signupFlowSuccess(t, w, r)
	})
	pending := &SignupChallenge{VerificationToken: "synthetic-expired", Email: "dev@example.com", FirstName: "Ada", LastName: "Lovelace", BaseURL: "https://api.example.test", ExpiresAt: time.Now().Add(-time.Minute)}
	if e := app.replaceSignupChallenge("default", nil, pending); e != nil {
		t.Fatal(e)
	}
	app.IsTTY = func() bool { return true }
	app.Stdin = strings.NewReader("yes\n123456\n")
	if exit := app.Run([]string{"signup", "--verification-code", "111111", "--output", "human"}); exit != ExitOK || starts != 1 || verifies != 1 || !strings.Contains(stderr.String(), "expired. Send a new code?") {
		t.Fatalf("resend exit=%d starts=%d verifies=%d out=%s stderr=%s", exit, starts, verifies, out, stderr)
	}
}
