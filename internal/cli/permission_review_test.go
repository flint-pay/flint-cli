package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionPermissionPreviewDoesNotAssumeMerchantScopes(t *testing.T) {
	for _, source := range []string{"checkout", "bearer"} {
		t.Run(source, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				t.Errorf("preview made request: %s %s", r.Method, r.URL.Path)
			}))
			defer server.Close()
			app, stdout, stderr := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_ACCESS_TOKEN", "")
			t.Setenv("FLINT_CHECKOUT_SESSION_SECRET", "")
			if source == "checkout" {
				t.Setenv("FLINT_CHECKOUT_SESSION_ID", "cs_test")
				t.Setenv("FLINT_CHECKOUT_SESSION_SECRET", "secret_test")
			} else {
				t.Setenv("FLINT_ACCESS_TOKEN", "session_test")
			}
			if exit := app.Run([]string{"api", "delete", "/v1/checkout-sessions/cs_test/delivery-selections/current", "--preview", "--output", "json"}); exit != ExitOK {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			var result map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			status, _ := lookupPath(result, "data.preview.permission_check.status")
			if status != "unknown" || requests != 0 {
				t.Fatalf("status=%v requests=%d output=%s", status, requests, stdout)
			}
		})
	}
}

func TestMissingScopeRemediationUsesCredentialSourceAndProfile(t *testing.T) {
	for _, source := range []string{"environment", "keychain"} {
		for _, output := range []string{"human", "json"} {
			t.Run(source+"/"+output, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/developer/auth-context" {
						t.Errorf("unauthorized resource request: %s", r.URL.Path)
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, limitedAuthContextJSON("sandbox"))
				}))
				defer server.Close()
				app, stdout, stderr := testApp(t, server.URL)
				t.Setenv("FLINT_ACCESS_TOKEN", "")
				t.Setenv("FLINT_CHECKOUT_SESSION_SECRET", "")
				if err := app.updateConfig(func(cfg *Config) error {
					cfg.Profiles["restricted"] = Profile{}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if source == "keychain" {
					t.Setenv("FLINT_API_KEY", "")
					app.LoadCredential = func(profile string) (string, error) {
						if profile != "restricted" {
							t.Errorf("loaded profile %q", profile)
						}
						return "flint_test_saved", nil
					}
				}
				if exit := app.Run([]string{"subscriptions", "list", "--profile", "restricted", "--output", output}); exit != ExitAuth {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
				}
				got := stdout.String() + stderr.String()
				if !strings.Contains(got, "--profile") || !strings.Contains(got, "restricted") {
					t.Errorf("missing profile guidance: %s", got)
				}
				if strings.Contains(got, "Replace FLINT_API_KEY") != (source == "environment") {
					t.Errorf("incorrect credential source guidance: %s", got)
				}
			})
		}
	}
}

func TestOAuthPermissionRemediationPreservesSelection(t *testing.T) {
	for _, profile := range []string{"default", "team account", "team'account"} {
		cmd := &Command{CanonicalName: "subscriptions.list", OperationID: "listSubscriptions"}
		err := commandPermissionError(cmd, AuthContext{AuthType: "oauth", OAuthSessionID: "session_test", ContextID: "ctx_explicit"}, profile)
		if err == nil {
			t.Fatal("missing permission error")
		}
		details := err.Details.(map[string]any)
		actions := details["remediation"].(map[string]any)["next_actions"].([]any)
		command := actions[len(actions)-1].(map[string]any)["command"].(string)
		quoted := map[string]string{"default": "default", "team account": "'team account'", "team'account": "'team'\"'\"'account'"}[profile]
		want := "flint reauth --context ctx_explicit --profile " + quoted + " --scope commerce.subscriptions.read"
		if command != want {
			t.Fatalf("command = %q, want %q", command, want)
		}
	}
}

func TestOAuthPermissionRemediationRunsForExplicitContext(t *testing.T) {
	app, out, stderr, state, stored := newContextTestApp(t)
	before, err := app.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &Command{CanonicalName: "subscriptions.list", OperationID: "listSubscriptions"}
	permissionErr := commandPermissionError(cmd, contextTestAuth("ctx_b"), "default")
	if permissionErr == nil {
		t.Fatal("missing permission error")
	}
	details := permissionErr.Details.(map[string]any)
	actions := details["remediation"].(map[string]any)["next_actions"].([]any)
	command := actions[len(actions)-1].(map[string]any)["command"].(string)
	args := append(strings.Fields(command)[1:], "--no-open", "--output", "json")
	if exit := app.Run(args); exit != ExitOK {
		t.Fatalf("remediation failed: exit=%d out=%s stderr=%s", exit, out, stderr)
	}
	if state.reauthForm.Get("context_id") != "ctx_b" || state.reauthForm.Get("scope_mode") != "additive" {
		t.Fatalf("wrong reauthorization target: %v", state.reauthForm)
	}
	credential, e := decodeOAuthCredential(stored())
	if e != nil || credential.ContextID != "ctx_b" {
		t.Fatalf("wrong credential context: %v", e)
	}
	if e := commandPermissionError(cmd, credential.Auth, "default"); e != nil {
		t.Fatalf("remediation did not grant required permission: %v", e)
	}
	after, err := app.loadConfig()
	if err != nil || after.Profiles["default"] != before.Profiles["default"] {
		t.Fatal("explicit context reauthorization changed the profile default")
	}
}

func TestLegacyOAuthPermissionRemediationPreservesEnvironment(t *testing.T) {
	for _, environment := range []string{"live", "sandbox"} {
		t.Run(environment, func(t *testing.T) {
			err := commandPermissionError(&Command{CanonicalName: "subscriptions.list", OperationID: "listSubscriptions"}, AuthContext{AuthType: "oauth", Environment: environment}, "default")
			details := err.Details.(map[string]any)
			actions := details["remediation"].(map[string]any)["next_actions"].([]any)
			command := actions[len(actions)-1].(map[string]any)["command"].(string)
			cmd, opts, help, e := parseInvocation(NewRegistry(), strings.Fields(command)[1:])
			if e != nil || validateOptions(cmd, &opts, help) != nil || cmd.CanonicalName != "auth.login" || opts.Live != (environment == "live") {
				t.Fatalf("invalid remediation for %s: %s", environment, command)
			}
		})
	}
}

func TestLegacyReauthorizationRejectsExplicitContext(t *testing.T) {
	app, out, _, state, stored := newContextTestApp(t)
	credential, e := decodeOAuthCredential(stored())
	if e != nil {
		t.Fatal(e)
	}
	credential.Version = 1
	credential.SessionID, credential.ContextID = "", ""
	credential.Auth.OAuthSessionID, credential.Auth.ContextID = "", ""
	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.StoreCredential("default", string(raw)); err != nil {
		t.Fatal(err)
	}
	if exit := app.Run([]string{"reauth", "--context", "ctx_b", "--scope", "commerce.subscriptions.read", "--no-open", "--output", "json"}); exit != ExitAuth || !strings.Contains(out.String(), "CONTEXT_SESSION_REQUIRED") {
		t.Fatalf("ignored unsupported legacy context: exit=%d out=%s", exit, out)
	}
	if state.devices != 0 || stored() != string(raw) {
		t.Fatal("invalid context request started approval or changed credentials")
	}
}
