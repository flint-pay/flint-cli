package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBuyerCreditNoteTopLevelRoutes(t *testing.T) {
	const invoiceID = "inv_01KEZF7C00A5GEBMWHEAJFSAR4"
	for _, tc := range []struct {
		name      string
		args      []string
		path      string
		invoiceID string
		pdf       bool
	}{
		{name: "all invoices", args: []string{"me", "credit-notes", "list"}, path: "/v1/me/credit-notes"},
		{name: "invoice filter", args: []string{"me", "credit-notes", "list", "--invoice-id", invoiceID}, path: "/v1/me/credit-notes", invoiceID: invoiceID},
		{name: "get", args: []string{"me", "credit-notes", "get", "cn_test"}, path: "/v1/me/credit-notes/cn_test"},
		{name: "pdf", args: []string{"me", "credit-notes", "pdf", "get", "cn_test"}, path: "/v1/me/credit-notes/cn_test/pdf", pdf: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			pdf := []byte("%PDF-1.7\ncredit note\n%%EOF")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != tc.path {
					t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, tc.path)
				}
				if r.URL.Query().Get("invoice_id") != tc.invoiceID {
					t.Error("invoice filter was not preserved")
				}
				if r.Header.Get("Authorization") != "Bearer flint_cses_test" {
					t.Error("buyer session credential was not forwarded")
				}
				if tc.pdf {
					if r.Header.Get("Accept") != "application/pdf" {
						t.Error("PDF accept header missing")
					}
					w.Header().Set("Content-Type", "application/pdf")
					_, _ = w.Write(pdf)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"data":[]}`)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			t.Setenv("FLINT_ACCESS_TOKEN", "flint_cses_test")
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
			app.LoadCredential = func(string) (string, error) { t.Error("unexpected keychain lookup"); return "", nil }
			args := append([]string{}, tc.args...)
			destination := filepath.Join(t.TempDir(), "credit-note.pdf")
			if tc.pdf {
				args = append(args, "--save-to", destination)
			}
			if code := app.Run(append(args, "--output", "json")); code != ExitOK {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, out, stderr)
			}
			if calls != 1 {
				t.Fatalf("calls=%d, want one request to the top-level collection", calls)
			}
			if tc.pdf {
				data, err := os.ReadFile(destination)
				if err != nil || string(data) != string(pdf) {
					t.Fatalf("PDF download differs: %v", err)
				}
			}
		})
	}
}

func TestRetiredBuyerInvoiceCreditNoteCommandsMakeNoRequests(t *testing.T) {
	for _, args := range [][]string{
		{"me", "invoices", "credit-notes", "list", "inv_test"},
		{"me", "invoices", "credit-notes", "get", "inv_test", "cn_test"},
		{"me", "invoices", "credit-notes", "pdf", "get", "inv_test", "cn_test"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("retired command made an HTTP request") }))
		app, out, _ := testApp(t, server.URL)
		exit, result := runJSON(t, app, out, args...)
		if code, _ := lookupPath(result, "error.code"); exit != ExitUsage || code != "UNKNOWN_COMMAND" {
			t.Fatalf("exit=%d result=%v", exit, result)
		}
		server.Close()
	}
}
