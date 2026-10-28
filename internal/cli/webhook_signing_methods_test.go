package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestWebhookSigningMethodRequests(t *testing.T) {
	for _, tc := range []struct {
		name, action, input string
		flags               []string
		wantMethods         []any
		wantVersion         bool
		wantExit            int
	}{
		{name: "create defaults on server", action: "create"},
		{name: "create jwt", action: "create", flags: []string{"--signing-method", "jwt"}, wantMethods: []any{"jwt"}},
		{name: "create hmac", action: "create", flags: []string{"--signing-method", "hmac"}, wantMethods: []any{"hmac"}},
		{name: "create both", action: "create", flags: []string{"--signing-method", "jwt", "--signing-method", "hmac"}, wantMethods: []any{"jwt", "hmac"}},
		{name: "invalid method", action: "create", flags: []string{"--signing-method", "rsa"}, wantExit: ExitUsage},
		{name: "duplicate method", action: "create", flags: []string{"--signing-method", "jwt", "--signing-method", "jwt"}, wantExit: ExitUsage},
		{name: "update scalar without version", action: "update", flags: []string{"--url", "https://example.com/new"}},
		{name: "update signing without version", action: "update", flags: []string{"--signing-method", "jwt"}, wantExit: ExitUsage},
		{name: "update events without version", action: "update", flags: []string{"--enabled-event", "payment_intent.succeeded"}, wantExit: ExitUsage},
		{name: "update signing with version", action: "update", flags: []string{"--signing-method", "jwt", "--signing-method", "hmac", "--expected-version", "3"}, wantMethods: []any{"jwt", "hmac"}, wantVersion: true},
		{name: "update input without version", action: "update", input: `{"signing_methods":["jwt"]}`, wantExit: ExitUsage},
		{name: "update input with version", action: "update", input: `{"signing_methods":["jwt"],"expected_version":3}`, wantMethods: []any{"jwt"}, wantVersion: true},
		{name: "secret not issued", action: "update", flags: []string{"--signing-method", "hmac", "--expected-version", "3"}, wantMethods: []any{"hmac"}, wantVersion: true, wantExit: ExitAPI},
		{name: "empty set", action: "update", input: `{"signing_methods":[],"expected_version":3}`, wantExit: ExitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/developer/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				wantMethod, wantPath := "POST", "/v1/webhook-endpoints"
				if tc.action == "update" {
					wantMethod, wantPath = "PATCH", wantPath+"/whep_test"
				}
				if r.Method != wantMethod || r.URL.Path != wantPath {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if tc.name == "secret not issued" {
					w.WriteHeader(http.StatusConflict)
					fmt.Fprint(w, `{"error":{"type":"conflict_error","code":"WEBHOOK_SECRET_NOT_ISSUED","message":"Use rotate-secret to issue the first secret and enable hmac."}}`)
					return
				}
				methods := tc.wantMethods
				if methods == nil {
					methods = []any{"jwt"}
				}
				data := map[string]any{"webhook_endpoint_id": "whep_test", "signing_methods": methods, "version": 3}
				if tc.action == "create" && slices.Contains(methods, any("hmac")) {
					data["secret"] = "synthetic-shared-secret"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer server.Close()
			app, out, errOut := testApp(t, server.URL)
			args := []string{"webhook-endpoints", tc.action}
			if tc.action == "create" {
				args = append(args, "--url", "https://example.com/webhooks/flint", "--enabled-event", "payment_intent.succeeded")
			} else {
				args = append(args, "whep_test")
			}
			args = append(args, tc.flags...)
			if tc.input != "" {
				args = append(args, "--input", tc.input)
			}
			args = append(args, "--output", "json", "--idempotency-key", "synthetic-webhook-action")
			if exit := app.Run(args); exit != tc.wantExit {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, errOut)
			}
			if tc.wantExit == ExitAPI {
				if got == nil || !strings.Contains(out.String(), `"code":"WEBHOOK_SECRET_NOT_ISSUED"`) || !strings.Contains(out.String(), `"http_status":409`) {
					t.Fatalf("missing conflict: stdout=%s stderr=%s", out, errOut)
				}
				return
			}
			if tc.wantExit != ExitOK {
				if got != nil {
					t.Fatal("invalid body reached endpoint")
				}
				return
			}
			if got == nil {
				t.Fatal("endpoint was not called")
			}
			methods, present := got["signing_methods"]
			if present != (tc.wantMethods != nil) || (present && !reflect.DeepEqual(methods, tc.wantMethods)) {
				t.Fatalf("signing_methods = %#v, want %#v", methods, tc.wantMethods)
			}
			if version, present := got["expected_version"]; present != tc.wantVersion || (present && version != float64(3)) {
				t.Fatalf("expected_version = %#v, present=%v", version, present)
			}
			if tc.action == "create" && !slices.Contains(tc.wantMethods, any("hmac")) && (strings.Contains(out.String(), `"secret"`) || strings.Contains(strings.ToLower(errOut.String()), "save")) {
				t.Fatalf("unexpected secret advice: stdout=%s stderr=%s", out, errOut)
			}
		})
	}
}

func TestWebhookSigningMCPValidation(t *testing.T) {
	for _, action := range []string{"create", "update"} {
		cmd, _ := NewRegistry().ByName("webhook-endpoints." + action)
		for _, methods := range [][]any{nil, {"jwt"}, {"hmac"}, {"jwt", "hmac"}, {}, {"jwt", "jwt"}, {"unknown"}} {
			for _, withVersion := range []bool{false, true} {
				args := map[string]any{}
				if action == "create" {
					args["url"], args["enabled_events"] = "https://example.com/webhooks/flint", []any{"payment_intent.succeeded"}
				} else {
					args["webhook_endpoint_id"] = "whep_test"
					if withVersion {
						args["expected_version"] = float64(3)
					}
				}
				if methods != nil {
					args["signing_methods"] = methods
				}
				valid := methods == nil || reflect.DeepEqual(methods, []any{"jwt"}) || reflect.DeepEqual(methods, []any{"hmac"}) || reflect.DeepEqual(methods, []any{"jwt", "hmac"})
				valid = valid && (action == "create" || methods == nil || withVersion)
				if err := validateMCPArguments(cmd, args); (err == nil) != valid {
					t.Errorf("%s methods=%v version=%v valid=%v: %v", action, methods, withVersion, valid, err)
				}
			}
		}
	}
}

func TestWebhookCreateOutputSchemaAllowsAbsentSecret(t *testing.T) {
	cmd, _ := NewRegistry().ByName("webhook-endpoints.create")
	schema, err := schemaForCommand(cmd, false)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("https://example.com/webhook-output", schema); err != nil {
		t.Fatal(err)
	}
	compiled, compileErr := compiler.Compile("https://example.com/webhook-output")
	if compileErr != nil {
		t.Fatal(compileErr)
	}
	for _, withSecret := range []bool{false, true} {
		body := map[string]any{"data": map[string]any{
			"webhook_endpoint_id": "whep_test", "api_version": "default", "enabled_events": []any{"payment_intent.succeeded"},
			"signing_methods": []any{"jwt"}, "status": "active", "url": "https://example.com/webhooks/flint", "version": float64(1),
		}}
		if withSecret {
			body["data"].(map[string]any)["secret"] = "synthetic-shared-secret"
			body["data"].(map[string]any)["signing_methods"] = []any{"hmac"}
		}
		if err := compiled.Validate(body); err != nil {
			t.Errorf("withSecret=%v: %v", withSecret, err)
		}
	}
}

func TestWebhookSigningHelpAndMCPDescriptions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wants []string
	}{
		{"webhook-endpoints.create", []string{"--signing-method", "jwt (default, verify with Flint's public keys) or hmac (shared secret)", "secret is returned only when hmac is included", "--signing-method jwt"}},
		{"webhook-endpoints.update", []string{"--signing-method", "required with --enabled-event or --signing-method"}},
		{"webhook-endpoints.rotate-secret", []string{"Rotate a webhook endpoint secret, or issue the first one and enable hmac"}},
		{"listen", []string{"local HMAC listen secret", "regardless of endpoint signing methods", "JWT handlers need an HMAC path or a local test bypass"}},
	} {
		app, out, errOut := testApp(t, "")
		if exit := app.Run([]string{"help", tc.name}); exit != ExitOK {
			t.Fatalf("%s exit=%d stdout=%s stderr=%s", tc.name, exit, out, errOut)
		}
		for _, want := range tc.wants {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s help lacks %q: %s", tc.name, want, out)
			}
		}
	}
	for name, wants := range map[string][]string{
		"webhook-endpoints.create":        {"signing_methods defaults to [jwt]", "Authorization: Bearer <token>", "Flint's JWKS", "save it if present"},
		"webhook-endpoints.update":        {"replaces the set", "409 WEBHOOK_SECRET_NOT_ISSUED", "expected_version is required when sending", "signing_methods"},
		"webhook-endpoints.rotate-secret": {"issue the first one and enable hmac"},
		"listen":                          {"HMAC listen secret", "regardless of endpoint signing methods", "JWT handlers need an HMAC path or a local test bypass"},
	} {
		cmd, _ := NewRegistry().ByName(name)
		schema, _ := schemaForMCPTool(cmd, mcpPolicy{})
		description := mcpToolDescription(cmd, schema)
		for _, want := range wants {
			if !strings.Contains(description, want) {
				t.Errorf("%s MCP description lacks %q: %s", name, want, description)
			}
		}
	}
}
