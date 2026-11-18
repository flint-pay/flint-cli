package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMCPSecretResults(t *testing.T) {
	for _, tc := range []struct {
		name, operation, tool, route, reply, secret, pointer, id, mask string
		arguments                                                      map[string]any
		selectFields                                                   []any
		typed                                                          bool
	}{
		{
			name: "api key", operation: "api-keys.create", tool: "flint_write", route: "/v1/api-keys",
			reply:  `{"data":{"api_key_id":"key_fixture","secret_key":"flint_test_synthetic_created","echo":"Created flint_test_synthetic_created"}}`,
			secret: "flint_test_synthetic_created", pointer: "/data/secret_key", id: "key_fixture", mask: "flint_test_…",
			arguments: map[string]any{"name": "fixture", "access": "full"}, selectFields: []any{"secret_key"},
		},
		{
			name: "dispute", operation: "disputes.response-sessions.create", tool: "flint_write", route: "/v1/disputes/dsp_fixture/response-sessions",
			reply:  `{"data":{"dispute_response_session_id":"drs_fixture","client_session":{"stripe":{"account_session":{"client_secret":"synthetic_stripe_session_value"}}}}}`,
			secret: "synthetic_stripe_session_value", pointer: "/data/client_session", id: "drs_fixture", mask: mcpRedacted,
			arguments: map[string]any{"dispute_id": "dsp_fixture"},
		},
		{
			name: "webhook creation", operation: "webhook-endpoints.create", tool: "flint_write", route: "/v1/webhook-endpoints",
			reply:  `{"data":{"webhook_endpoint_id":"whep_fixture","url":"https://example.com/webhooks","secret":"whsec_synthetic_create"}}`,
			secret: "whsec_synthetic_create", pointer: "/data/secret", id: "whep_fixture", mask: "whsec_…",
			arguments: map[string]any{"url": "https://example.com/webhooks", "enabled_events": []any{"order.paid"}, "signing_methods": []any{"hmac"}},
		},
		{
			name: "typed webhook rotation", operation: "webhook-endpoints.rotate-secret", tool: "webhook_endpoints_rotate_secret", route: "/v1/webhook-endpoints/whep_fixture/rotate-secret",
			reply:  `{"data":{"webhook_endpoint_id":"whep_fixture","secret":"whsec_synthetic_rotation"}}`,
			secret: "whsec_synthetic_rotation", pointer: "/data/secret", id: "whep_fixture", mask: "whsec_…",
			arguments: map[string]any{"webhook_endpoint_id": "whep_fixture"}, typed: true,
		},
		{
			name: "read with opaque checkout link", operation: "checkout-sessions.list", tool: "flint_read", route: "/v1/checkout-sessions",
			reply:  `{"data":[{"checkout_session_id":"cks_fixture","url":"https://example.com/opaque-launch-credential"}],"next_page_token":"fixture_next_page"}`,
			secret: "https://example.com/opaque-launch-credential", pointer: "/data/0/url", id: "cks_fixture", mask: mcpRedacted,
			selectFields: []any{"url", "checkout_session_id"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/auth-context":
					fmt.Fprint(w, authContextJSON("sandbox"))
				case tc.route:
					fmt.Fprint(w, tc.reply)
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			opts := defaultOptions()
			args := map[string]any{"operation": tc.operation, "idempotency_key": "synthetic-secret-test", "confirm": true}
			if tc.arguments != nil {
				args["arguments"] = tc.arguments
			}
			if tc.selectFields != nil {
				args["select"] = tc.selectFields
			}
			if tc.tool == "flint_read" {
				delete(args, "idempotency_key")
				delete(args, "confirm")
			}
			if tc.typed {
				opts = mcpServeOptions(t, "--tools", tc.operation)
				args = map[string]any{"webhook_endpoint_id": tc.arguments["webhook_endpoint_id"], "confirm": true, "idempotency_key": "synthetic-secret-test"}
			}
			result := mcpCall(t, app, opts, tc.tool, args)
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			serialized := string(raw)
			if strings.Contains(serialized, tc.secret) {
				t.Fatal("serialized MCP result exposed the synthetic credential")
			}
			if result["isError"] == true || !strings.Contains(serialized, tc.id) || !strings.Contains(serialized, tc.mask) {
				t.Fatalf("missing successful result, ID or mask: %s", serialized)
			}
			matches, err := filepath.Glob(filepath.Join(app.ConfigDir, "mcp-secrets", "result-*", "secrets.json"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("expected a single private file, got %d (%v)", len(matches), err)
			}
			path := matches[0]
			content := result["content"].([]map[string]any)
			notes, _ := content[len(content)-1]["text"].(string)
			if !strings.Contains(notes, path) || !strings.Contains(notes, "Only the user should open this file") {
				t.Fatal("tool result did not tell the user how to retrieve the secret")
			}
			if tc.tool == "flint_read" && !strings.Contains(serialized, "fixture_next_page") {
				t.Fatal("result no longer includes the pagination cursor")
			}
			for file, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
				info, err := os.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS != "windows" && info.Mode().Perm() != mode {
					t.Fatalf("permissions %o, want %o", info.Mode().Perm(), mode)
				}
			}
			stored, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(stored, &fields); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(fields[tc.pointer]), tc.secret) {
				t.Fatal("secret not recoverable from the private file")
			}
		})
	}
}

func TestMCPSecretStorageFailurePreservesSuccessfulResult(t *testing.T) {
	app := &App{ConfigDir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(app.ConfigDir, "mcp-secrets"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	const secret = "fixture-opaque-secret"
	result, notes, fields := maskMCPResult("createAPIKey", map[string]any{
		"data": map[string]any{"api_key_id": "key_fixture", "secret_key": secret},
	}, "echoing "+secret)
	notes += app.mcpSecretNote(fields)
	raw, err := mcpCompactJSON(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw)+notes, secret) || !strings.Contains(string(raw), "key_fixture") ||
		!strings.Contains(notes, "operation succeeded") || !strings.Contains(notes, "could not save") {
		t.Fatal("storage failure did not preserve the safe success result and instructions")
	}
}

func TestMCPErrorSecretsAreRedacted(t *testing.T) {
	app := &App{}
	const secret = "opaque-synthetic-value"
	e := usageError("TEST_ERROR", "Rejected credential "+secret, "")
	e.Details = map[string]any{"fields": []any{map[string]any{"refresh_token": secret}}}
	result := app.mcpLocalError(nil, nil, Options{}, mcpPolicy{}, e, "")
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "TEST_ERROR") {
		t.Fatal("error credential was exposed or the error code was lost")
	}
}

func TestMCPSecretResultPreservesPublicURLsAndMasksCapabilities(t *testing.T) {
	const public = "https://example.com/return"
	const signed = "https://example.com/download?X-Amz-Signature=fixture"
	const bearer = "https://example.com/opaque-customer-link"
	input := map[string]any{"data": map[string]any{
		"url": bearer, "return_url": public, "signed_download": signed,
		"opaque": map[string]any{"signing_secret": "fixture-signing-value"},
	}}
	result, _, fields := maskMCPResult("createCustomerAccessLink", input, "")
	data := mcpObject(mcpObject(result)["data"])
	if data["return_url"] != public || data["url"] != mcpRedacted || data["signed_download"] != mcpRedacted ||
		fields["/data/url"] != bearer || fields["/data/signed_download"] != signed ||
		fields["/data/opaque/signing_secret"] != "fixture-signing-value" {
		t.Fatal("expected the return URL to remain public and the credentials to be masked and collected")
	}
	if mcpObject(input["data"])["url"] != bearer {
		t.Fatal("redaction mutated the source used by pagination")
	}
}
