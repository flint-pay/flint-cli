package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryEnvironmentKeyOverridesSavedContext(t *testing.T) {
	for _, savedEnvironment := range []string{"live", "sandbox"} {
		t.Run(savedEnvironment, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/developer/auth-context" {
					fmt.Fprint(w, `{"data":{"environment":"sandbox","merchant_id":"mer_current","sandbox_id":"test_current"}}`)
					return
				}
				fmt.Fprint(w, `{"data":{"customer_id":"cus_current"}}`)
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			if err := app.saveConfig(Config{DefaultProfile: "default", Profiles: map[string]Profile{"default": {Environment: savedEnvironment, MerchantID: "mer_old", SandboxID: "test_old"}}}); err != nil {
				t.Fatal(err)
			}
			if err := app.recordHistory(map[string]any{"data": map[string]any{"customer_id": "cus_old"}}, "customers.get", historyScope{Profile: "default", Environment: "live", MerchantID: "mer_old"}); err != nil {
				t.Fatal(err)
			}
			for _, argv := range [][]string{{"customers", "get", "cus_current", "--output", "json"}, {"customers", "get", "@last", "--output", "json"}} {
				out.Reset()
				if exit := app.Run(argv); exit != ExitOK || !strings.Contains(out.String(), "cus_current") {
					t.Fatalf("%v exit=%d: %s", argv, exit, out)
				}
			}
			// History must still work offline after authenticated resource operations.
			server.Close()
			out.Reset()
			if exit := app.Run([]string{"history", "--output", "json"}); exit != ExitOK || !strings.Contains(out.String(), "cus_current") || strings.Contains(out.String(), "cus_old") {
				t.Fatalf("history exit=%d: %s", exit, out)
			}
			// Explicit guards still restrict the offline view, unlike stale metadata.
			projectDir := filepath.Join(app.WorkingDir, ".flint")
			if err := os.MkdirAll(projectDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectDir, "config.json"), []byte(`{"merchant":"mer_current","sandbox":"test_other"}`), 0600); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if exit := app.Run([]string{"history", "--output", "json"}); exit != ExitOK || strings.Contains(out.String(), "cus_current") {
				t.Fatalf("guarded history exit=%d: %s", exit, out)
			}
		})
	}
}
