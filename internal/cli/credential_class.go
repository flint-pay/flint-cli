package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

// Select the source without opening the credential store or obtaining a workload token.
// Sources that resolve saved credentials are merchant credentials regardless of format.
func activeCredentialClass() (string, opindex.Credential) {
	source, token := opindex.CredentialLogin, ""
	switch {
	case strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN")) != "":
		source, token = opindex.CredentialAccessToken, strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN"))
	case credentialFromEnvironment() != "":
		source = opindex.CredentialAPIKey
	case workloadIdentityID() != "":
		source = opindex.CredentialWorkload
	}
	return source, opindex.ClassifyCredential(source, token)
}

func (a *App) credentialClassError(command *Command) *CLIError {
	if !command.AuthRequired || command.OperationID == "" {
		return nil
	}
	source, credential := activeCredentialClass()
	if credential.Schemes == nil {
		return nil // Unknown bearer formats are the API's decision.
	}
	index, err := a.operationIndex()
	if err != nil {
		return cliError(ExitSoftware, errorTypeInternal, "OPERATION_INDEX_FAILED", "Could not read the embedded operation index.")
	}
	entry, found := index.CredentialPolicy(command.OperationID)
	if !found || entry.Audience == "local" || entry.Audience == "keyless" || credential.Allows(entry) {
		return nil
	}
	name := "flint " + strings.ReplaceAll(command.CanonicalName, ".", " ")
	code := "MERCHANT_CREDENTIAL_REQUIRED"
	message := fmt.Sprintf("%s needs a merchant credential, but FLINT_ACCESS_TOKEN holds %s. Unset FLINT_ACCESS_TOKEN to use your saved login.", name, credentialClassLabel(source, credential))
	action := map[string]any{"reason_message": "Unset FLINT_ACCESS_TOKEN to use your saved login, or set FLINT_API_KEY to a merchant API key."}
	switch {
	case credential.Partner && !entry.PartnerAccess:
		code = "PARTNER_ACCESS_NOT_ALLOWED"
		message = name + " does not accept a partner access token. Unset FLINT_ACCESS_TOKEN to use your saved login."
	case entry.Audience == "buyer":
		code = "BUYER_CREDENTIAL_REQUIRED"
		message = fmt.Sprintf("%s acts as a buyer and needs a session credential accepted by this operation, but the active credential is %s.", name, credentialClassLabel(source, credential))
		if containsString(entry.Schemes, "CustomerSessionBearer") {
			message = fmt.Sprintf("%s acts as a buyer and needs a customer session secret, but the active credential is %s. Create a customer session with a merchant credential, then set FLINT_ACCESS_TOKEN to its secret.", name, credentialClassLabel(source, credential))
			action = map[string]any{
				"command":        `flint customer-sessions create --input '{"customer_id":"CUSTOMER_ID"}' --field secret`,
				"reason_message": "Run this command with a merchant credential, then set FLINT_ACCESS_TOKEN to the returned customer session secret.",
			}
		} else {
			action = map[string]any{"command": name + " --help", "reason_message": "Set FLINT_ACCESS_TOKEN to a buyer credential matching one of the accepted schemes."}
		}
	default:
		if !containsString(entry.Schemes, "BearerAuth") {
			message = fmt.Sprintf("%s needs a credential accepted by this operation, but the active credential is %s.", name, credentialClassLabel(source, credential))
			action = map[string]any{"command": name + " --help", "reason_message": "Use a credential matching one of the accepted schemes."}
		}
	}
	e := cliError(ExitAuth, errorTypeAuthentication, code, message)
	e.ErrorSource, e.Origin = "caller", "cli"
	e.Details = map[string]any{
		"accepted_schemes": append([]string(nil), entry.Schemes...),
		"credential":       source,
		"remediation":      map[string]any{"retry": "none", "next_actions": []any{action}},
	}
	return e
}

// Labels describe the selected source and the shared adapter's schemes, never token prefixes.
func credentialClassLabel(source string, credential opindex.Credential) string {
	switch source {
	case opindex.CredentialAPIKey:
		return "a merchant API key"
	case opindex.CredentialLogin:
		return "a saved merchant login or imported API key"
	case opindex.CredentialWorkload:
		return "a merchant workload identity"
	}
	switch {
	case credential.Partner:
		return "a partner access token"
	case containsString(credential.Schemes, "CustomerSessionBearer"):
		return "a customer session secret"
	case containsString(credential.Schemes, "CheckoutSessionBearer"):
		return "a checkout session access token"
	default:
		return "a merchant access token"
	}
}
