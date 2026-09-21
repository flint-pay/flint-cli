package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

func (a *App) localAuthLogin(cmd *Command, opts Options) int {
	for _, name := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_CHECKOUT_SESSION_SECRET"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return a.fail(configError("ENVIRONMENT_CREDENTIAL_ACTIVE", "Unset "+name+" before browser login; it would override the saved credential.", nil), opts)
		}
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	previousRaw, err := a.LoadCredential(resolved.ProfileName)
	if err != nil {
		return a.fail(configError("CREDENTIAL_LOOKUP_FAILED", "Could not read the existing profile credential before login.", err), opts)
	}
	baseURL := a.BaseURL
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("FLINT_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}
	baseURL, err = validateBaseURL(baseURL)
	if err != nil {
		return a.fail(configError("INVALID_BASE_URL", err.Error(), err), opts)
	}
	timeout := 10 * time.Minute
	if len(opts.Raw["timeout"]) > 0 {
		timeout = opts.Timeout
	}
	ctx, cancel := context.WithTimeout(a.commandContext(), timeout)
	defer cancel()
	reauthorize := cmd.CanonicalName == "auth.reauth"
	requestedScopes, scopeErr := requestedCLIScopes(opts)
	if scopeErr != nil {
		return a.fail(scopeErr, opts)
	}
	var previous oauthCredential
	if isOAuthCredential(previousRaw) {
		var e *CLIError
		previous, e = decodeOAuthCredential(previousRaw)
		if e != nil && !booleanRawOption(opts, "new-session") {
			return a.fail(e, opts)
		}
		if e != nil {
			previous = oauthCredential{}
		} // Never reuse unvalidated issuer/token metadata for cleanup.
		if e == nil {
			if e = a.oauthBaseURL(previous); e != nil {
				return a.fail(e, opts)
			}
			baseURL = previous.BaseURL
			if !reauthorize && !booleanRawOption(opts, "new-session") {
				_, _, envelope, e := a.fetchCredentialContext(withOAuthContext(ctx, resolved.ContextID), resolved.ProfileName, previousRaw, false)
				expired := e != nil && e.Code == "OAUTH_SESSION_EXPIRED"
				// A legacy grant has only one context. An authoritative rejection
				// of its cached token needs a new login, even before local expiry.
				if previous.Version == 1 && e != nil && e.Code == "OAUTH_SESSION_INVALID" {
					expired = true
				}
				if e != nil && e.Code == "CONTEXT_ACCESS_DENIED" && previous.Version == 2 {
					// A rejected access token can mean either a removed context or
					// a revoked session. Check the non-consuming session endpoint
					// before replacing the login.
					_, sessionErr := a.listOAuthContexts(withOAuthIdentity(ctx, previous), resolved.ProfileName)
					expired = sessionErr != nil && sessionErr.Code == "invalid_grant"
					if sessionErr != nil && !expired {
						e = sessionErr
					}
				}
				if e != nil && !expired {
					e.Message += " Use flint login --new-session to replace this login."
					return a.fail(e, opts)
				}
				if !expired {
					if len(requestedScopes) > 0 {
						return a.fail(usageError("SCOPES_REQUIRE_REAUTH", "A browser session is already active. Use flint reauth with the requested --scope values, or add --new-session to replace it.", "scope"), opts)
					}
					if e := validateCredentialIntent(envelope.Data, opts, resolved.MerchantGuard, resolved.SandboxGuard); e != nil {
						return a.fail(e, opts)
					}
					next := "flint context list or flint reauth"
					if previous.Version == 1 {
						next = "flint doctor"
					}
					return a.outputLocal(map[string]any{"data": map[string]any{"authenticated": true, "already_authenticated": true, "active_context": envelope.Data, "next": next}}, cmd, opts)
				}
				fmt.Fprintln(a.Stderr, "Your saved Flint session expired or was revoked. Sign in again to continue.")
			}
		}
	}
	if reauthorize && previous.Kind != "oauth" {
		return a.fail(configError("LOGIN_REQUIRED", "Run flint login before reauthorizing a session.", nil), opts)
	}
	if reauthorize && opts.ContextID != "" && previous.Version != 2 {
		return a.fail(contextSessionRequired(), opts)
	}
	// JSON output implicitly disables terminal input elsewhere. This flow reads
	// no terminal input, so only an explicit automation setting blocks approval.
	noInputEnv := strings.TrimSpace(os.Getenv("FLINT_NO_INPUT"))
	if booleanRawOption(opts, "no-input") || noInputEnv == "1" || strings.EqualFold(noInputEnv, "true") {
		return a.fail(usageError("LOGIN_REQUIRES_APPROVAL", "Browser login requires your approval on the website. For automation, use FLINT_API_KEY or flint auth import --stdin.", "no-input"), opts)
	}
	mode := "sandbox"
	if opts.Live {
		mode = "live"
	}
	scopes := append([]string(nil), initialCLIScopes...)
	additive := reauthorize && previous.Version == 2 && len(requestedScopes) > 0
	if additive {
		scopes = requestedScopes
	} else {
		scopes = appendUniqueStrings(scopes, requestedScopes...)
	}
	body := url.Values{"client_id": {oauthClientID}, "scope": {strings.Join(scopes, " ")}, "session_mode": {"contexts"}}
	if additive {
		body.Set("scope_mode", "additive")
	} else {
		body.Set("environment", mode)
		if resolved.MerchantGuard != "" {
			body.Set("merchant_id", resolved.MerchantGuard)
		}
		if resolved.SandboxGuard != "" {
			body.Set("sandbox_id", resolved.SandboxGuard)
		}
	}
	path := loginDevicePath
	if reauthorize && previous.Version == 2 {
		path = oauthReauthorizePath
		if resolved.ContextID == "" {
			resolved.ContextID = previous.ContextID
		}
		body.Set("context_id", resolved.ContextID)
	}
	var response []byte
	var e *CLIError
	if path == oauthReauthorizePath {
		locked := *a
		locked.Context = ctx
		err := locked.withConfigLock(func() error {
			raw, err := a.LoadCredential(resolved.ProfileName)
			if err != nil {
				return err
			}
			current, decodeErr := decodeOAuthCredential(raw)
			if decodeErr != nil {
				return decodeErr
			}
			if current.SessionID != previous.SessionID || current.BaseURL != baseURL {
				return fmt.Errorf("the session changed")
			}
			body.Set("refresh_token", current.RefreshToken)
			response, e = a.oauthRequest(ctx, baseURL, path, body)
			return nil
		})
		if err != nil {
			return a.fail(configError("OAUTH_SESSION_CHANGED", "Could not start reauthorization for the saved session.", err), opts)
		}
	} else {
		response, e = a.oauthRequest(ctx, baseURL, path, body)
	}
	if e != nil {
		if reauthorize && e.Code == "LOGIN_UNAVAILABLE" {
			return a.fail(contextEndpointError(e), opts)
		}
		return a.fail(loginRequestError(ctx, e), opts)
	}
	var device struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	if json.Unmarshal(response, &device) != nil {
		return a.fail(invalidResponseError("INVALID_LOGIN_RESPONSE", "Flint returned an invalid browser login response.", nil), opts)
	}
	if device.Interval == 0 {
		device.Interval = 5
	}
	target, valid := loginVerificationURL(device.VerificationURI, device.VerificationURIComplete, baseURL, device.UserCode)
	if !valid || device.DeviceCode == "" || (strings.Contains(target, device.DeviceCode) || strings.Contains(device.VerificationURI, device.DeviceCode) || strings.Contains(device.UserCode, device.DeviceCode)) || len(device.DeviceCode) > 4096 || device.ExpiresIn < 1 || device.ExpiresIn > 1800 || device.Interval < 1 || device.Interval > 60 {
		return a.fail(invalidResponseError("INVALID_LOGIN_RESPONSE", "Flint returned invalid browser login details. Run flint auth import instead.", nil), opts)
	}
	pollCtx, stopPolling := context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
	defer stopPolling()
	if _, err := fmt.Fprintf(a.Stderr, "Open %s\nConfirm this code on the website: %s\nWaiting for approval…\n", device.VerificationURI, device.UserCode); err != nil {
		return a.fail(networkError("OUTPUT_WRITE_FAILED", "Could not display browser login instructions.", err), opts)
	}
	if !booleanRawOption(opts, "no-open") {
		opener := a.OpenBrowser
		if opener == nil {
			opener = openBrowser
		}
		if err := opener(target); err != nil {
			fmt.Fprintln(a.Stderr, "Could not open the browser. Open the link above to continue.")
		}
	}
	pollBody := url.Values{"client_id": {oauthClientID}, "grant_type": {deviceGrantType}, "device_code": {device.DeviceCode}}
	interval := time.Duration(device.Interval) * time.Second
	for {
		if !sleepContext(pollCtx, interval) {
			return a.fail(loginRequestError(pollCtx, nil), opts)
		}
		completed := false
		result := ExitOK
		attempt := func() error {
			if reauthorize && previous.Version == 2 {
				raw, err := a.LoadCredential(resolved.ProfileName)
				if err != nil {
					return err
				}
				current, e := decodeOAuthCredential(raw)
				if e != nil {
					return e
				}
				if current.SessionID != previous.SessionID || current.BaseURL != baseURL {
					return fmt.Errorf("the saved session changed")
				}
				previous = current
			}
			response, e = a.oauthRequest(pollCtx, baseURL, loginTokenPath, pollBody)
			if e == nil {
				completed = true
				result = a.finishBrowserLogin(ctx, cmd, opts, resolved, baseURL, response, previous, reauthorize && previous.Version == 2, requestedScopes)
			}
			return nil
		}
		if reauthorize && previous.Version == 2 {
			locked := *a
			locked.Context = ctx
			if err := locked.withConfigLock(attempt); err != nil {
				return a.fail(configError("OAUTH_SESSION_CHANGED", "The saved session changed during browser approval. Retry reauthorization.", err), opts)
			}
		} else {
			_ = attempt()
		}
		if completed {
			return result
		}
		if e != nil {
			switch strings.ToLower(e.Code) {
			case "authorization_pending":
				continue
			case "oauth_timeout":
				interval *= 2
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			case "access_denied":
				return a.fail(configError("LOGIN_DENIED", "Browser login was declined. Run flint login to try again.", nil), opts)
			case "expired_token":
				return a.fail(configError("LOGIN_EXPIRED", "The sign-in code expired. Run flint login to try again.", nil), opts)
			default:
				return a.fail(loginRequestError(pollCtx, e), opts)
			}
		}
	}
}

func (a *App) finishBrowserLogin(ctx context.Context, cmd *Command, opts Options, resolved ResolvedConfig, baseURL string, response []byte, previous oauthCredential, locked bool, requestedScopes []string) int {
	credential, e := a.decodeOAuthTokens(response, baseURL)
	if e != nil {
		fallback := oauthCredential{}
		if locked {
			fallback = previous
		}
		return a.fail(a.cleanupOAuthResponse(response, baseURL, fallback, e), opts)
	}
	if cmd.CanonicalName == "auth.reauth" && credential.Version != 2 {
		return a.fail(a.cleanupOAuth(credential, contextEndpointError(configError("LOGIN_UNAVAILABLE", "", nil))), opts)
	}
	if locked {
		if credential.Version != 2 || credential.SessionID != previous.SessionID || credential.RefreshToken == previous.RefreshToken {
			return a.fail(a.cleanupOAuth(credential, configError("OAUTH_SESSION_CHANGED", "Reauthorization returned an invalid session or unrotated token.", nil)), opts)
		}
		if len(requestedScopes) > 0 && credential.ContextID != resolved.ContextID {
			return a.fail(a.cleanupOAuth(credential, configError("OAUTH_CONTEXT_MISMATCH", "Scope reauthorization returned a different context from the requested context.", nil)), opts)
		}
		// Redemption rotates the existing family. Persist before context validation
		// so transient failures cannot discard its only usable refresh token.
		credential.Auth = previous.Auth
		credential.PendingValidation = true
		encoded, _ := json.Marshal(credential)
		if err := a.StoreCredential(resolved.ProfileName, string(encoded)); err != nil {
			return a.fail(a.cleanupOAuth(credential, configError("KEYCHAIN_WRITE_FAILED", "Could not save the reauthorized session.", err)), opts)
		}
	}
	auth, e := a.fetchAuthContext(ctx, baseURL, credential.AccessToken, false)
	if e != nil {
		if locked {
			return a.fail(cliError(e.ExitCode, e.Type, "OAUTH_CONTEXT_UNAVAILABLE", "The reauthorized session is saved but could not be verified. Retry flint auth status or select an authorized context."), opts)
		}
		return a.fail(a.cleanupOAuth(credential, configError("LOGIN_VALIDATION_FAILED", "The issued session could not be validated. Run flint login again.", nil)), opts)
	}
	if !validOAuthContext(auth) || (credential.Version == 2 && (auth.OAuthSessionID != credential.SessionID || auth.ContextID != credential.ContextID)) {
		return a.fail(a.cleanupOAuth(credential, configError("LOGIN_VALIDATION_FAILED", "The issued session context did not match the token response.", nil)), opts)
	}
	if !sameScopeSet(credential.TokenScopes, auth.Scopes) {
		return a.fail(a.cleanupOAuth(credential, configError("OAUTH_SCOPE_MISMATCH", "The authorized scopes did not match the token response. Run flint auth login again.", nil)), opts)
	}
	if locked && previous.ContextID == credential.ContextID && !matchesOAuthCredential(credential, auth) {
		return a.fail(a.cleanupOAuth(credential, configError("OAUTH_CONTEXT_MISMATCH", "Reauthorization changed the selected context identity.", nil)), opts)
	}
	// Fresh logins default to sandbox. Reauthorization can retain an explicitly
	// selected live context, but must not silently select a different live one.
	if locked && credential.ContextID == resolved.ContextID {
		auth.SelectedContext = true
	}
	if e = validateCredentialIntent(auth, opts, resolved.MerchantGuard, resolved.SandboxGuard); e != nil {
		return a.fail(a.cleanupOAuth(credential, e), opts)
	}
	credential.Auth = auth
	credential.PendingValidation = false
	encoded, _ := json.Marshal(credential)
	persistence := *a
	persistence.Context = ctx
	if locked {
		e = persistence.saveReauthorizedCredentialUnlocked(resolved, string(encoded), auth)
	} else {
		e = persistence.saveAuthenticatedCredential(resolved.ProfileName, string(encoded), auth)
	}
	if e != nil {
		if locked {
			return a.fail(e, opts)
		}
		return a.fail(a.cleanupOAuth(credential, e), opts)
	}
	if !locked && previous.RefreshToken != "" && previous.RefreshToken != credential.RefreshToken {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cleanupErr := a.revokeOAuth(cleanupCtx, previous)
		cancel()
		if cleanupErr != nil {
			fmt.Fprintln(a.Stderr, "Signed in, but the previous session could not be revoked. Revoke it at https://app.withflintpay.com/developers/cli.")
		}
	}
	next := "flint context list"
	if credential.Version == 1 {
		next = "flint doctor"
	}
	data := map[string]any{"authenticated": true, "credential_saved": true, "profile": resolved.ProfileName, "environment": auth.Environment, "merchant_id": auth.MerchantID, "sandbox_id": auth.SandboxID, "context_id": auth.ContextID, "oauth_session_id": auth.OAuthSessionID, "next": next}
	if len(requestedScopes) > 0 {
		approved, unapproved := partitionScopes(requestedScopes, auth.Scopes)
		data["requested_scopes"] = requestedScopes
		data["approved_scopes"] = approved
		data["unapproved_scopes"] = unapproved
		data["partial_approval"] = len(unapproved) > 0
		if len(unapproved) > 0 && !opts.Quiet {
			fmt.Fprintln(a.Stderr, "Some requested scopes were not approved:", strings.Join(unapproved, ", "))
		}
	}
	return a.outputLocal(map[string]any{"data": data}, cmd, opts)
}

func requestedCLIScopes(opts Options) ([]string, *CLIError) {
	allowed := make(map[string]bool)
	for _, scope := range embeddedAllowedCLIScopes() {
		allowed[scope] = true
	}
	result := make([]string, 0, len(opts.Raw["scope"]))
	seen := make(map[string]bool)
	for _, raw := range opts.Raw["scope"] {
		scope := strings.TrimSpace(raw)
		if scope == "" || strings.ContainsAny(scope, " \t\r\n") || !allowed[scope] {
			return nil, usageError("INVALID_SCOPE", "Unknown or unsupported CLI OAuth scope: "+raw+".", "scope")
		}
		if !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	return result, nil
}

func appendUniqueStrings(base []string, values ...string) []string {
	seen := make(map[string]bool, len(base)+len(values))
	for _, value := range base {
		seen[value] = true
	}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			base = append(base, value)
		}
	}
	return base
}

func partitionScopes(requested, granted []string) ([]string, []string) {
	set := make(map[string]bool, len(granted))
	for _, scope := range granted {
		set[scope] = true
	}
	approved := make([]string, 0, len(requested))
	unapproved := make([]string, 0)
	for _, scope := range requested {
		if set[scope] {
			approved = append(approved, scope)
		} else {
			unapproved = append(unapproved, scope)
		}
	}
	return approved, unapproved
}

func loginRequestError(ctx context.Context, e *CLIError) *CLIError {
	if ctx.Err() == context.Canceled {
		return configError("LOGIN_CANCELED", "Browser login was canceled.", nil)
	}
	if ctx.Err() != nil {
		return configError("LOGIN_EXPIRED", "Browser login timed out. Run flint login to try again.", nil)
	}
	if e != nil && e.Code == "invalid_scope" {
		return configError("invalid_scope", "The requested scope is not available for the selected context.", nil)
	}
	if e != nil && e.Code == "invalid_context" {
		return configError("invalid_context", "The selected context does not match this browser authorization request.", nil)
	}
	if e != nil && e.Code == "LOGIN_UNAVAILABLE" {
		return configError("LOGIN_UNAVAILABLE", "Browser login is not available. Use flint auth import.", nil)
	}
	// Never echo remote error details: this exchange contains one-time secrets.
	return networkError("LOGIN_REQUEST_FAILED", "Could not complete browser login. Try again, or use flint auth import.", nil)
}

func loginVerificationURL(raw, complete, baseURL, code string) (string, bool) {
	if len(code) < 4 || len(code) > 32 {
		return "", false
	}
	for _, c := range code {
		if c < 33 || c > 126 {
			return "", false
		}
	}
	validate := func(value string) bool {
		u, err := url.Parse(value)
		if err != nil || u.User != nil || u.Fragment != "" {
			return false
		}
		base, err := url.Parse(baseURL)
		if err != nil {
			return false
		}
		origin := *u
		origin.RawQuery = ""
		if _, err := validateBaseURL(origin.String()); err != nil {
			return false
		}
		return (u.Scheme == "https" && (u.Host == "app.withflintpay.com" || (base.Scheme == "https" && base.Host == "api.staging.withflintpay.com" && u.Host == "app.staging.withflintpay.com"))) || (u.Scheme == base.Scheme && u.Host == base.Host)
	}
	if !validate(raw) {
		return "", false
	}
	if complete == "" {
		return raw, true
	}
	if !validate(complete) {
		return "", false
	}
	a, _ := url.Parse(raw)
	b, _ := url.Parse(complete)
	if a.Scheme != b.Scheme || a.Host != b.Host {
		return "", false
	}
	return complete, true
}
