package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An access token's environment is invisible locally. Live OAuth tokens must
// get the same gates as live API keys, and tokens the API will not describe
// must not be treated as sandbox for sensitive writes.
func TestAccessTokenWritesUseIntrospectedOrUnverifiedEnvironment(t *testing.T) {
	createIntent := []string{"payment-intents", "create", "--idempotency-key", "saved-test-action-1", "--amount", "500", "--currency", "USD", "--payment-options", "card", "--output", "json"}
	for _, scenario := range []struct {
		name       string
		authStatus int
		authBody   string
		args       []string
		wantExit   int
		wantCode   string
		wantWrite  bool
	}{
		{"live oauth token needs --live", 200, `{"data":{"auth_type":"oauth","environment":"live","merchant_id":"mer_1","scopes":["payments.payment_intents.write"]}}`, createIntent, ExitAuth, "LIVE_ACKNOWLEDGEMENT_REQUIRED", false},
		{"live oauth token needs confirmation", 200, `{"data":{"auth_type":"oauth","environment":"live","merchant_id":"mer_1","scopes":["payments.payment_intents.write"]}}`, append(append([]string{}, createIntent...), "--live"), ExitConfirmation, "PRODUCTION_CONFIRMATION_REQUIRED", false},
		{"sandbox oauth token writes", 200, `{"data":{"auth_type":"oauth","environment":"sandbox","merchant_id":"mer_1","scopes":["payments.payment_intents.write"]}}`, createIntent, ExitOK, "", true},
		{"unverifiable token needs confirmation", 401, `{"error":{"type":"authentication_error","code":"UNAUTHORIZED","message":"no"}}`, createIntent, ExitConfirmation, "UNVERIFIED_ENVIRONMENT_CONFIRMATION_REQUIRED", false},
		{"unverifiable token confirmed", 401, `{"error":{"type":"authentication_error","code":"UNAUTHORIZED","message":"no"}}`, append(append([]string{}, createIntent...), "--confirm"), ExitOK, "", true},
		{"unverifiable token ordinary write", 401, `{"error":{"type":"authentication_error","code":"UNAUTHORIZED","message":"no"}}`, []string{"customers", "create", "--email", "a@example.com", "--output", "json"}, ExitOK, "", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			wrote := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer partner_token" {
					t.Errorf("Authorization = %q", got)
				}
				if r.URL.Path == "/v1/auth-context" {
					w.WriteHeader(scenario.authStatus)
					fmt.Fprint(w, scenario.authBody)
					return
				}
				if r.Method == http.MethodPost {
					wrote = true
				}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"data":{"id":"x"}}`)
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			t.Setenv("FLINT_ACCESS_TOKEN", "partner_token")
			t.Setenv("FLINT_API_KEY", "")
			app.LoadCredential = func(string) (string, error) { t.Error("unexpected keychain lookup"); return "", nil }
			exit := app.Run(scenario.args)
			if exit != scenario.wantExit {
				t.Fatalf("exit=%d want %d: %s", exit, scenario.wantExit, out)
			}
			if wrote != scenario.wantWrite {
				t.Fatalf("write sent=%v want %v", wrote, scenario.wantWrite)
			}
			if scenario.wantCode != "" {
				var envelope struct {
					Error struct{ Code string } `json:"error"`
				}
				if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &envelope); err != nil || envelope.Error.Code != scenario.wantCode {
					t.Fatalf("code=%q want %q: %s", envelope.Error.Code, scenario.wantCode, out)
				}
			}
		})
	}
}
