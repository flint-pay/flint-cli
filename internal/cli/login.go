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
	// JSON output implicitly disables terminal input elsewhere. This flow reads
	// no terminal input, so only an explicit automation setting blocks approval.
	noInputEnv := strings.TrimSpace(os.Getenv("FLINT_NO_INPUT"))
	if booleanRawOption(opts, "no-input") || noInputEnv == "1" || strings.EqualFold(noInputEnv, "true") {
		return a.fail(usageError("LOGIN_REQUIRES_APPROVAL", "Browser login requires your approval on the website. For automation, use FLINT_API_KEY or flint auth import --stdin.", "no-input"), opts)
	}
	for _, name := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_CHECKOUT_SESSION_SECRET"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return a.fail(configError("ENVIRONMENT_CREDENTIAL_ACTIVE", "Unset "+name+" before browser login; it would override the saved credential.", nil), opts)
		}
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	if _, err := a.LoadCredential(resolved.ProfileName); err != nil {
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
	mode := "sandbox"
	if opts.Live {
		mode = "live"
	}
	body := url.Values{"client_id": {oauthClientID}, "scope": {strings.Join(initialCLIScopes, " ")}, "environment": {mode}}
	if resolved.MerchantGuard != "" {
		body.Set("merchant_id", resolved.MerchantGuard)
	}
	if resolved.SandboxGuard != "" {
		body.Set("sandbox_id", resolved.SandboxGuard)
	}
	response, e := a.oauthRequest(ctx, baseURL, loginDevicePath, body)
	if e != nil {
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
		response, e = a.oauthRequest(pollCtx, baseURL, loginTokenPath, pollBody)
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
				return a.fail(configError("LOGIN_DENIED", "Browser login was declined. Run flint auth login to try again.", nil), opts)
			case "expired_token":
				return a.fail(configError("LOGIN_EXPIRED", "The sign-in code expired. Run flint auth login to try again.", nil), opts)
			default:
				return a.fail(loginRequestError(pollCtx, e), opts)
			}
		}
		credential, validationErr := a.decodeOAuthTokens(response, baseURL)
		if validationErr != nil {
			return a.fail(a.cleanupOAuthResponse(response, baseURL, oauthCredential{}, validationErr), opts)
		}
		auth, validationErr := a.fetchAuthContext(ctx, baseURL, credential.AccessToken, false)
		if validationErr != nil || !validOAuthContext(auth) {
			validationErr = configError("LOGIN_VALIDATION_FAILED", "The issued OAuth session could not be validated. Run flint auth login again.", nil)
		} else {
			validationErr = validateCredentialIntent(auth, opts, resolved.MerchantGuard, resolved.SandboxGuard)
		}
		if validationErr == nil {
			credential.Auth = auth
			encoded, _ := json.Marshal(credential)
			persistence := *a
			persistence.Context = ctx
			validationErr = persistence.saveAuthenticatedCredential(resolved.ProfileName, string(encoded), auth)
		}
		if validationErr != nil {
			return a.fail(a.cleanupOAuth(credential, validationErr), opts)
		}

		return a.outputLocal(map[string]any{"data": map[string]any{"authenticated": true, "credential_saved": true, "profile": resolved.ProfileName, "environment": auth.Environment, "merchant_id": auth.MerchantID, "sandbox_id": auth.SandboxID}}, cmd, opts)
	}
}

func loginRequestError(ctx context.Context, e *CLIError) *CLIError {
	if ctx.Err() == context.Canceled {
		return configError("LOGIN_CANCELED", "Browser login was canceled.", nil)
	}
	if ctx.Err() != nil {
		return configError("LOGIN_EXPIRED", "Browser login timed out. Run flint auth login to try again.", nil)
	}
	if e != nil && e.Code == "LOGIN_UNAVAILABLE" {
		return configError("LOGIN_UNAVAILABLE", "Browser login is not available on this Flint server yet. Use flint auth import.", nil)
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
		return (u.Scheme == "https" && u.Host == "app.withflintpay.com") || (u.Scheme == base.Scheme && u.Host == base.Host)
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
