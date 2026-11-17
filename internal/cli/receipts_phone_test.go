package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// FLI-2476: the pinned snapshot must include the receipts settings section.
func TestSettingsUpdateAcceptsReceipts(t *testing.T) {
	app, out, errOut := testApp(t, "http://127.0.0.1:1")
	exit := app.Run([]string{"settings", "update", "--input", `{"receipts":{"footer_text":"Thank you for your gift."}}`, "--dry-run=client", "--output", "json"})
	if exit != ExitOK || strings.Contains(out.String()+errOut.String(), "field is not allowed") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, errOut)
	}
}

func TestSettingsGetSelectsReceipts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		fmt.Fprint(w, `{"data":{"receipts":{"footer_text":"Thank you for your gift."}},"request_id":"req_00000000000000000000000001"}`)
	}))
	defer server.Close()
	app, out, errOut := testApp(t, server.URL)
	exit := app.Run([]string{"settings", "get", "--select", "receipts", "--output", "json"})
	if exit != ExitOK || strings.Contains(out.String()+errOut.String(), "isn't a field") || !strings.Contains(out.String(), "Thank you for your gift.") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, errOut)
	}
}

// The customer phone flags state the E.164 rule Flint's API publishes (FLI-2481).
func TestCustomerPhoneHelpStatesE164Rule(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	for _, name := range []string{"customers.create", "customers.update"} {
		cmd, ok := app.Registry.ByName(name)
		if !ok {
			t.Fatalf("%s is missing", name)
		}
		found := false
		for _, arg := range cmd.Arguments {
			if arg.Flag != "--phone" {
				continue
			}
			found = true
			for _, want := range []string{"E.164", "+14155551234", "trims surrounding whitespace"} {
				if !strings.Contains(arg.Description, want) {
					t.Errorf("%s --phone help lacks %q: %s", name, want, arg.Description)
				}
			}
		}
		if !found {
			t.Errorf("%s has no --phone flag", name)
		}
	}
}
