package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

const oauthClientID = "flint-cli"
const loginDevicePath = "/v1/oauth/device/authorize"
const loginTokenPath = "/v1/oauth/token"
const oauthRevokePath = "/v1/oauth/revoke"
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// The complete record lives only in the OS keychain. Existing raw API-key
// entries remain readable; tokens are never placed in config.json.
type oauthCredential struct {
	PendingValidation bool        `json:"pending_validation,omitempty"`
	Kind              string      `json:"kind"`
	Version           int         `json:"version"`
	BaseURL           string      `json:"base_url"`
	AccessToken       string      `json:"access_token"`
	RefreshToken      string      `json:"refresh_token"`
	ExpiresAt         time.Time   `json:"expires_at"`
	Auth              AuthContext `json:"auth_context"`
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func isOAuthCredential(raw string) bool { return strings.HasPrefix(strings.TrimSpace(raw), "{") }

func decodeOAuthCredential(raw string) (oauthCredential, *CLIError) {
	var c oauthCredential
	if json.Unmarshal([]byte(raw), &c) != nil || c.Kind != "oauth" || c.Version != 1 || !validOAuthToken(c.AccessToken) || !validOAuthToken(c.RefreshToken) || c.ExpiresAt.IsZero() || !validOAuthContext(c.Auth) {
		return c, configError("INVALID_OAUTH_CREDENTIAL", "The saved OAuth session is invalid. Run flint auth login again.", nil)
	}
	base, err := validateBaseURL(c.BaseURL)
	if err != nil || base != c.BaseURL {
		return c, configError("INVALID_OAUTH_CREDENTIAL", "The saved OAuth server is invalid. Run flint auth login again.", nil)
	}
	return c, nil
}

func validOAuthToken(token string) bool {
	if token == "" || len(token) > 16384 {
		return false
	}
	for _, c := range token {
		if c <= 32 || c >= 127 {
			return false
		}
	}
	return true
}

func validOAuthContext(auth AuthContext) bool {
	return auth.AuthType == "oauth" && auth.OAuthGrantID != "" && auth.MerchantID != "" && len(auth.Scopes) > 0 &&
		(auth.Environment == "live" || (auth.Environment == "sandbox" && auth.SandboxID != ""))
}

func (a *App) oauthBaseURL(c oauthCredential) *CLIError {
	override := a.BaseURL
	if override == "" {
		override = strings.TrimSpace(os.Getenv("FLINT_BASE_URL"))
	}
	if override != "" {
		normalized, err := validateBaseURL(override)
		if err != nil || normalized != c.BaseURL {
			return configError("OAUTH_SERVER_MISMATCH", "This OAuth session belongs to a different Flint server. Unset FLINT_BASE_URL or sign in to that server using a separate profile.", nil)
		}
	}
	return nil
}

// OAuth endpoints use RFC 6749 form bodies and flat JSON responses, not Flint
// API envelopes. Do not log bodies or automatically replay rotating grants.
func (a *App) oauthRequest(ctx context.Context, baseURL, path string, form url.Values) ([]byte, *CLIError) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, networkError("OAUTH_REQUEST_FAILED", "Could not prepare the OAuth request.", nil)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "flintpay-cli/"+a.Info.Version)
	req.Header.Set("X-Flint-CLI-Version", a.Info.Version)
	req.Header.Set("Flint-Version", apispec.APIVersion())
	client := a.HTTPClient
	if client == nil {
		client = defaultAPIHTTPClient
	}
	clone := *client
	clone.CheckRedirect = rejectAPIRedirect
	resp, err := clone.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, networkError("OAUTH_TIMEOUT", "The OAuth request timed out.", nil)
		}
		return nil, networkError("OAUTH_REQUEST_FAILED", "Could not reach the OAuth server. Retry the command; if the session expired, run flint auth login.", nil)
	}
	defer resp.Body.Close()
	if path == oauthRevokePath {
		if resp.StatusCode == http.StatusOK {
			return nil, nil
		}
		// RFC 7009 confirms completed revocation with HTTP 200. A 202 is
		// only acceptance, and must not cause local credentials to be deleted.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, networkError("OAUTH_REVOCATION_UNCONFIRMED", "The OAuth server did not confirm completed revocation. Retry logout.", nil)
		}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return nil, networkError("OAUTH_RESPONSE_INVALID", "The OAuth server returned an unreadable response.", nil)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return nil, configError("LOGIN_UNAVAILABLE", "OAuth browser login is not available on this Flint server yet. Use flint auth import.", nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		// Only known protocol error names may reach output. Never echo remote
		// descriptions, arbitrary error strings, or token response bodies.
		switch body.Error {
		case "authorization_pending", "slow_down", "access_denied", "expired_token", "invalid_grant", "invalid_client", "invalid_scope", "unsupported_grant_type":
			return nil, configError(body.Error, "OAuth authorization failed. Run flint auth login again.", nil)
		}
		return nil, networkError("OAUTH_REQUEST_FAILED", "The OAuth server rejected the request. Try again, or run flint auth login.", nil)
	}
	return raw, nil
}

func (a *App) decodeOAuthTokens(raw []byte, baseURL string) (oauthCredential, *CLIError) {
	var response oauthTokenResponse
	if json.Unmarshal(raw, &response) != nil || !strings.EqualFold(response.TokenType, "Bearer") || !validOAuthToken(response.AccessToken) || !validOAuthToken(response.RefreshToken) || response.ExpiresIn < 1 || response.ExpiresIn > 86400 {
		return oauthCredential{}, configError("INVALID_OAUTH_RESPONSE", "Flint returned invalid OAuth tokens or expiry. Run flint auth login again.", nil)
	}
	return oauthCredential{Kind: "oauth", Version: 1, BaseURL: baseURL, AccessToken: response.AccessToken, RefreshToken: response.RefreshToken, ExpiresAt: a.Now().Add(time.Duration(response.ExpiresIn) * time.Second)}, nil
}

func (a *App) revokeOAuth(ctx context.Context, c oauthCredential) *CLIError {
	_, e := a.oauthRequest(ctx, c.BaseURL, oauthRevokePath, url.Values{"client_id": {oauthClientID}, "token": {c.RefreshToken}, "token_type_hint": {"refresh_token"}})
	return e
}

// Even a malformed success response can contain an issued refresh token.
// Revoke it when possible instead of leaving an unusable session behind.
func (a *App) cleanupOAuthResponse(raw []byte, baseURL string, fallback oauthCredential, primary *CLIError) *CLIError {
	var response oauthTokenResponse
	_ = json.Unmarshal(raw, &response)
	if validOAuthToken(response.RefreshToken) {
		fallback = oauthCredential{BaseURL: baseURL, RefreshToken: response.RefreshToken}
	}
	if !validOAuthToken(fallback.RefreshToken) {
		return primary
	}
	return a.cleanupOAuth(fallback, primary)
}

func (a *App) cleanupOAuth(c oauthCredential, primary *CLIError) *CLIError {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := a.revokeOAuth(ctx, c); e != nil {
		primary.Message += " Automatic session revocation failed; revoke this CLI session on the Flint website."
	}
	return primary
}

// Serialize refresh with logout/import across processes, rereading the keychain
// after acquiring the lock so only one process rotates a given refresh token.
func (a *App) oauthAccess(ctx context.Context, profile string) (oauthCredential, *CLIError) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var c oauthCredential
	var result *CLIError
	locked := *a
	locked.Context = ctx
	err := locked.withConfigLock(func() error {
		raw, err := a.LoadCredential(profile)
		if err != nil {
			result = configError("CREDENTIAL_LOOKUP_FAILED", "Could not read the OAuth session from the OS keychain.", nil)
			return result
		}
		if !isOAuthCredential(raw) {
			result = configError("OAUTH_SESSION_CHANGED", "The saved credential changed. Retry the command.", nil)
			return result
		}
		c, result = decodeOAuthCredential(raw)
		if result != nil {
			return result
		}
		if result = a.oauthBaseURL(c); result != nil {
			return result
		}
		if !a.Now().Add(30 * time.Second).Before(c.ExpiresAt) {
			response, e := a.oauthRequest(ctx, c.BaseURL, loginTokenPath, url.Values{"client_id": {oauthClientID}, "grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken}})
			if e != nil {
				if e.Code == "invalid_grant" {
					e = configError("OAUTH_SESSION_EXPIRED", "Your Flint session expired or was revoked. Run flint auth login again.", nil)
				}
				result = e
				return result
			}
			next, e := a.decodeOAuthTokens(response, c.BaseURL)
			if e != nil {
				result = a.cleanupOAuthResponse(response, c.BaseURL, c, e)
				return result
			}
			if next.RefreshToken == c.RefreshToken {
				result = a.cleanupOAuth(next, configError("OAUTH_REFRESH_INVALID", "The OAuth server did not rotate the refresh token. Run flint auth login again.", nil))
				return result
			}
			// Save the rotated token before further network calls: the old
			// refresh token is consumed. Until validation finishes, retain the
			// original expected identity and block business requests.
			next.Auth = c.Auth
			next.PendingValidation = true
			encoded, _ := json.Marshal(next)
			if err := a.StoreCredential(profile, string(encoded)); err != nil {
				result = a.cleanupOAuth(next, configError("KEYCHAIN_WRITE_FAILED", "Could not save the refreshed OAuth session. Run flint auth login again.", nil))
				return result
			}
			c = next
		}
		if !c.PendingValidation {
			return nil
		}
		auth, e := a.fetchAuthContext(withoutOAuthSession(ctx), c.BaseURL, c.AccessToken, false)
		if e != nil && e.ExitCode != ExitAuth {
			// Keep the staged tokens for a later verification attempt. An API
			// outage or cancellation does not establish that the grant is bad.
			result = cliError(e.ExitCode, e.Type, "OAUTH_CONTEXT_UNAVAILABLE", "The refreshed session is saved but could not be verified. Retry the command.")
			return result
		}
		if e != nil || !validOAuthContext(auth) || auth.OAuthGrantID != c.Auth.OAuthGrantID || auth.Environment != c.Auth.Environment || auth.MerchantID != c.Auth.MerchantID || auth.SandboxID != c.Auth.SandboxID {
			result = a.cleanupOAuth(c, configError("OAUTH_REFRESH_INVALID", "The refreshed session did not match the original grant. Run flint auth login again.", nil))
			return result
		}
		c.Auth = auth
		c.PendingValidation = false
		encoded, _ := json.Marshal(c)
		if err := a.StoreCredential(profile, string(encoded)); err != nil {
			result = configError("KEYCHAIN_WRITE_FAILED", "The refreshed session is saved but its verification state could not be updated. Retry the command.", nil)
			return result
		}
		return nil
	})
	if err != nil && result == nil {
		result = configError("OAUTH_REFRESH_FAILED", "Could not lock or update the OAuth session. Retry the command.", nil)
	}
	return c, result
}

// Local checks must inspect the credential ordinary API commands would use.
// FLINT_ACCESS_TOKEN has precedence over both imported keys and saved sessions.
func (a *App) resolveCheckCredential(profile string) (string, string, error) {
	if token := strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN")); token != "" {
		return token, "environment_access_token", nil
	}
	return a.resolveCredential(profile)
}

func (a *App) fetchCredentialContext(ctx context.Context, profile, raw string, debug bool) (string, string, authContextEnvelope, *CLIError) {
	if token := strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN")); token != "" && token == raw {
		if !validOAuthToken(token) || strings.HasPrefix(token, "flint_test_") || strings.HasPrefix(token, "flint_live_") {
			return "", "", authContextEnvelope{}, configError("INVALID_CREDENTIAL", "FLINT_ACCESS_TOKEN must contain an access token. Use FLINT_API_KEY for an API key.", nil)
		}
		base, err := a.baseURLForCredential("flint_test_")
		if err != nil {
			return "", "", authContextEnvelope{}, configError("INVALID_CREDENTIAL", err.Error(), err)
		}
		envelope, e := a.fetchAuthContextResponse(ctx, base, token, false)
		if e != nil {
			return "", "", envelope, cliError(e.ExitCode, e.Type, "CREDENTIAL_VALIDATION_FAILED", "Could not validate FLINT_ACCESS_TOKEN. Check the server connection and replace or unset the environment token if it is no longer valid.")
		}
		return token, base, envelope, nil
	}
	if !isOAuthCredential(raw) {
		base, err := a.baseURLForCredential(raw)
		if err != nil {
			return "", "", authContextEnvelope{}, configError("INVALID_CREDENTIAL", err.Error(), err)
		}
		auth, e := a.fetchAuthContextResponse(ctx, base, raw, debug)
		return raw, base, auth, e
	}
	c, e := a.oauthAccess(ctx, profile)
	if e != nil {
		return "", "", authContextEnvelope{}, e
	}
	envelope, e := a.fetchAuthContextResponse(withoutOAuthSession(ctx), c.BaseURL, c.AccessToken, false)
	if e != nil {
		if e.ExitCode == ExitAuth {
			return "", "", envelope, configError("OAUTH_SESSION_INVALID", "Flint rejected the OAuth session. Run flint auth login again.", nil)
		}
		return "", "", envelope, cliError(e.ExitCode, e.Type, "OAUTH_CONTEXT_UNAVAILABLE", "Could not verify the OAuth session because the API request failed. Retry the command.")
	}
	auth := envelope.Data
	if !validOAuthContext(auth) || auth.OAuthGrantID != c.Auth.OAuthGrantID || auth.Environment != c.Auth.Environment || auth.MerchantID != c.Auth.MerchantID || auth.SandboxID != c.Auth.SandboxID {
		return "", "", envelope, configError("OAUTH_CONTEXT_MISMATCH", "The OAuth session context changed. Run flint auth login again.", nil)
	}
	envelope.Data.CredentialScope = "oauth:" + auth.OAuthGrantID
	return c.AccessToken, c.BaseURL, envelope, nil
}

type oauthSessionKey struct{}
type oauthSession struct{ Profile, GrantID string }

func withoutOAuthSession(ctx context.Context) context.Context {
	return context.WithValue(context.WithValue(ctx, oauthSessionKey{}, (*oauthSession)(nil)), responseContractKey{}, (*Command)(nil))
}

func (a *App) oauthRequestToken(ctx context.Context, key, baseURL string) (string, *CLIError) {
	session, _ := ctx.Value(oauthSessionKey{}).(*oauthSession)
	if session == nil || key == "" {
		return key, nil
	}
	c, e := a.oauthAccess(ctx, session.Profile)
	if e != nil {
		return "", e
	}
	if c.BaseURL != baseURL || c.Auth.OAuthGrantID != session.GrantID {
		return "", configError("OAUTH_SESSION_CHANGED", "The active OAuth session changed. Retry the command.", nil)
	}
	return c.AccessToken, nil
}
