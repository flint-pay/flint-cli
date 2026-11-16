package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestSignupTimezone(t *testing.T) {
	for _, tc := range []struct {
		name, detected, override, want string
		invalid                        bool
	}{
		{name: "detected", detected: "America/Chicago", want: "America/Chicago"},
		{name: "override", detected: "America/Chicago", override: "Asia/Kathmandu", want: "Asia/Kathmandu"},
		{name: "unavailable", detected: "not-a-zone"},
		{name: "invalid override", detected: "America/Chicago", override: "not-a-zone", invalid: true},
		{name: "local override", detected: "America/Chicago", override: "Local", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TZ", tc.detected)
			requests := 0
			app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path == "/v1/onboarding/start" {
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					zone, present := payload["timezone"]
					if (tc.want != "" && zone != tc.want) || (tc.want == "" && present) {
						t.Errorf("timezone = %#v, present %v; want %q", zone, present, tc.want)
					}
				}
				if r.URL.Path == "/v1/onboarding/verify-email" {
					fmt.Fprintf(w, `{"data":{"onboarding_session_token":"synthetic-session","default_sandbox_id":"test_new","merchant":{"timezone":%q}}}`, tc.want)
					return
				}
				signupFlowSuccess(t, w, r)
			})
			app.StoreCredential = func(string, string) error { return nil }
			args := append(append([]string{}, signupFlowStartArgs...), "--no-input", "--output", "json")
			if tc.override != "" {
				args = append(args, "--timezone", tc.override)
			}
			exit := app.Run(args)
			if tc.invalid {
				if exit != ExitUsage || requests != 0 || !strings.Contains(out.String(), "INVALID_TIMEZONE") {
					t.Fatalf("exit=%d requests=%d output=%s %s", exit, requests, out, stderr)
				}
				return
			}
			if exit != ExitOK {
				t.Fatalf("exit=%d: %s %s", exit, out, stderr)
			}
			pending := signupTestLoadPending(t, app)
			if tc.want != "" {
				if pending.Inputs["timezone"][0] != tc.want || !strings.Contains(out.String(), tc.want) {
					t.Fatalf("timezone missing: %#v %s", pending.Inputs, out)
				}
			}
			// Resume preserves the initial choice even if the machine's zone changes.
			t.Setenv("TZ", "Europe/London")
			out.Reset()
			if tc.want != "" {
				if exit := app.Run([]string{"signup", "--verification-code", "123456", "--timezone", "Europe/London", "--no-input", "--output", "json"}); exit != ExitUsage || !strings.Contains(out.String(), "SIGNUP_INPUT_MISMATCH") {
					t.Fatalf("changed zone accepted: %d %s", exit, out)
				}
			}
			out.Reset()
			if exit := app.Run([]string{"signup", "--verification-code", "123456", "--no-input"}); exit != ExitOK {
				t.Fatalf("resume=%d %s %s", exit, out, stderr)
			}
			if tc.want != "" && !strings.Contains(out.String(), "Business time zone: "+tc.want) {
				t.Fatalf("confirmation missing: %s", out)
			}
		})
	}
}

func TestSignupTimezoneValidation(t *testing.T) {
	for _, zone := range []string{"", "Local", "+06:00", "Mars/Olympus", "/etc/localtime"} {
		if validSignupTimezone(zone) {
			t.Errorf("accepted %q", zone)
		}
	}
	for _, zone := range []string{"UTC", "America/Chicago", "Asia/Kathmandu"} {
		if !validSignupTimezone(zone) {
			t.Errorf("rejected %q", zone)
		}
	}
	t.Setenv("TZ", "")
	if got := localSignupTimezone(); got != "UTC" {
		t.Errorf("empty TZ = %q", got)
	}
	t.Setenv("TZ", ":America/Chicago")
	if got := localSignupTimezone(); got != "America/Chicago" {
		t.Errorf("colon TZ = %q", got)
	}
}
