package cli

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const signupSessionOutputSentinel = "synthetic-private-onboarding-bearer"

func signupVerifiedSandboxResponse(w http.ResponseWriter) {
	fmt.Fprintf(w, `{"data":{"onboarding_session_token":%q,"onboarding_session_expires_at":"2099-02-01T00:00:00Z","default_sandbox_id":"test_new","status":"complete","user":{"token":%q},"future_token":%q}}`, signupSessionOutputSentinel, signupSessionOutputSentinel, signupSessionOutputSentinel)
}

func TestSignupNewServerOutputExcludesSessionBearer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flags    []string
		wantExit int
	}{
		{"json", []string{"--output", "json"}, ExitOK},
		{"human", []string{"--output", "human"}, ExitOK},
		{"json_jq", []string{"--output", "json", "--jq", ".data.onboarding"}, ExitOK},
		{"json_field", []string{"--output", "json", "--field", "data.onboarding.default_sandbox_id"}, ExitOK},
		{"json_select", []string{"--output", "json", "--select", "onboarding"}, ExitOK},
		{"human_field", []string{"--output", "human", "--field", "data.onboarding.default_sandbox_id"}, ExitOK},
		{"human_select", []string{"--output", "human", "--select", "onboarding"}, ExitOK},
		{"missing_secret_jq", []string{"--output", "json", "--jq", ".data.onboarding.onboarding_session_token"}, ExitOK},
		{"missing_secret_field", []string{"--output", "json", "--field", "data.onboarding.onboarding_session_token"}, ExitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/onboarding/verify-email" {
					signupVerifiedSandboxResponse(w)
					return
				}
				if r.URL.Path == "/v1/sandboxes/test_new/api-keys" && r.Header.Get("Authorization") != "Bearer "+signupSessionOutputSentinel {
					t.Error("signup did not retain its private bearer")
				}
				signupFlowSuccess(t, w, r)
			})
			var stored string
			app.StoreCredential = func(_ string, key string) error { stored = key; return nil }
			args := append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input", "--debug")
			exit := app.Run(append(args, tc.flags...))
			if strings.Contains(out.String()+stderr.String(), signupSessionOutputSentinel) || strings.Contains(out.String()+stderr.String(), "flint_test_synthetic_new") {
				t.Fatal("signup exposed a private credential")
			}
			if exit != tc.wantExit {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
			}
			if stored != "flint_test_synthetic_new" || signupTestLoadPending(t, app) != nil {
				t.Fatal("output filtering changed credential save or progress cleanup")
			}
			if tc.name == "json" {
				if !strings.Contains(out.String(), `"onboarding":{"default_sandbox_id":"test_new","status":"complete"}`) || strings.Contains(out.String(), "onboarding_session_token") || strings.Contains(out.String(), "future_token") {
					t.Fatalf("unsafe onboarding summary: %s", out)
				}
			}
		})
	}
}

func TestSignupNewServerFailedMintResumesWithoutVerification(t *testing.T) {
	verifies, mints, states := 0, 0, 0
	app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/onboarding/verify-email":
			verifies++
			signupVerifiedSandboxResponse(w)
		case "/v1/onboarding/state":
			states++
			fmt.Fprint(w, `{"data":{"default_sandbox_id":"test_new","status":"complete"}}`)
		case "/v1/sandboxes/test_new/api-keys":
			mints++
			if r.Header.Get("Authorization") != "Bearer "+signupSessionOutputSentinel {
				t.Error("resume lost private bearer")
			}
			if mints == 1 {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":{"code":"FORBIDDEN","message":"temporarily refused"}}`)
				return
			}
			signupFlowSuccess(t, w, r)
		default:
			signupFlowSuccess(t, w, r)
		}
	})
	args := append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--no-input", "--output", "json")
	if exit := app.Run(args); exit == ExitOK || signupTestLoadPending(t, app) == nil {
		t.Fatal("failed mint lost resumable progress")
	}
	out.Reset()
	stderr.Reset()
	if exit := app.Run([]string{"signup", "--verification-code", "123456", "--no-input", "--output", "json"}); exit != ExitOK {
		t.Fatalf("resume exit=%d output=%s", exit, out)
	}
	if verifies != 1 || mints != 2 || states != 1 || signupTestLoadPending(t, app) != nil {
		t.Fatalf("verifies=%d mints=%d states=%d", verifies, mints, states)
	}
	if strings.Contains(out.String()+stderr.String(), signupSessionOutputSentinel) {
		t.Fatal("resume exposed private bearer")
	}
}
