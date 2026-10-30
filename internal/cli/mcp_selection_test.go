package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Reuses FLI-695's committed POST fixture: a declared but absent field must
// preserve success, while unknown select paths must fail before the write.
func TestMCPSelectionAfterMutation(t *testing.T) {
	for _, test := range []struct {
		name           string
		selectFields   any
		valid, missing bool
	}{
		{"unknown field", []any{"customer_id", "definitely_missing"}, false, false},
		{"malformed field", []any{true}, false, false},
		{"identity and status retained", []any{"email"}, true, false},
		{"only identity requested", []any{"customer_id"}, true, false},
		{"declared absent optional field", []any{"phone"}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				if r.Method != http.MethodPost {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				writes++
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"data":{"customer_id":"cus_fixture","status":"active","email":"fixture@example.com"},"request_id":"req_committed","warnings":["existing response warning"]}`)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL+"/v1")
			result := mcpCall(t, app, defaultOptions(), "flint_write", map[string]any{"operation": "customers.create", "arguments": map[string]any{"email": "fixture@example.com"}, "select": test.selectFields, "idempotency_key": "selection-fixture"})
			if !test.valid {
				if writes != 0 || result["isError"] != true {
					t.Fatalf("writes=%d result=%s", writes, mcpResultText(result))
				}
				return
			}
			if writes != 1 || result["isError"] == true {
				t.Fatalf("writes=%d result=%s", writes, mcpResultText(result))
			}
			value := mcpResultJSON(t, result)
			for path, want := range map[string]any{"data.customer_id": "cus_fixture", "data.status": "active", "request_id": "req_committed"} {
				if got := lookupPathValue(value, path); got != want {
					t.Errorf("%s=%#v want=%#v", path, got, want)
				}
			}
			text := mcpResultText(result)
			if !strings.Contains(text, "Flint environment: sandbox") || !strings.Contains(text, "stripped trailing /v1") || !strings.Contains(text, "existing response warning") {
				t.Fatalf("envelope or warnings lost: %s", text)
			}
			if test.missing {
				if _, exists := lookupPath(value, "data.phone"); exists || !strings.Contains(text, "Warning: select path phone is not in this result.") {
					t.Fatalf("missing optional field: %s", text)
				}
			}
			if test.name != "identity and status retained" && strings.Contains(text, "fixture@example.com") {
				t.Fatalf("unselected email retained: %s", text)
			}
		})
	}
}
