package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestScopeKeyModesMetadata(t *testing.T) {
	var doc openAPIDocument
	if err := json.Unmarshal([]byte(`{"x-flint-scopes":[
  {"name":"live_only","key_modes":["live"]},
  {"name":"ordinary","key_modes":["live","test"]},
  {"name":"empty","key_modes":[]},
  {"name":"unknown","key_modes":["future"]},
  {"name":"mixed_unknown","key_modes":["live","future"]}
 ]}`), &doc); err != nil {
		t.Fatal(err)
	}
	for _, environment := range []string{"sandbox", "test", "live", "", "future"} {
		for _, scope := range []string{"live_only", "ordinary", "empty", "unknown", "mixed_unknown", "absent"} {
			want := scope == "live_only" && (environment == "sandbox" || environment == "test")
			if got := doc.scopeNeedsLiveKey(scope, environment); got != want {
				t.Errorf("%s/%s needs live = %t, want %t", environment, scope, got, want)
			}
		}
	}
	embedded, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"accounts.organizations.write", "developer.partner_apps.read", "developer.partner_apps.write", "developer.sandboxes.read", "developer.sandboxes.write"} {
		if !embedded.scopeNeedsLiveKey(scope, "sandbox") {
			t.Errorf("embedded scope %s is not live-only", scope)
		}
	}
	for _, scope := range initialCLIScopes {
		if embedded.scopeNeedsLiveKey(scope, "sandbox") {
			t.Errorf("current baseline unexpectedly requires live: %s", scope)
		}
	}
}

func TestScopeKeyModesGeneratedCommands(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"partner-apps.list", "sandboxes.list", "organizations.create", "sandboxes.api-keys.create"} {
		cmd, ok := registry.ByName(name)
		if !ok {
			t.Fatalf("missing generated command %s", name)
		}
		for _, authType := range []string{"api_key", "oauth"} {
			for _, environment := range []string{"sandbox", "test", "live", ""} {
				t.Run(name+"/"+authType+"/"+environment, func(t *testing.T) {
					auth := AuthContext{AuthType: authType, Environment: environment, OAuthSessionID: "session_test", ContextID: "ctx_test", Scopes: []string{"customers.read"}}
					e := commandPermissionError(cmd, auth, "default")
					if e == nil || e.Code != "MISSING_REQUIRED_SCOPES" || e.ExitCode != ExitAuth {
						t.Fatalf("permission error = %#v", e)
					}
					details := e.Details.(map[string]any)
					liveRequired := environment == "sandbox" || environment == "test"
					if (details["reason"] == "live_mode_required") != liveRequired {
						t.Fatalf("details = %#v", details)
					}
					actions, _ := json.Marshal(details["remediation"])
					if liveRequired {
						if !strings.Contains(string(actions), "live key") || strings.Contains(string(actions), "flint reauth") || strings.Contains(string(actions), "flint login") || strings.Contains(string(actions), "Create an API key") {
							t.Fatalf("impossible sandbox guidance: %s", actions)
						}
					} else if authType == "oauth" && !strings.Contains(string(actions), "flint reauth --context ctx_test") {
						t.Fatalf("lost live/unknown OAuth guidance: %s", actions)
					} else if authType == "api_key" && !strings.Contains(string(actions), "Create an API key") {
						t.Fatalf("lost live/unknown key guidance: %s", actions)
					}
					// Synthetic sandbox fixtures grant every scope. Eligibility changes
					// remediation only, never a requirement already satisfied by the key.
					auth.Scopes = allTestScopes()
					if e := commandPermissionError(cmd, auth, "default"); e != nil {
						t.Fatalf("granted scopes rejected: %v", e)
					}
				})
			}
		}
	}
}

func TestScopeKeyModesMixedRequirements(t *testing.T) {
	if _, err := loadOpenAPI(); err != nil {
		t.Fatal(err)
	}
	const operationID = "scopeModesTest"
	cmd := &Command{CanonicalName: "test.scopes", OperationID: operationID}
	defer delete(openAPIOperations, operationID)
	for _, mode := range []string{"any", "all"} {
		openAPIOperations[operationID] = openAPIOperation{
			FlintScopesMode:     mode,
			FlintRequiredScopes: []string{"developer.sandboxes.write", "customers.write", "customers.read"},
		}
		for _, authType := range []string{"api_key", "oauth"} {
			for _, grantedLive := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/grantedLive=%t", mode, authType, grantedLive), func(t *testing.T) {
					auth := AuthContext{AuthType: authType, Environment: "sandbox", OAuthSessionID: "session_test", ContextID: "ctx_test"}
					if grantedLive {
						auth.Scopes = []string{"developer.sandboxes.write"}
					}
					e := commandPermissionError(cmd, auth, "default")
					if mode == "any" && grantedLive {
						if e != nil {
							t.Fatalf("granted alternative rejected: %v", e)
						}
						return
					}
					if e == nil {
						t.Fatal("missing permission error")
					}
					details := e.Details.(map[string]any)
					liveRequired := mode == "all" && !grantedLive
					if (details["reason"] == "live_mode_required") != liveRequired {
						t.Fatalf("details = %#v", details)
					}
					actions, _ := json.Marshal(details["remediation"])
					if liveRequired {
						if !strings.Contains(string(actions), "live key") || strings.Contains(string(actions), "flint reauth") {
							t.Fatalf("guidance = %s", actions)
						}
					} else {
						if (mode == "any" || authType == "oauth") && strings.Contains(string(actions), "developer.sandboxes.write") {
							t.Fatalf("unavailable alternative suggested: %s", actions)
						}
						if !strings.Contains(string(actions), "customers.write") {
							t.Fatalf("eligible scope omitted: %s", actions)
						}
						if authType == "oauth" && strings.Contains(string(actions), "--scope customers.read") != (mode == "all") {
							t.Fatalf("wrong OAuth scopes: %s", actions)
						}
					}
					if mode == "any" && !strings.Contains(strings.Join(details["missing_scopes"].([]string), ","), "developer.sandboxes.write") {
						t.Fatal("lost complete missing scope facts")
					}
				})
			}
		}
	}
}

func TestScopeKeyModesStopGeneratedRequests(t *testing.T) {
	for _, environment := range []string{"sandbox", "live"} {
		for _, args := range [][]string{{"partner-apps", "list"}, {"sandboxes", "list"}, {"organizations", "create", "--name", "Scope test", "--parent-organization-id", "org_test"}, {"sandboxes", "api-keys", "create", "test_123"}} {
			t.Run(environment+"/"+strings.Join(args, "."), func(t *testing.T) {
				targetRequests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/auth-context" {
						targetRequests++
						t.Errorf("target request: %s", r.URL.Path)
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, limitedAuthContextJSON(environment))
				}))
				defer server.Close()
				app, stdout, stderr := testApp(t, server.URL)
				if environment == "live" {
					t.Setenv("FLINT_API_KEY", "flint_live_test")
					args = append(slices.Clone(args), "--live")
				}
				args = append(slices.Clone(args), "--output", "json")
				if exit := app.Run(args); exit != ExitAuth {
					t.Fatalf("exit=%d out=%s err=%s", exit, stdout, stderr)
				}
				var result struct {
					Error struct {
						Code    string         `json:"code"`
						Details map[string]any `json:"details"`
					} `json:"error"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Error.Code != "MISSING_REQUIRED_SCOPES" || (result.Error.Details["reason"] == "live_mode_required") != (environment == "sandbox") || targetRequests != 0 {
					t.Fatalf("incorrect permission result: %s", stdout)
				}
			})
		}
	}
}

func TestDoctorScopeKeyModes(t *testing.T) {
	original := initialCLIScopes
	initialCLIScopes = []string{"customers.read", "developer.sandboxes.write"}
	defer func() { initialCLIScopes = original }()
	for _, authType := range []string{"api_key", "oauth"} {
		for _, environment := range []string{"sandbox", "live", ""} {
			for _, scopes := range [][]string{nil, {"customers.write"}, {"customers.read"}, {"customers.read", "developer.sandboxes.write"}} {
				check := doctorScopeCheck(AuthContext{AuthType: authType, Environment: environment, Scopes: scopes})
				missing, _ := check["missing_cli_baseline"].([]string)
				liveOnly, _ := check["live_required_scopes"].([]string)
				if environment == "sandbox" {
					if slices.Contains(missing, "developer.sandboxes.write") {
						t.Fatalf("impossible missing baseline: %#v", check)
					}
					wantLive := !slices.Contains(scopes, "developer.sandboxes.write")
					if slices.Contains(liveOnly, "developer.sandboxes.write") != wantLive {
						t.Fatalf("live-only facts = %#v", check)
					}
					if !strings.Contains(fmt.Sprint(check["note"]), "live key") {
						t.Fatalf("missing limitation: %#v", check)
					}
					if len(scopes) == 0 && !slices.Contains(missing, "customers.read") {
						t.Fatalf("ordinary scope omitted: %#v", check)
					}
				} else if len(liveOnly) > 0 || strings.Contains(fmt.Sprint(check["note"]), "live key") {
					t.Fatalf("changed live/unknown guidance: %#v", check)
				}
				if (check["status"] == "fail") != (len(scopes) == 0) {
					t.Fatalf("changed status: %#v", check)
				}
			}
		}
	}
	initialCLIScopes = []string{"developer.sandboxes.write"}
	check := doctorScopeCheck(AuthContext{Environment: "sandbox"})
	if fix := fmt.Sprint(check["fix"]); !strings.Contains(fix, "live key") || strings.Contains(fix, "Sign in again") {
		t.Fatalf("impossible empty-scope guidance: %#v", check)
	}
}

func TestScopeKeyModesOrganizationReadAlternative(t *testing.T) {
	cmd, ok := NewRegistry().ByName("organizations.list")
	if !ok {
		t.Fatal("missing organizations.list")
	}
	for _, authType := range []string{"api_key", "oauth"} {
		for _, environment := range []string{"sandbox", "live"} {
			e := commandPermissionError(cmd, AuthContext{AuthType: authType, Environment: environment, OAuthSessionID: "session_test", ContextID: "ctx_test"}, "default")
			if e == nil {
				t.Fatal("missing permission error")
			}
			details := e.Details.(map[string]any)
			if details["reason"] != nil {
				t.Fatalf("eligible read alternative requires live: %#v", details)
			}
			guidance, _ := json.Marshal(details["remediation"])
			if !strings.Contains(string(guidance), "accounts.organizations.read") {
				t.Fatalf("read alternative omitted: %s", guidance)
			}
			if environment == "sandbox" && (strings.Contains(string(guidance), "accounts.organizations.write") || strings.Contains(e.Message, "accounts.organizations.write")) {
				t.Fatalf("ineligible alternative suggested: %s / %s", e.Message, guidance)
			}
			if !slices.Equal(details["missing_scopes"].([]string), []string{"accounts.organizations.read", "accounts.organizations.write"}) {
				t.Fatalf("missing scope facts changed: %#v", details)
			}
		}
	}
}
