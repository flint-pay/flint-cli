package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func signupTestPending() *SignupChallenge {
	return &SignupChallenge{VerificationToken: "synthetic-verification", Email: "dev@example.com", FirstName: "Ada", LastName: "Lovelace", BaseURL: "https://api.example.test"}
}

func signupTestLoadPending(t *testing.T, app *App) *SignupChallenge {
	t.Helper()
	cfg, err := app.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Profiles["default"].PendingSignup
}

func TestSignupConcurrentStartsPreserveNewerChallenge(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	app, _, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/onboarding/start" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			return
		}
		if starts.Add(1) == 1 {
			close(started)
			<-release
			fmt.Fprint(w, `{"data":{"verification_token":"synthetic-older"}}`)
		} else {
			fmt.Fprint(w, `{"data":{"verification_token":"synthetic-newer"}}`)
		}
	})
	peer := *app
	result := make(chan *CLIError, 1)
	go func() {
		_, e := peer.startSignupChallenge("default", signupTestPending(), nil, defaultOptions())
		result <- e
	}()
	<-started
	_, e := app.startSignupChallenge("default", signupTestPending(), nil, defaultOptions())
	close(release)
	if e != nil {
		t.Fatal(e)
	}
	if e := <-result; e == nil || e.Code != "SIGNUP_CHALLENGE_CHANGED" {
		t.Fatalf("stale start error = %v", e)
	}
	pending := signupTestLoadPending(t, app)
	if pending.VerificationToken != "synthetic-newer" {
		t.Fatalf("new challenge was overwritten: %#v", pending)
	}
	stale := signupTestPending()
	if _, e, _ := app.verifySignupAttempt("default", stale, "123456", defaultOptions()); e == nil || e.Code != "SIGNUP_CHALLENGE_CHANGED" {
		t.Fatalf("stale verification error = %v", e)
	}
	if starts.Load() != 2 || !reflect.DeepEqual(pending, signupTestLoadPending(t, app)) {
		t.Fatal("stale verification changed newer progress")
	}
}

func TestSignupConcurrentAttemptsUseLatestCounter(t *testing.T) {
	var requests atomic.Int32
	app, _, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		signupFlowReject(w, "INVALID_VERIFICATION", "Incorrect verification code.")
	})
	pending := signupTestPending()
	if e := app.replaceSignupChallenge("default", nil, pending); e != nil {
		t.Fatal(e)
	}
	barrier := make(chan struct{})
	results := make(chan *CLIError, 6)
	for i := 0; i < 6; i++ {
		peer, snapshot := *app, *pending
		go func() {
			<-barrier
			opts := defaultOptions()
			opts.NoInput = true
			opts.Raw = map[string][]string{"verification-code": {"111111"}}
			_, e := peer.verifySignupChallenge(bufio.NewReader(strings.NewReader("")), opts, "default", &snapshot)
			results <- e
		}()
	}
	close(barrier)
	for i := 0; i < 6; i++ {
		if e := <-results; e == nil || (e.Code != "INVALID_VERIFICATION" && e.Code != "VERIFICATION_ATTEMPTS_EXCEEDED") {
			t.Fatalf("attempt error = %v", e)
		}
	}
	current := signupTestLoadPending(t, app)
	if requests.Load() != 5 || current.Attempts != 5 || current.VerificationKey != "" {
		t.Fatalf("requests=%d pending=%#v", requests.Load(), current)
	}
}

func TestSignupCompletionPreservesNewerProgress(t *testing.T) {
	issuing, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/onboarding/start" && starts.Add(1) == 2 {
			fmt.Fprint(w, `{"data":{"verification_token":"synthetic-newer"}}`)
			return
		}
		if r.URL.Path == "/v1/onboarding/api-key" {
			close(issuing)
			<-release
		}
		signupFlowSuccess(t, w, r)
	})
	peer := *app
	peer.Stdout, peer.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	finished := make(chan int, 1)
	go func() {
		args := append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input", "--output", "json")
		finished <- app.Run(args)
	}()
	<-issuing
	exit := peer.Run(append(append([]string{}, signupFlowStartArgs...), "--no-input", "--output", "json"))
	close(release)
	if exit != ExitOK {
		t.Fatalf("new signup exit=%d", exit)
	}
	if exit := <-finished; exit != ExitOK {
		t.Fatalf("completion exit=%d out=%s stderr=%s", exit, out, stderr)
	}
	pending := signupTestLoadPending(t, app)
	if pending == nil || pending.VerificationToken != "synthetic-newer" || pending.Attempts != 0 {
		t.Fatalf("completion cleared newer signup: %#v", pending)
	}
}

func TestSignupTransportFailureReplaysBeforeCorrectCode(t *testing.T) {
	for _, scenario := range []string{"last allowed attempt", "different code on resume"} {
		t.Run(scenario, func(t *testing.T) {
			app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) { signupFlowSuccess(t, w, r) })
			var failedKey, failedCode string
			var replayed, correctRequests int
			failTransport := false
			app.HTTPClient.Transport = listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/onboarding/verify-email" {
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					key, code := r.Header.Get("Idempotency-Key"), body["verification_code"]
					if failTransport {
						failedKey, failedCode = key, code
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					if failedKey != "" && replayed == 0 {
						if key != failedKey || code != failedCode {
							t.Fatalf("request was not replayed: key=%q code=%q", key, code)
						}
						replayed++
					}
					if code != "123456" {
						signupFlowReject(w, "INVALID_VERIFICATION", "Incorrect verification code.")
					} else {
						correctRequests++
						fmt.Fprint(w, `{"data":{"onboarding_session_token":"synthetic-session"}}`)
					}
				} else {
					signupFlowSuccess(t, w, r)
				}
				return w.Result(), nil
			})
			if exit := app.Run(append(append([]string{}, signupFlowStartArgs...), "--no-input", "--output", "json")); exit != ExitOK {
				t.Fatalf("start exit=%d out=%s", exit, out)
			}
			wrongAttempts := 4
			failureCode := "123456"
			if scenario == "different code on resume" {
				wrongAttempts, failureCode = 0, "111111"
			}
			for i := 0; i < wrongAttempts; i++ {
				if exit := app.Run([]string{"signup", "--verification-code", "111111", "--no-input", "--output", "json"}); exit == ExitOK {
					t.Fatal("wrong code succeeded")
				}
			}
			out.Reset()
			failTransport = true
			if exit := app.Run([]string{"signup", "--verification-code", failureCode, "--timeout", "10ms", "--no-input", "--output", "json"}); exit != ExitNetwork {
				t.Fatalf("timeout exit=%d out=%s stderr=%s", exit, out, stderr)
			}
			pending := signupTestLoadPending(t, app)
			if pending.Attempts != wrongAttempts || failedKey == "" || pending.VerificationKey != failedKey || pending.VerificationCode != failureCode {
				t.Fatalf("unresolved attempt was not persisted: %#v", pending)
			}
			failTransport = false
			out.Reset()
			resumed := *app
			if exit := resumed.Run([]string{"signup", "--verification-code", "123456", "--no-input", "--output", "json"}); exit != ExitOK || replayed != 1 || correctRequests != 1 {
				t.Fatalf("resume exit=%d replayed=%d correct=%d out=%s stderr=%s", exit, replayed, correctRequests, out, stderr)
			}
			if signupTestLoadPending(t, app) != nil {
				t.Fatal("completed challenge remains pending")
			}
		})
	}
}

func TestSignupTwoPhasePersistsOnboardingInputs(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", override), func(t *testing.T) {
			var profileBody map[string]any
			app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/onboarding/state":
					fmt.Fprint(w, `{"data":{"can_issue_api_key":false,"next_step":{"code":"complete_profile","machine_completable":true,"submit_endpoint":"/v1/onboarding/profile","required_fields":["country","website_url","support_email","support_phone","support_url"]}}}`)
				case "/v1/onboarding/profile":
					if err := json.NewDecoder(r.Body).Decode(&profileBody); err != nil {
						t.Fatal(err)
					}
					fmt.Fprint(w, `{"data":{"can_issue_api_key":true,"default_sandbox_id":"test_new"}}`)
				default:
					signupFlowSuccess(t, w, r)
				}
			})
			args := append(append([]string{}, signupFlowStartArgs...), "--country", "ca", "--website-url", "https://example.test", "--support-email", "support@example.test", "--support-phone", "+15555550100", "--support-url", "https://example.test/support", "--requested-capability", "payments", "--requested-capability", "refunds", "--no-input", "--output", "json")
			if exit := app.Run(args); exit != ExitOK || profileBody != nil {
				t.Fatalf("start exit=%d out=%s", exit, out)
			}
			pending := signupTestLoadPending(t, app)
			if len(pending.Inputs) != 6 || len(pending.Inputs["requested-capability"]) != 2 {
				t.Fatalf("inputs not persisted: %#v", pending.Inputs)
			}
			args = []string{"signup", "--verification-code", "123456", "--no-input", "--output", "json"}
			country, website := "CA", "https://example.test"
			capabilities := []any{"payments", "refunds"}
			if override {
				args = append(args, "--country", "us", "--website-url", "https://new.example.test", "--requested-capability", "transfers")
				country, website, capabilities = "US", "https://new.example.test", []any{"transfers"}
			}
			resumed := *app
			if exit := resumed.Run(args); exit != ExitOK {
				t.Fatalf("resume exit=%d out=%s stderr=%s", exit, out, stderr)
			}
			want := map[string]any{"country": country, "requested_capabilities": capabilities, "profile": map[string]any{"website_url": website, "support_email": "support@example.test", "support_phone": "+15555550100", "support_url": "https://example.test/support"}}
			if !reflect.DeepEqual(profileBody, want) {
				t.Fatalf("profile body=%#v want=%#v", profileBody, want)
			}
		})
	}
}

func TestSignupPaymentCommandUsesResolvedProfile(t *testing.T) {
	for _, output := range []string{"human", "json"} {
		for _, profile := range []string{"default", "development", "dev team's $(touch /tmp/unused)"} {
			t.Run(output+"/"+profile, func(t *testing.T) {
				app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) { signupFlowSuccess(t, w, r) })
				if err := app.updateConfig(func(cfg *Config) error {
					cfg.DefaultProfile = profile
					cfg.Profiles[profile] = Profile{}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				args := append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input", "--output", output)
				if profile == "development" {
					args = append(args, "--profile", profile)
				}
				if exit := app.Run(args); exit != ExitOK {
					t.Fatalf("signup exit=%d out=%s stderr=%s", exit, out, stderr)
				}
				want := "FLINT_BASE_URL='https://api.example.test' " + signupNextCommand
				switch profile {
				case "development":
					want += " --profile 'development'"
				case "dev team's $(touch /tmp/unused)":
					want += " --profile 'dev team'\"'\"'s $(touch /tmp/unused)'"
				}
				if output == "json" {
					var result map[string]any
					if err := json.Unmarshal(out.Bytes(), &result); err != nil || valueAt(result, "data.next_command") != want {
						t.Fatalf("next command out=%s err=%v", out, err)
					}
				} else if !strings.Contains(out.String(), "\n  "+want+"\n") {
					t.Fatalf("human next command out=%s", out)
				}
			})
		}
	}
}

func TestSignupCanceledVerificationKeepsReplay(t *testing.T) {
	app, _, _ := signupFlowApp(t, nil)
	pending := signupTestPending()
	if e := app.replaceSignupChallenge("default", nil, pending); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app.Context = ctx
	app.HTTPClient.Transport = listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	})
	opts := defaultOptions()
	opts.Timeout = time.Second
	if _, e, _ := app.verifySignupAttempt("default", pending, "123456", opts); e == nil || e.ExitCode != ExitNetwork {
		t.Fatalf("cancellation error=%v", e)
	}
	current := signupTestLoadPending(t, app)
	if current.Attempts != 0 || current.VerificationKey == "" || current.VerificationCode != "123456" {
		t.Fatalf("cancellation lost replay: %#v", current)
	}
}
