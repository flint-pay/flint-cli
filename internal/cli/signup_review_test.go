package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSignupReviewFifthSuccessResumesSession(t *testing.T) {
	verifies := 0
	failState := true
	app, out, stderr := signupFlowApp(t, nil)
	app.HTTPClient.Transport = listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/onboarding/state" && failState {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return signupReviewResponse(t, r, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/onboarding/verify-email" {
				verifies++
				if verifies < 5 {
					signupFlowReject(w, "INVALID_VERIFICATION", "Incorrect verification code.")
					return
				}
				fmt.Fprint(w, `{"data":{"onboarding_session_token":"synthetic-session","onboarding_session_expires_at":"2099-02-01T00:00:00Z"}}`)
				return
			}
			if r.URL.Path == "/v1/onboarding/state" && r.Header.Get("Authorization") != "Bearer synthetic-session" {
				t.Fatal("saved session not used")
			}
			signupFlowSuccess(t, w, r)
		}), nil
	})
	if exit := app.Run(append(append([]string{}, signupFlowStartArgs...), "--no-input", "--output", "json")); exit != ExitOK {
		t.Fatalf("start=%d %s", exit, out)
	}
	for i := 0; i < 4; i++ {
		if exit := app.Run([]string{"signup", "--verification-code", "111111", "--no-input", "--output", "json"}); exit == ExitOK {
			t.Fatal("wrong code accepted")
		}
	}
	out.Reset()
	if exit := app.Run([]string{"signup", "--verification-code", "123456", "--timeout", "10ms", "--no-input", "--output", "json"}); exit != ExitNetwork {
		t.Fatalf("state failure=%d %s %s", exit, out, stderr)
	}
	pending := signupTestLoadPending(t, app)
	if pending.Attempts != 5 || pending.SessionToken != "synthetic-session" || !pending.SessionExpiresAt.Equal(time.Date(2099, 2, 1, 0, 0, 0, 0, time.UTC)) || pending.VerificationKey != "" {
		t.Fatalf("session not saved: %#v", pending)
	}
	failState = false
	out.Reset()
	resumed := *app
	if exit := resumed.Run([]string{"signup", "--verification-code", "123456", "--no-input", "--output", "json"}); exit != ExitOK || verifies != 5 || signupTestLoadPending(t, app) != nil {
		t.Fatalf("resume=%d verifies=%d out=%s stderr=%s", exit, verifies, out, stderr)
	}
}

func TestSignupReviewGenericValidationCanBeCorrected(t *testing.T) {
	var codes, keys []string
	app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/onboarding/verify-email" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			codes = append(codes, body["verification_code"])
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			if len(codes) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":{"code":"INVALID_REQUEST","message":"Validation failed."}}`)
				return
			}
			fmt.Fprint(w, `{"data":{"onboarding_session_token":"synthetic-session"}}`)
			return
		}
		signupFlowSuccess(t, w, r)
	})
	app.Run(append(append([]string{}, signupFlowStartArgs...), "--no-input", "--output", "json"))
	if exit := app.Run([]string{"signup", "--verification-code", "malformed", "--no-input", "--output", "json"}); exit != ExitAPI {
		t.Fatalf("validation=%d %s", exit, out)
	}
	pending := signupTestLoadPending(t, app)
	if pending.VerificationKey != "" || pending.VerificationCode != "" || pending.Attempts != 0 {
		t.Fatalf("validation retained replay or consumed attempt: %#v", pending)
	}
	if exit := app.Run([]string{"signup", "--verification-code", "123456", "--no-input", "--output", "json"}); exit != ExitOK || len(codes) != 2 || codes[1] != "123456" || keys[0] == keys[1] {
		t.Fatalf("corrected=%d codes=%v keys=%v %s", exit, codes, keys, out)
	}
}

func TestSignupReviewExplicitDefaultCommands(t *testing.T) {
	for _, output := range []string{"human", "json"} {
		t.Run(output, func(t *testing.T) {
			app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) { signupFlowSuccess(t, w, r) })
			if err := app.updateConfig(func(cfg *Config) error { cfg.DefaultProfile = "work"; cfg.Profiles["work"] = Profile{}; return nil }); err != nil {
				t.Fatal(err)
			}
			if exit := app.Run(append(append([]string{}, signupFlowStartArgs...), "--profile", "default", "--no-input", "--output", output)); exit != ExitOK || !strings.Contains(out.String(), "--profile 'default'") {
				t.Fatalf("continuation=%d %s", exit, out)
			}
			out.Reset()
			if exit := app.Run([]string{"signup", "--verification-code", "123456", "--profile", "default", "--no-input", "--output", output}); exit != ExitOK || !strings.Contains(out.String(), "--profile 'default'") {
				t.Fatalf("payment=%d %s", exit, out)
			}
		})
	}
}

func TestSignupReviewResumedOriginInstructions(t *testing.T) {
	payments := 0
	app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "api.example.test" {
			t.Fatalf("wrong origin: %s", r.URL)
		}
		if r.URL.Path == "/v1/payment-links" {
			payments++
			if r.Header.Get("Authorization") != "Bearer flint_test_synthetic_new" {
				t.Fatal("saved key not used")
			}
			fmt.Fprint(w, `{"data":{"url":"https://pay.example.test/test"}}`)
			return
		}
		signupFlowSuccess(t, w, r)
	})
	stored := ""
	app.StoreCredential = func(_ string, key string) error { stored = key; return nil }
	app.LoadCredential = func(string) (string, error) { return stored, nil }
	if exit := app.Run(append(append([]string{}, signupFlowStartArgs...), "--no-input", "--output", "json")); exit != ExitOK || !strings.Contains(out.String(), "FLINT_BASE_URL='https://api.example.test'") {
		t.Fatalf("start=%d %s", exit, out)
	}
	t.Setenv("FLINT_BASE_URL", "")
	out.Reset()
	if exit := app.Run([]string{"signup", "--verification-code", "123456", "--no-input", "--output", "json"}); exit != ExitOK {
		t.Fatalf("resume=%d %s", exit, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	command, _ := valueAt(result, "data.next_command").(string)
	prefix := "FLINT_BASE_URL='https://api.example.test' "
	if command != prefix+signupNextCommand {
		t.Fatalf("missing required origin: %s", command)
	}
	// Apply the environment assignment in the generated command, then use its key.
	t.Setenv("FLINT_BASE_URL", "https://api.example.test")
	out.Reset()
	if exit := app.Run([]string{"payment-links", "create", "--name", "Test payment", "--item-name", "Test payment", "--amount", "1000", "--currency", "USD", "--no-input", "--output", "json"}); exit != ExitOK || payments != 1 {
		t.Fatalf("payment=%d requests=%d %s", exit, payments, out)
	}
}

func TestSignupReviewProjectContextStopsBeforeProvisioning(t *testing.T) {
	app, out, _ := signupFlowApp(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("signup provisioned before detecting project context")
	})
	dir := filepath.Join(app.WorkingDir, ".flint")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"context":"ctx_project"}`), 0600); err != nil {
		t.Fatal(err)
	}
	app.StoreCredential = func(string, string) error { t.Fatal("credential replaced"); return nil }
	// Environment credentials suppress context resolution, but the project pin
	// will return after that override is removed and must still block signup.
	t.Setenv("FLINT_API_KEY", "flint_test_synthetic_override")
	exit := app.Run(append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input", "--output", "json"))
	if exit == ExitOK || !strings.Contains(out.String(), "SIGNUP_PROJECT_CONTEXT") || !strings.Contains(out.String(), "outside the project") || !strings.Contains(out.String(), "remove the pinned context") || signupTestLoadPending(t, app) != nil {
		t.Fatalf("preflight=%d %s", exit, out)
	}
}

func signupReviewResponse(t *testing.T, r *http.Request, handler http.HandlerFunc) *http.Response {
	t.Helper()
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "application/json")
	handler(w, r)
	return w.Result()
}

func TestSignupReviewExpiredSessionStopsBeforeRequest(t *testing.T) {
	app, out, _ := signupFlowApp(t, func(http.ResponseWriter, *http.Request) { t.Fatal("expired session sent a request") })
	pending := signupTestPending()
	pending.Attempts = 5
	pending.SessionToken = "synthetic-session"
	pending.SessionExpiresAt = time.Now().Add(-time.Minute)
	if e := app.replaceSignupChallenge("default", nil, pending); e != nil {
		t.Fatal(e)
	}
	if exit := app.Run([]string{"signup", "--verification-code", "123456", "--no-input", "--output", "json"}); exit == ExitOK || !strings.Contains(out.String(), "SIGNUP_SESSION_EXPIRED") || strings.Contains(out.String(), "synthetic-session") {
		t.Fatalf("expired=%d %s", exit, out)
	}
}
