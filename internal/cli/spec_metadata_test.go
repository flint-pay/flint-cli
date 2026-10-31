package cli

import (
	"github.com/flint-pay/flint-cli/internal/opindex"
	"strings"
	"testing"
)

func TestSpecMetadataAuthoritative(t *testing.T) {
	app := &App{Registry: NewRegistry()}
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	marked := 0
	for path, methods := range doc.Paths {
		for method, op := range methods {
			entry, found := index.OperationSafety(op.OperationID)
			if !found || entry.Irreversible != op.FlintDestructive {
				t.Errorf("%s %s: index=%+v spec=%v", method, path, entry, op.FlintDestructive)
			}
			if op.FlintDestructive {
				marked++
			}
			if op.FlintAudience == "flint_hosted" {
				for _, source := range []string{opindex.CredentialAPIKey, opindex.CredentialAccessToken, opindex.CredentialLogin, opindex.CredentialWorkload} {
					if opindex.ClassifyCredential(source, "unknown").Allows(entry) {
						t.Errorf("%s exposed to %s", op.OperationID, source)
					}
				}
			}
			if strings.ToUpper(method) == "DELETE" && !op.FlintDestructive && terminalConfirmationRequired(entry, "sandbox") {
				t.Errorf("DELETE fallback for %s", op.OperationID)
			}
		}
	}
	if marked != 117 {
		t.Errorf("published destructive count=%d", marked)
	}
	for _, name := range []string{"orders.pay", "payment-intents.confirm", "payment-intents.capture", "invoices.collect"} {
		entry, found := index.Lookup(name)
		if !found {
			continue
		}
		if !entry.Irreversible || !entry.ConfirmRequired() || terminalConfirmationRequired(entry, "sandbox") || !terminalConfirmationRequired(entry, "live") || !terminalConfirmationRequired(entry, environmentUnverified) {
			t.Errorf("collection policy %s: %+v", name, entry)
		}
	}
}
