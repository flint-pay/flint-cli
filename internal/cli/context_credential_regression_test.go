package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnvironmentCredentialOverridesConfiguredContext(t *testing.T) {
	for _, variable := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN"} {
		for _, project := range []bool{false, true} {
			for _, command := range [][]string{{"auth", "status"}, {"config", "validate"}, {"doctor"}} {
				t.Run(fmt.Sprintf("%s/project=%t/%s", variable, project, strings.Join(command, "-")), func(t *testing.T) {
					token := "override-access"
					if variable == "FLINT_API_KEY" {
						token = "flint_test_override"
					}
					var requests atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/v1/developer/auth-context":
							requests.Add(1)
							if r.Header.Get("Authorization") != "Bearer "+token {
								t.Error("did not use environment credential")
							}
							fmt.Fprint(w, `{"data":{"auth_type":"api_key","api_key_id":"key_override","environment":"sandbox","merchant_id":"mer_override","sandbox_id":"test_override","scopes":["payments.payment_intents.read"]}}`)
						case "/v1/openapi.json":
							fmt.Fprint(w, `{"x-flint-api-releases":{"current_version":"2026-02-01"}}`)
						default:
							t.Errorf("unexpected request %s", r.URL.Path)
							http.NotFound(w, r)
						}
					}))
					defer server.Close()
					app, out, stderr := testApp(t, server.URL)
					t.Setenv("FLINT_API_KEY", "")
					t.Setenv("FLINT_ACCESS_TOKEN", "")
					t.Setenv(variable, token)
					stored := installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(time.Hour)))
					before := stored()
					profile := Profile{ContextID: "ctx_saved"}
					if err := app.updateConfig(func(cfg *Config) error { cfg.Profiles["default"] = profile; return nil }); err != nil {
						t.Fatal(err)
					}
					if project {
						dir := filepath.Join(app.WorkingDir, ".flint")
						if err := os.MkdirAll(dir, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"context":"ctx_project"}`), 0600); err != nil {
							t.Fatal(err)
						}
					}
					argv := append(append([]string{}, command...), "--output", "json")
					if exit := app.Run(argv); exit != ExitOK {
						t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
					}
					if requests.Load() != 1 {
						t.Fatalf("expected override validation, got %d requests", requests.Load())
					}
					out.Reset()
					stderr.Reset()
					// Explicit context selection must still fail before sending a request.
					if exit := app.Run(append(argv, "--context", "ctx_explicit")); exit != ExitAuth {
						t.Fatalf("explicit context accepted: exit=%d out=%s", exit, out)
					}
					if requests.Load() != 1 {
						t.Fatal("explicit context reached API with environment credential")
					}
					out.Reset()
					stderr.Reset()
					// The override must not bypass independent merchant guards.
					if exit := app.Run(append(argv, "--merchant", "mer_other")); exit != ExitAuth {
						t.Fatalf("merchant guard bypassed: exit=%d out=%s", exit, out)
					}
					cfg, err := app.loadConfig()
					if err != nil {
						t.Fatal(err)
					}
					if stored() != before || cfg.Profiles["default"] != profile {
						t.Fatal("override changed the saved browser session or context")
					}
				})
			}
		}
	}
}

func TestLegacyLoginRecoversRejectedCachedToken(t *testing.T) {
	for _, scenario := range []string{"revoked", "active", "outage", "no-input"} {
		t.Run(scenario, func(t *testing.T) {
			app, out, _, state, stored := newContextTestApp(t)
			c, e := decodeOAuthCredential(stored())
			if e != nil {
				t.Fatal(e)
			}
			c.Version = 1
			c.SessionID = ""
			c.ContextID = ""
			c.Auth.OAuthSessionID = ""
			c.Auth.ContextID = ""
			raw, _ := json.Marshal(c)
			if err := app.StoreCredential("default", string(raw)); err != nil {
				t.Fatal(err)
			}
			if err := app.updateConfig(func(cfg *Config) error { cfg.Profiles["default"] = Profile{}; return nil }); err != nil {
				t.Fatal(err)
			}
			before := stored()
			state.revokedOriginal = scenario == "revoked" || scenario == "no-input"
			state.outage = scenario == "outage"
			argv := []string{"login", "--no-open", "--output", "json", "--timeout", "3s"}
			if scenario == "no-input" {
				argv = append(argv, "--no-input")
			}
			exit := app.Run(argv)
			if scenario == "revoked" {
				if exit != ExitOK || state.devices != 1 {
					t.Fatalf("login did not recover: exit=%d devices=%d out=%s", exit, state.devices, out)
				}
				next, e := decodeOAuthCredential(stored())
				if e != nil || next.RefreshToken == c.RefreshToken {
					t.Fatalf("replacement session not saved: %v", e)
				}
			} else {
				if state.devices != 0 || stored() != before {
					t.Fatal("started approval or replaced session without an authoritative rejection and approval intent")
				}
				if scenario == "active" && exit != ExitOK {
					t.Fatalf("valid login not reused: exit=%d out=%s", exit, out)
				}
				if scenario != "active" && exit == ExitOK {
					t.Fatalf("expected failure: out=%s", out)
				}
			}
		})
	}
}
