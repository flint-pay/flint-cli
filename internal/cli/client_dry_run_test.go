package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Every case rejects even attempted HTTP calls, including credential introspection
// and workload subject-token discovery, before a request can reach a server.
func freshClientDryRunApp(t *testing.T) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	app, out, stderr := testApp(t, "http://127.0.0.1:1")
	for _, name := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_WORKLOAD_IDENTITY_ID", "FLINT_PROFILE", "FLINT_MERCHANT", "FLINT_CREDENTIAL_STORE"} {
		t.Setenv(name, "")
	}
	path, err := app.configPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	app.LoadCredential = func(string) (string, error) {
		t.Error("fresh profile queried unavailable keyring")
		return "", errors.New("keyring unavailable")
	}
	app.StoreCredential = func(string, string) error { t.Error("dry run stored a credential"); return nil }
	app.DeleteCredential = func(string) error { t.Error("dry run deleted a credential"); return nil }
	var requests atomic.Int32
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("client dry run attempted HTTP")
	})}
	previousFederationClient := federationHTTPClient
	federationHTTPClient = app.HTTPClient
	t.Cleanup(func() {
		federationHTTPClient = previousFederationClient
		if count := requests.Load(); count != 0 {
			t.Errorf("client dry run attempted %d HTTP requests", count)
		}
	})
	return app, out, stderr
}

func TestClientDryRunWithoutCredentialSkipsUnverifiedGuards(t *testing.T) {
	for _, live := range []bool{false, true} {
		for _, guards := range []string{"none", "merchant", "sandbox", "both"} {
			t.Run(fmt.Sprintf("live=%t/guards=%s", live, guards), func(t *testing.T) {
				app, out, stderr := freshClientDryRunApp(t)
				args := []string{"customers", "create", "--input", `{"email":"offline@example.test"}`, "--dry-run=client"}
				if live {
					args = append(args, "--live")
				}
				if guards == "merchant" || guards == "both" {
					args = append(args, "--merchant", "mer_guard")
				}
				if guards == "sandbox" || guards == "both" {
					dir := filepath.Join(app.WorkingDir, ".flint")
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"sandbox":"test_guard"}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				exit, result := runJSON(t, app, out, args...)
				environment := "sandbox"
				if live {
					environment = "live"
				}
				if exit != ExitOK || lookupPathValue(result, "data.method") != "POST" || lookupPathValue(result, "data.path") != "/v1/customers" || lookupPathValue(result, "data.body.email") != "offline@example.test" || lookupPathValue(result, "data.environment") != environment || lookupPathValue(result, "data.persistent_side_effects") != false {
					t.Fatalf("exit=%d result=%v stderr=%s", exit, result, stderr)
				}
				warnings, _ := lookupPathValue(result, "data.warnings").([]any)
				if len(warnings) != 1 {
					t.Fatalf("missing skipped-check warning: %v", result)
				}
				warning := warnings[0].(map[string]any)
				if warning["message"] != unauthenticatedDryRunWarning || fmt.Sprint(warning["skipped_checks"]) != "[scopes merchant_guard sandbox_guard]" || !strings.Contains(stderr.String(), unauthenticatedDryRunWarning) {
					t.Fatalf("warning=%v stderr=%s", warning, stderr)
				}
				if strings.Contains(out.String(), "mer_guard") || strings.Contains(out.String(), "test_guard") {
					t.Fatalf("guard presented as verified context: %s", out)
				}
			})
		}
	}
}

func TestClientDryRunWithoutCredentialPreviewIsUnverified(t *testing.T) {
	app, out, stderr := freshClientDryRunApp(t)
	// Stale profile metadata must not choose the environment or satisfy guards.
	p := profileWithoutCredential()
	p.Environment, p.MerchantID, p.SandboxID = "live", "mer_stale", "test_stale"
	p.MerchantGuard = "mer_guard"
	if err := app.saveConfig(Config{DefaultProfile: "default", Profiles: map[string]Profile{"default": p}}); err != nil {
		t.Fatal(err)
	}
	for _, live := range []bool{false, true} {
		args := []string{"webhook-endpoints", "delete", "whep_offline", "--dry-run=client", "--preview"}
		if live {
			args = append(args, "--live")
		}
		exit, result := runJSON(t, app, out, args...)
		if exit != ExitOK || lookupPathValue(result, "data.preview.permission_check.status") != "unknown" || !strings.Contains(fmt.Sprint(lookupPathValue(result, "data.preview.permission_check.reason")), "skipped") {
			t.Fatalf("exit=%d result=%v stderr=%s", exit, result, stderr)
		}
		environment := "sandbox"
		if live {
			environment = "live"
		}
		if lookupPathValue(result, "data.environment") != environment || strings.Contains(out.String(), "mer_stale") || strings.Contains(out.String(), "test_stale") {
			t.Fatalf("stale profile metadata used: %s", out)
		}
	}
}

func TestClientDryRunRequiredKeyRequestNeverExchangesTokensOrExecutes(t *testing.T) {
	for _, credential := range []string{"absent", "workload_identity"} {
		t.Run(credential, func(t *testing.T) {
			app, out, stderr := freshClientDryRunApp(t)
			if credential == "workload_identity" {
				t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "wid_synthetic")
				t.Setenv("FLINT_OIDC_SOURCE", "github_actions")
				t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "http://127.0.0.1:1/subject-token")
				t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "synthetic")
			}
			exit, result := runJSON(t, app, out, "refunds", "create", "--payment-intent-id", "pi_offline", "--refund-type", "full", "--idempotency-key", "offline-required-key", "--dry-run=client")
			if exit != ExitOK || lookupPathValue(result, "data.path") != "/v1/refunds" || lookupPathValue(result, "data.headers.Idempotency-Key") != "offline-required-key" || lookupPathValue(result, "data.body.payment_intent_id") != "pi_offline" || lookupPathValue(result, "data.body.refund_type") != "full" {
				t.Fatalf("exit=%d result=%v stderr=%s", exit, result, stderr)
			}
		})
	}
}

func TestClientDryRunValidationNeverSuggestsReusingAnUnsentKey(t *testing.T) {
	for _, credential := range []string{"absent", "api_key", "workload_identity"} {
		for _, output := range []string{"human", "json"} {
			t.Run(credential+"/"+output, func(t *testing.T) {
				app, out, stderr := freshClientDryRunApp(t)
				switch credential {
				case "api_key":
					t.Setenv("FLINT_API_KEY", "flint_test_synthetic")
				case "workload_identity":
					t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "wid_synthetic")
					t.Setenv("FLINT_OIDC_SOURCE", "github_actions")
					t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "http://127.0.0.1:1/subject-token")
					t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "synthetic")
				}
				args := []string{"refunds", "create", "--input", `{"refund_type":42}`, "--idempotency-key", "offline-required-key", "--dry-run=client", "--output", output}
				if exit := app.Run(args); exit != ExitUsage {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
				}
				text := out.String() + stderr.String()
				if (output == "json" && !strings.Contains(text, "INVALID_REQUEST_BODY")) || !strings.Contains(text, "refund_type") || strings.Contains(text, "offline-required-key") || strings.Contains(text, "reuse --idempotency-key") || strings.Contains(text, "Idempotency key:") || strings.Contains(text, "To retry") {
					t.Fatal(text)
				}
			})
		}
	}
}

func TestClientDryRunRetainsConfiguredCredentialErrorsAndGuards(t *testing.T) {
	for _, tc := range []struct {
		name, key, code string
		live            bool
		merchant        string
		sandbox         bool
	}{
		{name: "unavailable keyring", code: "CREDENTIAL_LOOKUP_FAILED"},
		{name: "invalid credential", key: "synthetic_invalid", code: "CREDENTIAL_FORMAT_INVALID"},
		{name: "live acknowledgement", key: "flint_live_synthetic", code: "LIVE_ACKNOWLEDGEMENT_REQUIRED"},
		{name: "sandbox with live flag", key: "flint_test_synthetic", live: true, code: "CREDENTIAL_MODE_MISMATCH"},
		{name: "merchant mismatch", key: "flint_test_synthetic", merchant: "mer_other", code: "MERCHANT_GUARD_MISMATCH"},
		{name: "sandbox mismatch", key: "flint_test_synthetic", sandbox: true, code: "SANDBOX_GUARD_MISMATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, out, stderr := freshClientDryRunApp(t)
			present := true
			p := Profile{CredentialPresent: &present, CredentialStore: "keyring", MerchantID: "mer_actual"}
			if err := app.saveConfig(Config{DefaultProfile: "default", Profiles: map[string]Profile{"default": p}}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			app.LoadCredential = func(string) (string, error) {
				calls++
				if tc.key == "" {
					return "", errors.New("keyring unavailable")
				}
				return tc.key, nil
			}
			args := []string{"customers", "create", "--name", "Offline", "--dry-run=client"}
			if tc.live {
				args = append(args, "--live")
			}
			if tc.merchant != "" {
				args = append(args, "--merchant", tc.merchant)
			}
			if tc.sandbox {
				dir := filepath.Join(app.WorkingDir, ".flint")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"sandbox":"test_guard"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			exit, result := runJSON(t, app, out, args...)
			if exit == ExitOK || lookupPathValue(result, "error.code") != tc.code || calls != 1 || strings.Contains(stderr.String(), unauthenticatedDryRunWarning) {
				t.Fatalf("exit=%d result=%v lookups=%d stderr=%s", exit, result, calls, stderr)
			}
		})
	}
}

func TestClientDryRunWithoutCredentialDoesNotAuthorizeExecution(t *testing.T) {
	app, out, stderr := freshClientDryRunApp(t)
	exit, result := runJSON(t, app, out, "customers", "create", "--name", "Offline", "--dry-run=client", "--quiet")
	if exit != ExitOK || stderr.Len() != 0 || lookupPathValue(result, "data.warnings") == nil {
		t.Fatalf("exit=%d result=%v stderr=%s", exit, result, stderr)
	}
	exit, result = runJSON(t, app, out, "customers", "create", "--name", "Offline", "--confirm")
	if exit != ExitAuth || lookupPathValue(result, "error.code") != "API_KEY_REQUIRED" {
		t.Fatalf("dry-run context authorized execution: exit=%d result=%v", exit, result)
	}
}
