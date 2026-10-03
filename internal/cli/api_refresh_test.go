package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestCLIOAuthCommandsSendFormRequests(t *testing.T) {
	for _, test := range []struct {
		path  string
		input map[string]string
	}{
		{"/v1/oauth/device/authorize", map[string]string{"client_id": "flint-cli", "scope": "customers.read webhooks.read", "device_name": "Aaron's Mac + terminal"}},
		{"/v1/oauth/device/reauthorize", map[string]string{"client_id": "flint-cli", "refresh_token": "synthetic + / & token", "scope": "commerce.gift_cards.read", "session_mode": "contexts"}},
		{"/v1/oauth/contexts", map[string]string{"client_id": "flint-cli", "refresh_token": "synthetic + / & token"}},
		{"/v1/oauth/revoke", map[string]string{"client_id": "flint-cli", "token": "synthetic + / & token"}},
	} {
		for _, raw := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/raw=%t", test.path, raw), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Method != http.MethodPost || r.URL.Path != test.path {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
						t.Errorf("content type = %q", got)
					}
					if r.Header.Get("Authorization") != "" {
						t.Error("public OAuth request sent an unrelated API credential")
					}
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					want := url.Values{}
					for name, value := range test.input {
						want.Set(name, value)
					}
					if !reflect.DeepEqual(r.PostForm, want) {
						t.Errorf("form = %v, want %v", r.PostForm, want)
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"device_code":"synthetic_response"}`)
				}))
				defer server.Close()
				app, stdout, stderr := testApp(t, server.URL)
				input, err := json.Marshal(test.input)
				if err != nil {
					t.Fatal(err)
				}
				app.Stdin = strings.NewReader(string(input))
				var argv []string
				for _, command := range app.Registry.Commands {
					if command.Method == "POST" && command.APIPath == test.path {
						argv = append(argv, command.Path[1:]...)
						if command.InputSchema == "" {
							t.Fatal("form command has no input schema")
						}
						break
					}
				}
				if len(argv) == 0 {
					t.Fatal("form command missing from registry")
				}
				if raw {
					argv = []string{"api", "post", test.path}
				}
				argv = append(argv, "--input", "-", "--confirm", "--output", "json")
				if code := app.Run(argv); code != ExitOK || calls != 1 || !strings.Contains(stdout.String(), "synthetic_response") {
					t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", code, calls, stdout, stderr)
				}
			})
		}
	}
}

func TestFormRequestsRejectNonStringFields(t *testing.T) {
	for _, input := range []string{`{"client_id":42}`, `{"scope":["customers.read"]}`, `{"token":null}`, `{"token":{"nested":"value"}}`} {
		t.Run(input, func(t *testing.T) {
			app, _, _ := testApp(t, "")
			app.Stdin = strings.NewReader(input)
			command, options, _, err := parseInvocation(app.Registry, []string{"oauth", "device", "authorize", "--input", "-"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = app.prepareRequest(context.Background(), command, options, ResolvedConfig{}, "", "")
			if err == nil || err.Code != "INVALID_INPUT" {
				t.Fatalf("expected invalid form input, got %v", err)
			}
		})
	}
}

func TestOAuthTokenOutputSchemaAcceptsPartnerAndCLITokens(t *testing.T) {
	command, ok := NewRegistry().ByName("oauth.token")
	if !ok {
		t.Fatal("OAuth token command missing")
	}
	schema, err := schemaForCommand(command, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := schema["anyOf"]; !ok {
		t.Fatal("OAuth output schema omitted the public token alternatives")
	}
	for _, response := range []map[string]any{
		{"access_token": "synthetic", "expires_in": 900, "refresh_token": "synthetic", "scope": "customers.read", "token_type": "Bearer"},
		{"access_token": "synthetic", "expires_in": 3600, "environment_grant_id": "egrt_example", "merchant_id": "mer_example", "mode": "test", "partner_app_id": "papp_example", "partner_app_install_id": "pinst_example", "token_type": "bearer"},
	} {
		assertMCPOutputMatchesSchema(t, schema, response)
	}
}
