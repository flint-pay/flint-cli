package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthTokenOAuthRefresh(t *testing.T) {
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loginTokenPath:
			refreshes++
			writeTestTokens(w, "new-access", "new-refresh")
		case "/v1/auth-context":
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Error("export validated the wrong token")
			}
			fmt.Fprint(w, oauthAuthJSON("sandbox"))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	stored := installTestOAuth(t, app, testOAuthCredential(t, server.URL, time.Now().Add(-time.Minute)))
	if exit := app.Run([]string{"auth", "token", "--output", "json", "--debug"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	credential, e := decodeOAuthCredential(stored())
	if e != nil || refreshes != 1 || len(result) != 3 || result["access_token"] != credential.AccessToken || result["token_type"] != "Bearer" || result["expires_at"] != credential.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Fatal("export must contain only the refreshed bearer token, type, and expiry")
	}
	if strings.Contains(out.String(), "new-refresh") || strings.Contains(stderr.String(), "new-access") || strings.Contains(stderr.String(), "new-refresh") {
		t.Fatal("token leaked outside deliberate access-token output")
	}
}

func TestAuthTokenWorkload(t *testing.T) {
	exchanges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == loginTokenPath {
			exchanges++
			writeExchange(w, exchanges)
			return
		}
		fmt.Fprint(w, workloadAuthJSON("sandbox"))
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app.Stdout, app.Stderr = out, stderr
	now := time.Now().Truncate(time.Second)
	app.Now = func() time.Time { return now }
	if exit := app.Run([]string{"auth", "token", "--output", "json", "--debug"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if exchanges != 1 || len(result) != 3 || result["access_token"] != "fwat_synthetic_1" || result["token_type"] != "Bearer" || result["expires_at"] != now.Add(time.Hour).UTC().Format(time.RFC3339) {
		t.Fatal("incorrect workload token export")
	}
	if strings.Contains(stderr.String(), "fwat_synthetic") || strings.Contains(out.String()+stderr.String(), "eyJ") {
		t.Fatal("unexpected token exposure")
	}
}

func TestAuthTokenRefusesAPIKeysAndMissingCredentials(t *testing.T) {
	for _, source := range []string{"environment", "keychain", "missing", "wrong_variable"} {
		t.Run(source, func(t *testing.T) {
			app, out, stderr := testApp(t, "http://127.0.0.1:1")
			t.Setenv("FLINT_ACCESS_TOKEN", "")
			t.Setenv("FLINT_API_KEY", "")
			key := "flint_test_synthetic_export_refusal"
			want := "API_KEY_NOT_EXPORTABLE"
			switch source {
			case "environment":
				t.Setenv("FLINT_API_KEY", key)
				app.LoadCredential = func(string) (string, error) { t.Error("read saved login despite environment key"); return "", nil }
			case "keychain":
				app.LoadCredential = func(string) (string, error) { return key, nil }
			case "wrong_variable":
				t.Setenv("FLINT_ACCESS_TOKEN", key)
			case "missing":
				want = "API_KEY_REQUIRED"
			}
			if exit := app.Run([]string{"auth", "token", "--output", "json", "--debug"}); exit != ExitAuth || !strings.Contains(out.String(), want) {
				t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
			}
			if strings.Contains(out.String()+stderr.String(), key) {
				t.Fatal("API key exposed")
			}
		})
	}
}

func TestAuthTokenGuardsAndTokenTypes(t *testing.T) {
	for _, scenario := range []string{"live", "guard", "other_session", "ambiguous", "context"} {
		t.Run(scenario, func(t *testing.T) {
			body := oauthAuthJSON("sandbox")
			want := ""
			if scenario == "live" {
				body = oauthAuthJSON("live")
				want = "LIVE"
			}
			if scenario == "other_session" {
				body = strings.ReplaceAll(body, `"auth_type":"oauth"`, `"auth_type":"session"`)
				want = "TOKEN_NOT_EXPORTABLE"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_ACCESS_TOKEN", "synthetic-export-access")
			argv := []string{"auth", "token", "--output", "json"}
			switch scenario {
			case "guard":
				argv = append(argv, "--merchant", "mer_other")
				want = "MERCHANT_GUARD_MISMATCH"
			case "context":
				argv = append(argv, "--context", "ctx_other")
				want = "CONTEXT"
			case "ambiguous":
				t.Setenv("FLINT_API_KEY", "flint_test_synthetic_export_refusal")
			}
			exit := app.Run(argv)
			if scenario == "ambiguous" {
				if exit != ExitOK || !strings.Contains(out.String(), `"access_token":"synthetic-export-access"`) || !strings.Contains(stderr.String(), "takes precedence") {
					t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
				}
			} else if exit == ExitOK || !strings.Contains(out.String(), want) || strings.Contains(out.String()+stderr.String(), "synthetic-export-access") {
				t.Fatalf("unsafe export: exit=%d out=%s stderr=%s", exit, out, stderr)
			}
		})
	}
}

func TestAuthTokenRequiresJSONAndIsHiddenFromMCP(t *testing.T) {
	app, out, stderr := testApp(t, "http://127.0.0.1:1")
	if exit := app.Run([]string{"auth", "token", "--output", "human"}); exit != ExitUsage || !strings.Contains(stderr.String(), "--output json") || out.Len() != 0 {
		t.Fatalf("exit=%d out=%s stderr=%s", exit, out, stderr)
	}
	command := app.Registry.byName["auth.token"]
	if command == nil || !command.MCPHidden || command.Supports.Field || command.Supports.JQ || command.Supports.Select {
		t.Fatal("token export must remain hidden from MCP and use a fixed output shape")
	}
}
