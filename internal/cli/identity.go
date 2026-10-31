package cli

import (
	"os"
	"strings"
)

type cliIdentity struct {
	APIBaseURL       string `json:"api_base_url"`
	CredentialSource string `json:"credential_source"`
	Profile          string `json:"profile"`
	ProfileSource    string `json:"profile_source"`
}

type authStatusData struct {
	AuthContext
	CLI cliIdentity `json:"cli"`
}

// Sources describe the credential the CLI selected, never its secret value.
func (a *App) identityInfo(resolved ResolvedConfig, baseURL, source string) cliIdentity {
	if baseURL == "" {
		baseURL, _ = a.baseURLForCredential("flint_test_")
	}
	if source == "environment" {
		source = "environment_api_key"
	}
	return cliIdentity{APIBaseURL: baseURL, CredentialSource: source, Profile: resolved.ProfileName, ProfileSource: resolved.Sources["profile"]}
}

func (a *App) identityCredentialSource(resolved ResolvedConfig, auth AuthContext) string {
	switch {
	case strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN")) != "":
		return "environment_access_token"
	case credentialFromEnvironment() != "":
		return "environment_api_key"
	case workloadIdentityID() != "":
		return "workload_identity"
	case auth.isCLILogin():
		return "oauth_session"
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return "unavailable"
	}
	p, exists := cfg.Profiles[resolved.ProfileName]
	if !exists || p.CredentialPresent != nil && !*p.CredentialPresent {
		return "none"
	}
	if a.selectedCredentialStore(resolved.ProfileName, p) == "file" {
		return "file"
	}
	return "keychain"
}

func identityOutputProperties() map[string]any {
	properties := map[string]any{}
	for _, field := range []string{"api_base_url", "credential_source", "profile", "profile_source"} {
		properties[field] = map[string]any{"type": "string"}
	}
	return map[string]any{"cli": map[string]any{"type": "object", "properties": properties}, "mode": map[string]any{"type": "string"}, "access": map[string]any{"type": "string"}}
}
