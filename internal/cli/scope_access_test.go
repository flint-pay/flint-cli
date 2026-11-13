package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

func scopeErrorDetail(t *testing.T, e *CLIError) map[string]any {
	t.Helper()
	body, ok := e.Details.(map[string]any)
	if !ok {
		t.Fatalf("scope error body: %#v", e.Details)
	}
	details, ok := body["details"].([]any)
	if !ok || len(details) != 1 {
		t.Fatalf("scope details must be an API detail array: %#v", body)
	}
	return details[0].(map[string]any)
}

func TestTerminalScopeErrorUsesPublicRepresentation(t *testing.T) {
	for _, name := range []string{"products.list", "orders.edits.create"} {
		t.Run(name, func(t *testing.T) {
			app, out, stderr := testApp(t, "http://127.0.0.1:9")
			cmd, _ := app.Registry.ByName(name)
			e := commandPermissionError(cmd, AuthContext{AuthType: "api_key", Environment: "sandbox"}, "default")
			if e == nil || e.Code != "INSUFFICIENT_SCOPE" || e.ExitCode != ExitAuth {
				t.Fatalf("scope refusal: %#v", e)
			}
			app.writeError(e, Options{Output: "json"})
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Details []struct {
						Code             string                   `json:"code"`
						Message          string                   `json:"message"`
						Reason           string                   `json:"reason"`
						ScopeRequirement opindex.ScopeRequirement `json:"scope_requirement"`
						MissingScopes    []string                 `json:"missing_scopes"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "INSUFFICIENT_SCOPE" || len(body.Error.Details) != 1 {
				t.Fatal(out.String())
			}
			detail := body.Error.Details[0]
			if detail.Code != body.Error.Code || detail.Message != e.Message || detail.Reason != "scope_not_granted" || !reflect.DeepEqual(detail.ScopeRequirement, *commandScopeRequirement(cmd)) || len(detail.MissingScopes) == 0 {
				t.Fatalf("scope facts: %#v", detail)
			}
			for _, obsolete := range []string{"MISSING_REQUIRED_SCOPES", `"scope_mode"`, `"required_scopes"`, `"problems"`, `"facts"`} {
				if strings.Contains(out.String(), obsolete) {
					t.Fatalf("obsolete representation %s in %s", obsolete, out)
				}
			}
			app.writeError(e, Options{Output: "human"})
			if !strings.Contains(stderr.String(), "flint login --new-session") || !strings.Contains(stderr.String(), "?create=true&scope=") {
				t.Fatalf("missing human remediation: %s", stderr)
			}
		})
	}
}

func TestSchemaCommandPublishesScopeRequirement(t *testing.T) {
	app, out, stderr := testApp(t, "http://127.0.0.1:9")
	for _, name := range []string{"products.list", "orders.edits.create"} {
		out.Reset()
		if exit := app.Run([]string{"schema", "command", name, "--output", "json"}); exit != ExitOK {
			t.Fatalf("exit=%d stderr=%s", exit, stderr)
		}
		var body struct {
			Data struct {
				Requirement *opindex.ScopeRequirement `json:"scope_requirement"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		cmd, _ := app.Registry.ByName(name)
		if !reflect.DeepEqual(body.Data.Requirement, commandScopeRequirement(cmd)) {
			t.Fatalf("%s: %s", name, out)
		}
	}
	local, _ := app.Registry.ByName("version")
	if _, exists := commandDocument(local, "version")["scope_requirement"]; exists {
		t.Fatal("local command has a scope requirement")
	}
}

func TestScopeRemediationCredentialPaths(t *testing.T) {
	t.Setenv("FLINT_API_KEY", "")
	t.Setenv("FLINT_ACCESS_TOKEN", "")
	cmd, _ := NewRegistry().ByName("orders.edits.create")
	for _, kind := range []string{"api_key", "cli_login", "workload_identity", "legacy_workload"} {
		t.Run(kind, func(t *testing.T) {
			auth := AuthContext{AuthType: kind, Environment: "live", Scopes: []string{"commerce.orders.write"}}
			if kind == "legacy_workload" {
				auth.AuthType, auth.WorkloadIdentityID = "api_key", "wli_example"
			}
			e := commandPermissionError(cmd, auth, "team account")
			if e == nil {
				t.Fatal("missing scope error")
			}
			raw, _ := json.Marshal(e.Details.(map[string]any)["remediation"])
			guidance := string(raw)
			if strings.Contains(kind, "workload") {
				for _, want := range []string{"workload identity", "FLINT_OIDC_SCOPE", "commerce.refunds.write"} {
					if !strings.Contains(guidance, want) {
						t.Fatalf("missing %s: %s", want, guidance)
					}
				}
				for _, wrong := range []string{"flint login", "flint auth import", "flint reauth"} {
					if strings.Contains(guidance, wrong) {
						t.Fatalf("interactive workload remedy: %s", guidance)
					}
				}
			} else {
				for _, want := range []string{"flint login --new-session --live --profile 'team account'", "--scope commerce.orders.write", "--scope commerce.refunds.write"} {
					if !strings.Contains(guidance, want) {
						t.Fatalf("new session omitted held or missing scope: %s", guidance)
					}
				}
			}
		})
	}
	for _, override := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN"} {
		t.Run(override, func(t *testing.T) {
			t.Setenv("FLINT_API_KEY", "synthetic")
			if override == "FLINT_ACCESS_TOKEN" {
				t.Setenv(override, "synthetic")
			}
			e := commandPermissionError(cmd, AuthContext{AuthType: "api_key"}, "default")
			raw, _ := json.Marshal(e.Details.(map[string]any)["remediation"])
			if !strings.Contains(string(raw), "Replace "+override) || override == "FLINT_ACCESS_TOKEN" && strings.Contains(string(raw), "Replace FLINT_API_KEY") {
				t.Fatalf("wrong active override: %s", raw)
			}
		})
	}
}

func TestScopeAccessDiscoversPartialMerchantGroups(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	auth := AuthContext{AuthType: "api_key", Scopes: []string{"commerce.products.read"}}
	access := app.scopeAccess(auth)
	if access.Status != "known" || access.AllowedCommands == 0 || access.AllowedCommands+len(access.BlockedCommands) != access.ScopedCommands {
		t.Fatalf("scope totals: %+v", access)
	}
	if !slices.Contains(access.BlockedCommands, "products.create") || slices.Contains(access.BlockedCommands, "products.list") {
		t.Fatalf("read-only product access: %+v", access)
	}
	partial := false
	for _, group := range access.BlockedResourceGroups {
		if group.Group == "products" {
			partial = len(group.BlockedCommands) > 0 && len(group.BlockedCommands) < group.ScopedCommands
		}
		if group.Group == "me" || group.Group == "remembered-buyers" {
			t.Fatalf("buyer-only group offered to merchant credential: %+v", group)
		}
	}
	if !partial {
		t.Fatal("product group was treated as wholly blocked")
	}
	product, _ := app.Registry.ByName("products.list")
	if e := commandPermissionError(product, AuthContext{AuthType: "api_key", Scopes: []string{"commerce.products.write"}}, ""); e != nil {
		t.Fatalf("shared implied scopes were ignored: %v", e)
	}
	full := app.scopeAccess(AuthContext{AuthType: "api_key", Scopes: allTestScopes()})
	if len(full.BlockedCommands) != 0 || full.AllowedCommands != full.ScopedCommands {
		t.Fatalf("full scope access: %+v", full)
	}
}

func TestScopeAccessAuthStatusAndDoctor(t *testing.T) {
	for _, kind := range []string{"api_key", "onboarding_session", "partner_app"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"auth_type":%q,"mode":"test","merchant_id":"mer_example","scopes":["commerce.products.read"]}}`, kind)
		}))
		for _, command := range []string{"auth status", "doctor"} {
			for _, output := range []string{"human", "json"} {
				t.Run(kind+"/"+command+"/"+output, func(t *testing.T) {
					app, out, stderr := testApp(t, server.URL)
					args := append(strings.Fields(command), "--output", output)
					if exit := app.Run(args); exit != ExitOK {
						t.Fatalf("exit=%d out=%s err=%s", exit, out, stderr)
					}
					if output == "human" {
						if !strings.Contains(out.String(), "Scope access") || strings.Contains(out.String(), "map[") {
							t.Fatalf("unreadable discovery: %s", out)
						}
					} else {
						var body map[string]any
						_ = json.Unmarshal(out.Bytes(), &body)
						path := "data.scope_access.status"
						if command == "doctor" {
							data := body["data"].(map[string]any)
							for _, item := range data["checks"].([]any) {
								check := item.(map[string]any)
								if check["name"] == "scopes" {
									body = check
									path = "scope_access.status"
								}
							}
						}
						got, _ := lookupPath(body, path)
						want := "unknown"
						if kind == "api_key" {
							want = "known"
						}
						if got != want {
							t.Fatalf("scope knowledge %v != %s: %s", got, want, out)
						}
					}
				})
			}
		}
		if kind == "api_key" {
			app, out, stderr := testApp(t, server.URL)
			if exit := app.Run([]string{"auth", "status", "--field", "scope_access.status"}); exit != ExitOK || out.String() != "known\n" {
				t.Fatalf("auth status scope field: exit=%d out=%s err=%s", exit, out, stderr)
			}
		}
		server.Close()
	}
	for _, kind := range []string{"onboarding_session", "partner_app", "session"} {
		if got := doctorScopeCheck(AuthContext{AuthType: kind}); got["status"] == "fail" {
			t.Fatalf("scope-less context rejected: %+v", got)
		}
	}
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	cmd, _ := app.Registry.ByName("auth.status")
	root, defs := resourceOutputSchema(cmd)
	if _, ok, _ := outputSchemaPath(root, defs, []string{"scope_access", "blocked_commands"}, 0); !ok {
		t.Fatal("auth status discovery is missing from the output schema")
	}
}

func TestScopeAPIErrorArrayIsPreserved(t *testing.T) {
	app, out, _ := testApp(t, "http://127.0.0.1:9")
	detail := []any{map[string]any{"code": "INSUFFICIENT_SCOPE", "reason": "scope_not_granted", "scope_requirement": map[string]any{"mode": "any", "scopes": []string{"commerce.products.read", "commerce.products.write"}}, "missing_scopes": []string{"commerce.products.read", "commerce.products.write"}}}
	body := map[string]any{"type": "authorization_error", "code": "INSUFFICIENT_SCOPE", "message": "The required scopes are missing.", "details": detail}
	e := cliError(ExitAuth, errorTypeAuthorization, "INSUFFICIENT_SCOPE", "The required scopes are missing.")
	e.Details = body
	before, _ := json.Marshal(body)
	app.writeError(e, Options{Output: "json"})
	after, _ := json.Marshal(body)
	if !bytes.Equal(before, after) || strings.Contains(out.String(), `"problems"`) || !strings.Contains(out.String(), `"details":[`) {
		t.Fatalf("API details changed: %s", out)
	}
}
