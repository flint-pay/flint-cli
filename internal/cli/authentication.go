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

// Resolve raw API safety metadata before both flag validation and execution.
// OAuth authorization creates a grant even though its HTTP method is GET.
func resolveAPICommand(command *Command, opts Options) *Command {
	if command.CanonicalName != "api" || len(opts.Positionals) < 2 {
		return command
	}
	resolved := *command
	resolved.Method = strings.ToUpper(opts.Positionals[0])
	resolved.APIPath = opts.Positionals[1]
	resolved.Mutation = resolved.Method != "GET" && resolved.Method != "HEAD"
	resolved.Destructive = resolved.Method == "DELETE"
	if operation, ok := matchPublicOperation(resolved.Method, resolved.APIPath); ok {
		resolved.OperationID = operation.OperationID
		resolved.Security = operation.Security
		resolved.AuthRequired = len(operation.Security) > 0
		resolved.Mutation = resolved.Mutation || operation.OperationID == "authorizePartnerInstall"
		resolved.Destructive = resolved.Destructive || operation.FlintDestructive
		metadata := &Command{OperationID: operation.OperationID}
		applyPublicOperationMetadata(metadata)
		resolved.ResponseMediaType = metadata.ResponseMediaType
	}
	// Raw writes retain their conservative confirmation policy.
	resolved.Sensitive = resolved.Mutation
	return &resolved
}

func oauthAuthorizationEnvironment(command *Command, opts Options) string {
	if command.OperationID != "authorizePartnerInstall" {
		return ""
	}
	modes := opts.Raw["mode"]
	if command.CanonicalName == "api" {
		if parsed, err := url.Parse(command.APIPath); err == nil {
			modes = parsed.Query()["mode"]
		}
	}
	environment := ""
	for _, mode := range modes {
		// Repeated query parameters must not hide a live request behind a
		// later test value; API validation decides whether duplicates are valid.
		if mode == "live" {
			return "live"
		}
		if mode == "test" {
			environment = "sandbox"
		}
	}
	return environment
}

func (a *App) authenticateCommand(ctx context.Context, command *Command, opts Options, resolved ResolvedConfig) (string, string, authContextEnvelope, *CLIError) {
	ctx = withOAuthContext(ctx, resolved.ContextID)
	envelope := authContextEnvelope{}
	if resolved.ContextID != "" && (credentialFromEnvironment() != "" || os.Getenv("FLINT_ACCESS_TOKEN") != "" || os.Getenv("FLINT_CHECKOUT_SESSION_SECRET") != "") {
		return "", "", envelope, contextSessionRequired()
	}
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
		if requested := oauthAuthorizationEnvironment(command, opts); requested != "" {
			mode = requested
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
		e := configError("API_KEY_REQUIRED", "No Flint credential is configured. Run flint auth login (recommended for local development), or flint auth import to use an API key. For automation, set FLINT_API_KEY.", nil)
		e.Details = map[string]any{"remediation": map[string]any{"next_actions": []any{map[string]any{"command": "flint auth login"}, map[string]any{"command": "flint auth import", "url": dashboardAPIKeysURL}, map[string]any{"command": "flint signup"}}}}
		return "", "", envelope, e
	}
	if isOAuthCredential(key) {
		if opts.DryRun == "client" {
			c, e := decodeOAuthCredential(key)
			if e != nil {
				return "", "", envelope, e
			}
			if e := a.oauthBaseURL(c); e != nil {
				return "", "", envelope, e
			}
			if resolved.ContextID != "" && (c.Version != 2 || c.ContextID != resolved.ContextID) {
				return "", "", envelope, configError("CONTEXT_NOT_CACHED", "This context is not cached. Run flint auth status --context "+resolved.ContextID+" first.", nil)
			}
			if c.PendingValidation {
				return "", "", envelope, configError("OAUTH_CONTEXT_UNAVAILABLE", "The saved token needs validation. Run flint auth status first.", nil)
			}
			envelope.Data = c.Auth
			envelope.Data.SelectedContext = c.Version == 2
			envelope.Data.CredentialScope = oauthHistoryScope(c.Auth)
			return c.AccessToken, c.BaseURL, envelope, nil
		}
		authCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
		return a.fetchCredentialContext(authCtx, resolved.ProfileName, key, opts.Debug)
	}
	if resolved.ContextID != "" {
		return "", "", envelope, contextSessionRequired()
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
