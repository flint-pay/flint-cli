package cli

import (
	"context"
	"os"
	"strings"
	"time"
)

// Token export deliberately bypasses the normal result renderer and history.
// Only this command's explicit JSON output may contain a bearer credential.
func (a *App) localAuthToken(_ *Command, opts Options) int {
	if opts.Output != "json" {
		return a.fail(usageError("JSON_OUTPUT_REQUIRED", "Token export requires --output json. Keep the output private; it contains an access token.", "output"), opts)
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	ctx, cancel := context.WithTimeout(withOAuthContext(a.commandContext(), resolved.ContextID), opts.Timeout)
	defer cancel()
	raw, _, err := a.resolveCheckCredential(resolved.ProfileName)
	if err != nil {
		return a.fail(credentialStoreError("CREDENTIAL_LOOKUP_FAILED", "The credential could not be read from the selected credential store.", err), opts)
	}
	if raw == "" {
		return a.fail(configError("API_KEY_REQUIRED", missingCredentialMessage(resolved.ProfileName), nil), opts)
	}
	if strings.HasPrefix(raw, "flint_test_") || strings.HasPrefix(raw, "flint_live_") || (!isOAuthCredential(raw) && raw != workloadCredential && strings.TrimSpace(raw) != strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN"))) {
		return a.fail(configError("API_KEY_NOT_EXPORTABLE", "API keys cannot be exported. Run flint auth login, or configure workload identity for a short-lived access token.", nil), opts)
	}
	token, base, envelope, e := a.fetchCredentialContext(ctx, opts.Timeout, resolved.ProfileName, raw, false)
	if e != nil {
		return a.fail(e, opts)
	}
	if envelope.Data.AuthType != "oauth" && envelope.Data.AuthType != "workload_identity" {
		return a.fail(configError("TOKEN_NOT_EXPORTABLE", "Only OAuth and workload identity access tokens can be exported.", nil), opts)
	}
	if e := validateResolvedCredentialIntent(envelope.Data, opts, resolved, EnvironmentAffinityAccount); e != nil {
		return a.fail(e, opts)
	}
	var expires time.Time
	if raw == workloadCredential {
		cfg, e := a.federationConfig(base)
		if e != nil {
			return a.fail(e, opts)
		}
		federationCache.Lock()
		cached := federationCache.tokens[cfg]
		federationCache.Unlock()
		if cached.Value == "" || !a.Now().Before(cached.Expires) {
			return a.fail(configError("FEDERATION_EXCHANGE_FAILED", "The workload identity token expired during validation. Retry the command.", nil), opts)
		}
		token, expires = cached.Value, cached.Expires
	} else if isOAuthCredential(raw) {
		// Refresh may have rotated the saved credential during validation. Use
		// its expiry only when it still names the token being exported.
		if saved, err := a.loadSavedCredential(resolved.ProfileName); err == nil {
			if c, e := decodeOAuthCredential(saved); e == nil && c.AccessToken == token {
				expires = c.ExpiresAt
			}
		}
	} else if envelope.Data.ExpiresAt != nil {
		expires, _ = time.Parse(time.RFC3339, *envelope.Data.ExpiresAt)
	}
	result := map[string]any{"access_token": token, "token_type": "Bearer"}
	if !expires.IsZero() {
		result["expires_at"] = expires.UTC().Format(time.RFC3339)
	}
	if err := writeJSON(a.Stdout, result); err != nil {
		return a.fail(cliError(ExitSoftware, errorTypeInternal, "OUTPUT_FAILED", "Could not write the access token."), opts)
	}
	return ExitOK
}
