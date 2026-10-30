package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Reuses the FLI-695 triage fixture: one committed POST must remain a success.
func TestMCPSelectionAfterMutation(t *testing.T) {
	for _, controls := range []map[string]any{
		{"select": []any{"customer_id", "definitely_missing"}},
		{"field": "definitely_missing"},
		{"select": []any{"customer_id"}},
		{"select": []any{true}},
	} {
		t.Run(fmt.Sprint(controls), func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				if r.Method != http.MethodPost {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				writes++
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"data":{"customer_id":"cus_fixture","email":"fixture@example.com"},"request_id":"req_committed","warnings":["existing response warning"]}`)
			}))
			defer server.Close()
			// A trailing /v1 also produces an existing CLI warning to preserve.
			app, _, _ := testApp(t, server.URL+"/v1")
			result := mcpCall(t, app, defaultOptions(), "customers_create", map[string]any{"email": "fixture@example.com", "_flint": controls})
			raw, _ := json.Marshal(result)
			text := mcpResultText(result)
			invalidInput := fmt.Sprint(controls) == "map[select:[true]]"
			if invalidInput {
				if writes != 0 || result["isError"] != true {
					t.Fatalf("writes=%d result=%s", writes, raw)
				}
				return
			}
			if writes != 1 || result["isError"] == true || !strings.Contains(text, "cus_fixture") || !strings.Contains(text, "req_committed") || !strings.Contains(text, "Flint environment: sandbox") || !strings.Contains(text, "stripped trailing /v1") {
				t.Fatalf("writes=%d result=%s", writes, raw)
			}
			missing := strings.Contains(fmt.Sprint(controls), "definitely_missing")
			if missing {
				if !strings.Contains(text, "definitely_missing") || !strings.Contains(text, "mutation succeeded") || !strings.Contains(text, "fixture@example.com") || !strings.Contains(text, "existing response warning") || lookupPathValue(result, "structuredContent.data.customer_id") != "cus_fixture" {
					t.Fatal(string(raw))
				}
			} else if lookupPathValue(result, "structuredContent.value.data.customer_id") != "cus_fixture" || strings.Contains(text, "fixture@example.com") || strings.Contains(text, "selection failed") {
				t.Fatal(string(raw))
			}
		})
	}
}
