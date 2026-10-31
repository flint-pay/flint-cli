package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIdentityCLIInfoAcrossCommands(t *testing.T) {
	for _, argv := range [][]string{{"whoami"}, {"auth", "status"}, {"doctor"}, {"config", "get"}} {
		for _, output := range []string{"json", "human"} {
			t.Run(strings.Join(argv, "_")+"/"+output, func(t *testing.T) {
				calls := 0
				app, out, stderr := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					switch r.URL.Path {
					case "/v1/auth-context":
						fmt.Fprint(w, strings.Replace(authContextJSON("sandbox"), `"environment":"sandbox"`, `"mode":"test","access":"full"`, 1))
					case "/v1/openapi.json":
						w.Header().Set("Flint-Version", "2026-02-01")
					default:
						t.Fatalf("unexpected request %s", r.URL.Path)
					}
				})
				t.Setenv("FLINT_BASE_URL", "http://127.0.0.1:9")
				t.Setenv("FLINT_API_KEY", "flint_test_identity_synthetic")
				if err := app.updateConfig(func(cfg *Config) error { cfg.Profiles["qa"] = profileWithoutCredential(); return nil }); err != nil {
					t.Fatal(err)
				}
				args := append(append([]string{}, argv...), "--profile", "qa", "--output", output)
				exit := app.Run(args)
				if exit != ExitOK {
					t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
				}
				if strings.Contains(out.String()+stderr.String(), "flint_test_identity_synthetic") {
					t.Fatal("credential exposed")
				}
				if output == "json" {
					var value map[string]any
					if err := json.Unmarshal(out.Bytes(), &value); err != nil {
						t.Fatal(err)
					}
					for name, want := range map[string]string{"api_base_url": "http://127.0.0.1:9", "credential_source": "environment_api_key", "profile": "qa", "profile_source": "flag"} {
						if got := lookupPathValue(value, "data.cli."+name); got != want {
							t.Fatalf("%s=%v want=%s result=%v", name, got, want, value)
						}
					}
					if argv[0] == "doctor" {
						if _, ok := lookupPathValue(value, "data.checks").([]any); !ok {
							t.Fatal("doctor checks missing", value)
						}
					}
				} else {
					for _, want := range []string{"http://127.0.0.1:9", "environment_api_key", "qa"} {
						if !strings.Contains(out.String(), want) {
							t.Fatalf("missing %s: %s", want, out)
						}
					}
				}
				if argv[0] == "config" && calls != 0 {
					t.Fatal("config get made API requests")
				}
			})
		}
	}
}

func TestIdentityConfigCredentialSources(t *testing.T) {
	for _, source := range []string{"none", "keychain", "file", "oauth_session", "environment_access_token", "environment_api_key", "workload_identity", "unavailable"} {
		t.Run(source, func(t *testing.T) {
			app, out, _ := testApp(t, "http://127.0.0.1:9")
			t.Setenv("FLINT_API_KEY", "")
			app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("config get sent a request"); return nil, nil })}
			switch source {
			case "environment_access_token":
				t.Setenv("FLINT_ACCESS_TOKEN", "synthetic-access")
				t.Setenv("FLINT_API_KEY", "flint_test_synthetic")
			case "environment_api_key":
				t.Setenv("FLINT_API_KEY", "flint_test_synthetic")
			case "workload_identity":
				t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "wid_synthetic")
			case "keychain", "unavailable":
				app.LoadCredential = func(string) (string, error) {
					if source == "unavailable" {
						return "", fmt.Errorf("locked")
					}
					return "flint_test_synthetic", nil
				}
			case "file":
				app.credentialStore = "file"
				if e := app.saveAuthenticatedCredential("default", "flint_test_synthetic", AuthContext{Environment: "sandbox"}); e != nil {
					t.Fatal(e)
				}
				app.credentialStore = ""
			case "oauth_session":
				t.Setenv("FLINT_BASE_URL", "")
				installTestOAuth(t, app, testOAuthCredential(t, "http://127.0.0.1:9", time.Now().Add(time.Hour)))
			}
			exit, result := runJSON(t, app, out, "config", "get")
			if exit != ExitOK || lookupPathValue(result, "data.cli.credential_source") != source {
				t.Fatalf("exit=%d result=%v", exit, result)
			}
			if lookupPathValue(result, "data.cli.api_base_url") != "http://127.0.0.1:9" {
				t.Fatal(result)
			}
		})
	}
}

func TestIdentityAuthStatusCLIProjection(t *testing.T) {
	app, out, _ := signupFlowApp(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, authContextJSON("sandbox")) })
	t.Setenv("FLINT_BASE_URL", "http://127.0.0.1:9")
	t.Setenv("FLINT_API_KEY", "flint_test_synthetic")
	for _, args := range [][]string{{"--field", "cli.api_base_url"}, {"--select", "cli.api_base_url"}} {
		out.Reset()
		exit := app.Run(append([]string{"whoami", "--output", "json"}, args...))
		if exit != ExitOK || !strings.Contains(out.String(), "http://127.0.0.1:9") {
			t.Fatalf("exit=%d output=%s", exit, out)
		}
	}
}

func TestIdentityRootHelpListsColorEnvironment(t *testing.T) {
	app, out, _ := testApp(t, "http://127.0.0.1:9")
	if exit := app.Run([]string{"--help"}); exit != ExitOK || !strings.Contains(out.String(), "FLINT_COLOR") {
		t.Fatal(out)
	}
}

func TestIdentityDoctorRetainsSavedOAuthServerOnFailure(t *testing.T) {
	const savedBaseURL = "http://127.0.0.1:18990"
	for _, failure := range []string{"rejected", "transport"} {
		for _, output := range []string{"json", "human"} {
			t.Run(failure+"/"+output, func(t *testing.T) {
				app, out, stderr := testApp(t, savedBaseURL)
				t.Setenv("FLINT_API_KEY", "")
				t.Setenv("FLINT_BASE_URL", "")
				installTestOAuth(t, app, testOAuthCredential(t, savedBaseURL, time.Now().Add(time.Hour)))
				authCalls, discoveryCalls := 0, 0
				app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Scheme+"://"+r.URL.Host != savedBaseURL {
						t.Errorf("attempted a request outside the saved loopback server: %s", r.URL.Host)
						return nil, fmt.Errorf("unexpected server")
					}
					w := httptest.NewRecorder()
					switch r.URL.Path {
					case "/v1/auth-context":
						authCalls++
						if failure == "transport" {
							return nil, context.Canceled
						}
						w.WriteHeader(http.StatusUnauthorized)
						fmt.Fprint(w, `{"error":{"code":"INVALID_CREDENTIAL","message":"Session rejected."}}`)
					case "/v1/openapi.json":
						discoveryCalls++
						w.Header().Set("Flint-Version", "2026-02-01")
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
					}
					return w.Result(), nil
				})}
				exit := app.Run([]string{"doctor", "--output", output})
				if exit == ExitOK || authCalls != 1 {
					t.Fatalf("failure=%s exit=%d authCalls=%d", failure, exit, authCalls)
				}
				if output == "json" {
					var value map[string]any
					if err := json.Unmarshal(out.Bytes(), &value); err != nil {
						t.Fatal(err)
					}
					if lookupPathValue(value, "data.cli.api_base_url") != savedBaseURL || lookupPathValue(value, "data.cli.credential_source") != "oauth_session" {
						t.Fatalf("incorrect saved identity: %v", value)
					}
				} else if !strings.Contains(out.String(), "API base URL: "+savedBaseURL) {
					t.Fatalf("incorrect saved identity: %s", out)
				}
				if failure == "rejected" && discoveryCalls != 1 {
					t.Fatalf("diagnostics did not probe the saved server: %d", discoveryCalls)
				}
				if strings.Contains(out.String()+stderr.String(), defaultAPIBaseURL) {
					t.Fatal("diagnostics reported the production default instead of the saved server")
				}
			})
		}
	}
}
