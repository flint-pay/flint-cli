package opindex

import "strings"

const (
	CredentialAPIKey      = "api_key"
	CredentialLogin       = "cli_login"
	CredentialWorkload    = "workload_identity"
	CredentialAccessToken = "access_token"
)

type Credential struct {
	Schemes []string
	Partner bool
}

// Prefixes live here only. Unknown access tokens intentionally remain unclassified.
var credentialPrefixes = []struct {
	Prefix, Scheme string
	Partner        bool
}{
	{"fcat_", "BearerAuth", false}, {"fwat_", "BearerAuth", false},
	{"fpat_v1.", "BearerAuth", true}, {"ckat_v1.", "CheckoutSessionBearer", false},
	{"flint_cses_", "CustomerSessionBearer", false},
}

func ClassifyCredential(source, token string) Credential {
	switch source {
	case CredentialAPIKey, CredentialLogin, CredentialWorkload, "FLINT_API_KEY", "imported_key", "saved_login":
		return Credential{Schemes: []string{"BearerAuth"}}
	case CredentialAccessToken, "FLINT_ACCESS_TOKEN":
		for _, row := range credentialPrefixes {
			if strings.HasPrefix(token, row.Prefix) {
				return Credential{[]string{row.Scheme}, row.Partner}
			}
		}
	}
	return Credential{}
}

func CredentialSchemes(source, token string) []string {
	return ClassifyCredential(source, token).Schemes
}

// Allows is a credential-class check, not an authorization or scope grant.
func (credential Credential) Allows(entry Entry) bool {
	if entry.Audience == "flint_hosted" {
		return false
	}
	if entry.Audience == "local" || credential.Schemes == nil {
		return true
	}
	if credential.Partner && !entry.PartnerAccess {
		return false
	}
	for _, accepted := range entry.Schemes {
		for _, scheme := range credential.Schemes {
			if accepted == scheme {
				return true
			}
		}
	}
	return false
}

func (ix *Index) MissingScopes(entry Entry, granted []string, known bool) []string {
	if !known || entry.ScopeRequirement == nil {
		return nil
	}
	held := map[string]bool{}
	for _, scope := range granted {
		held[scope] = true
	}
	var satisfies func(string, map[string]bool) bool
	satisfies = func(scope string, seen map[string]bool) bool {
		if held[scope] {
			return true
		}
		if seen[scope] {
			return false
		}
		seen[scope] = true
		for _, parent := range ix.impliedBy[scope] {
			if satisfies(parent, seen) {
				return true
			}
		}
		return false
	}
	missing := []string{}
	for _, scope := range entry.ScopeRequirement.Scopes {
		if satisfies(scope, map[string]bool{}) {
			if entry.ScopeRequirement.Mode == "any" {
				return nil
			}
		} else {
			missing = append(missing, scope)
		}
	}
	return missing
}

// CredentialPolicy returns HTTP credential requirements independently of local
// discovery exposure. Bound CLI commands and raw routes use the same policy.
func (ix *Index) CredentialPolicy(operationID string) (Entry, bool) {
	entry, ok := ix.credentialPolicies[operationID]
	return entry, ok
}

// OperationSafety includes routes omitted from CLI/MCP discovery, so raw API
// calls use exactly the same classification as exposed operations.
func (ix *Index) OperationSafety(operationID string) (Entry, bool) {
	return ix.CredentialPolicy(operationID)
}
