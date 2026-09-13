package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
)

func matchPublicOperation(method, path string) (openAPIOperation, bool) {
	doc, err := loadOpenAPI()
	parsed, parseErr := url.Parse(path)
	if err != nil || parseErr != nil {
		return openAPIOperation{}, false
	}
	if operation, ok := doc.Paths[parsed.Path][strings.ToLower(method)]; ok {
		return operation, true
	}
	actual := strings.Split(parsed.Path, "/")
	bestScore := -1
	var best openAPIOperation
	for template, methods := range doc.Paths {
		parts := strings.Split(template, "/")
		if len(parts) != len(actual) {
			continue
		}
		matches := true
		score := 0
		for i, part := range parts {
			if part != actual[i] && !(strings.HasPrefix(part, "{") && actual[i] != "") {
				matches = false
				break
			}
			if !strings.HasPrefix(part, "{") {
				score++
			}
		}
		if matches {
			if operation, ok := methods[strings.ToLower(method)]; ok && score > bestScore {
				best, bestScore = operation, score
			}
		}
	}
	return best, bestScore >= 0
}

func (a *App) authenticateCommand(ctx context.Context, command *Command, opts Options, resolved ResolvedConfig) (string, string, authContextEnvelope, *CLIError) {
	envelope := authContextEnvelope{}
	token := strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN"))
	if strings.HasPrefix(token, "flint_test_") || strings.HasPrefix(token, "flint_live_") {
		return "", "", envelope, configError("API_KEY_IN_ACCESS_TOKEN", "Set FLINT_API_KEY for an API key. FLINT_ACCESS_TOKEN is for session and partner tokens.", nil)
	}
	checkoutSession := false
	for _, requirement := range command.Security {
		if _, ok := requirement["CheckoutSessionSecretHeader"]; ok && os.Getenv("FLINT_CHECKOUT_SESSION_SECRET") != "" {
			checkoutSession = true
		}
	}
	if checkoutSession && os.Getenv("FLINT_CHECKOUT_SESSION_ID") == "" {
		return "", "", envelope, configError("CHECKOUT_SESSION_ID_REQUIRED", "Set FLINT_CHECKOUT_SESSION_ID with FLINT_CHECKOUT_SESSION_SECRET.", nil)
	}
	if !command.AuthRequired || token != "" || checkoutSession {
		mode := "sandbox"
		if opts.Live {
			mode = "live"
		}
		baseKey := "flint_test_"
		if opts.Live {
			baseKey = "flint_live_"
		}
		baseURL, err := a.baseURLForCredential(baseKey)
		if err != nil {
			return "", "", envelope, configError("INVALID_BASE_URL", err.Error(), err)
		}
		if !command.AuthRequired {
			token = ""
		}
		// Session tokens cannot use the API-key introspection endpoint. The
		// selected route validates the token and its resource authorization.
		if (token != "" || checkoutSession) && resolved.SandboxGuard != "" {
			return "", "", envelope, configError("SESSION_SANDBOX_GUARD_UNSUPPORTED", "Session credentials cannot verify a configured sandbox guard. Use an API key for guarded merchant commands.", nil)
		}
		if (token != "" || checkoutSession) && resolved.MerchantGuard != "" {
			return "", "", envelope, configError("SESSION_MERCHANT_GUARD_UNSUPPORTED", "Session credentials cannot verify a configured merchant guard. Use an API key for guarded merchant commands.", nil)
		}
		if command.CanonicalName == "auth.status" {
			authCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
			defer cancel()
			actual, authErr := a.fetchAuthContextResponse(authCtx, baseURL, token, opts.Debug)
			return token, baseURL, actual, authErr
		}
		envelope.Data = AuthContext{Environment: mode, AuthType: "session"}
		if token != "" || checkoutSession {
			digest := sha256.Sum256([]byte(token + "\x00" + os.Getenv("FLINT_CHECKOUT_SESSION_ID") + "\x00" + os.Getenv("FLINT_CHECKOUT_SESSION_SECRET")))
			envelope.Data.CredentialScope = hex.EncodeToString(digest[:])
		}
		return token, baseURL, envelope, nil
	}
	key, _, err := a.resolveCredential(resolved.ProfileName)
	if err != nil {
		return "", "", envelope, configError("CREDENTIAL_LOOKUP_FAILED", "The credential could not be read from the OS keychain.", err)
	}
	if key == "" {
		e := configError("API_KEY_REQUIRED", "No Flint credential is configured. Run flint auth import for an API key, flint signup to create an account, or set FLINT_ACCESS_TOKEN for a session token.", nil)
		e.Details = map[string]any{"remediation": map[string]any{"next_actions": []any{map[string]any{"command": "flint auth import", "url": dashboardAPIKeysURL}, map[string]any{"command": "flint signup"}}}}
		return "", "", envelope, e
	}
	baseURL, err := a.baseURLForCredential(key)
	if err != nil {
		return "", "", envelope, configError("INVALID_CREDENTIAL", err.Error(), err)
	}
	if opts.DryRun == "client" {
		mode, err := credentialEnvironment(key)
		if err != nil {
			return "", "", envelope, configError("INVALID_CREDENTIAL", err.Error(), err)
		}
		envelope.Data = AuthContext{Environment: mode, MerchantID: resolved.MerchantID, APIKeyID: resolved.APIKeyID, SandboxID: resolved.SandboxID}
	} else {
		authCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
		var authErr *CLIError
		envelope, authErr = a.fetchAuthContextResponse(authCtx, baseURL, key, opts.Debug)
		if authErr != nil {
			return "", "", envelope, authErr
		}
	}
	return key, baseURL, envelope, nil
}
