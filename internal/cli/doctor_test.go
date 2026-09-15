package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoctorRejectedCredential(t *testing.T) {
	for _, source := range []string{"keychain", "environment"} {
		for _, output := range []string{"human", "json"} {
			t.Run(source+"/"+output, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(w, `{"error":{"code":"INVALID_API_KEY","message":"No API key matches the provided credential."}}`)
				}))
				defer server.Close()
				app, stdout, stderr := testApp(t, server.URL)
				if source == "keychain" {
					t.Setenv("FLINT_API_KEY", "")
					app.LoadCredential = func(string) (string, error) { return "flint_test_saved", nil }
				}
				if exit := app.Run([]string{"doctor", "--output", output}); exit != ExitAuth || stderr.Len() != 0 {
					t.Fatalf("exit=%d stderr=%s", exit, stderr)
				}
				got := stdout.String()
				for _, want := range []string{"API validation follows", "API rejected the credential", "No API key matches", "flint auth import --profile default"} {
					if !strings.Contains(got, want) {
						t.Errorf("missing %q in %s", want, got)
					}
				}
				if source == "environment" && !strings.Contains(got, "Replace or unset FLINT_API_KEY") {
					t.Errorf("missing environment remediation: %s", got)
				}
				if strings.Contains(got, "connectivity") || strings.Contains(got, "flint_test_") || strings.Contains(got, "map[") {
					t.Errorf("misleading or unsafe output: %s", got)
				}
				if output == "json" {
					var result struct {
						Data []map[string]any `json:"data"`
					}
					if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					last := result.Data[len(result.Data)-1]
					if last["name"] != "authentication" || last["status"] != "fail" {
						t.Fatalf("check=%v", last)
					}
				} else if !strings.Contains(got, "✗") || !strings.Contains(got, "Next:") {
					t.Errorf("missing readable failure: %s", got)
				}
			})
		}
	}
}

func TestDoctorServerFailureIsNotAuthenticationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	app, stdout, _ := testApp(t, server.URL)
	if exit := app.Run([]string{"doctor", "--output", "json"}); exit != ExitAPI {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stdout.String(), `"name":"connectivity"`) || strings.Contains(stdout.String(), "replace the saved credential") {
		t.Fatalf("unexpected diagnosis: %s", stdout)
	}
}

func TestDoctorHumanSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/openapi.json" {
			fmt.Fprint(w, `{"x-flint-api-releases":{"current_version":"2026-02-01"}}`)
			return
		}
		fmt.Fprint(w, authContextJSON("sandbox"))
	}))
	defer server.Close()
	app, stdout, _ := testApp(t, server.URL)
	if exit := app.Run([]string{"doctor", "--output", "human"}); exit != ExitOK {
		t.Fatalf("exit=%d output=%s", exit, stdout)
	}
	for _, want := range []string{"Config loaded (profile: default)", "API accepted the credential", "sandbox", "mer_123", "2026-02-01", "limited key"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("missing %q in %s", want, stdout)
		}
	}
	if strings.Contains(stdout.String(), "map[") || strings.Contains(stdout.String(), "✗") {
		t.Fatalf("unexpected output: %s", stdout)
	}
}
