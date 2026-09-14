package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

var initialCLIScopes = embeddedInitialCLIScopes()

func (a *App) runSignup(opts Options) int {
	// Validate the local destination before creating remote account resources or
	// issuing a one-time API key that this process may be unable to persist.
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	_, previousErr := a.LoadCredential(resolved.ProfileName)
	if previousErr != nil {
		return a.fail(configError("CREDENTIAL_LOOKUP_FAILED", "The existing profile credential could not be read before signup.", previousErr), opts)
	}
	reader := bufio.NewReader(&contextInputReader{ctx: a.commandContext(), reader: a.Stdin})
	email, e := a.signupValue(reader, opts, "email", "Email")
	if e != nil {
		return a.fail(e, opts)
	}
	if !strings.Contains(email, "@") {
		return a.fail(usageError("INVALID_EMAIL", "Enter a valid email address.", "email"), opts)
	}
	firstName, e := a.signupValue(reader, opts, "first-name", "First name")
	if e != nil {
		return a.fail(e, opts)
	}
	lastName, e := a.signupValue(reader, opts, "last-name", "Last name")
	if e != nil {
		return a.fail(e, opts)
	}
	baseURL := strings.TrimSpace(os.Getenv("FLINT_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}
	baseURL, err = validateBaseURL(baseURL)
	if err != nil {
		return a.fail(configError("INVALID_BASE_URL", err.Error(), err), opts)
	}
	startBody, _ := json.Marshal(map[string]any{"email": email, "first_name": firstName, "last_name": lastName})
	idempotency, err := newIdempotencyKey()
	if err != nil {
		return a.fail(networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate an idempotency key.", err), opts)
	}
	start, e := a.doSignupRequest(baseURL, "", http.MethodPost, "/v1/onboarding/start", startBody, idempotency, opts)
	if e != nil {
		return a.fail(e, opts)
	}
	verificationToken, ok := firstStringAt(start.Value, "data.verification_token")
	if !ok {
		return a.fail(invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint did not return an email verification token.", nil), opts)
	}
	code, inputErr := a.signupValue(reader, opts, "verification-code", "Verification code")
	if inputErr != nil {
		return a.fail(inputErr, opts)
	}
	verifyBody, _ := json.Marshal(map[string]any{"verification_token": verificationToken, "verification_code": code})
	verifyKey, err := newIdempotencyKey()
	if err != nil {
		return a.fail(networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate an idempotency key.", err), opts)
	}
	verify, e := a.doSignupRequest(baseURL, "", http.MethodPost, "/v1/onboarding/verify-email", verifyBody, verifyKey, opts)
	if e != nil {
		return a.fail(e, opts)
	}
	sessionToken, ok := firstStringAt(verify.Value, "data.onboarding_session_token")
	if !ok {
		return a.fail(invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint did not return an onboarding session token.", nil), opts)
	}
	state, e := a.doSignupRequest(baseURL, sessionToken, http.MethodGet, "/v1/onboarding/state", nil, "", opts)
	if e != nil {
		return a.fail(e, opts)
	}
	state, e = a.advanceSignupUntilKeyAvailable(baseURL, sessionToken, state, reader, email, opts)
	if e != nil {
		return a.fail(e, opts)
	}
	sandboxID, _ := firstStringAt(state.Value, "data.default_sandbox_id")
	if sandboxID == "" {
		return a.fail(invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint did not return a default sandbox for the initial API key.", nil), opts)
	}
	keyBody, _ := json.Marshal(map[string]any{"name": "flint-cli", "sandbox_id": sandboxID, "scopes": initialCLIScopes})
	issueKey, err := newIdempotencyKey()
	if err != nil {
		return a.fail(networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate an idempotency key.", err), opts)
	}
	created, e := a.doSignupRequest(baseURL, sessionToken, http.MethodPost, "/v1/onboarding/api-key", keyBody, issueKey, opts)
	if e != nil {
		return a.fail(e, opts)
	}
	secret, ok := firstStringAt(created.Value, "data.secret_key")
	if !ok {
		return a.fail(invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint did not return the initial sandbox API key.", nil), opts)
	}
	apiKeyID, _ := firstStringAt(created.Value, "data.api_key_id")
	if apiKeyID == "" {
		primary := invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint did not return the initial sandbox API key ID.", nil)
		return a.fail(a.compensateIssuedSignupKey(baseURL, secret, apiKeyID, primary, opts), opts)
	}
	var persistenceErr *CLIError
	lockErr := a.withConfigLock(func() error {
		if a.Context != nil && a.Context.Err() != nil {
			persistenceErr = networkError("REQUEST_CANCELED", "Signup was canceled before the credential was stored.", a.Context.Err())
			return persistenceErr
		}
		cfg, loadErr := a.loadConfig()
		if loadErr != nil {
			persistenceErr = configError("CONFIG_INVALID", loadErr.Error(), loadErr)
			return loadErr
		}
		previousCredential, credentialErr := a.LoadCredential(resolved.ProfileName)
		if credentialErr != nil {
			persistenceErr = configError("CREDENTIAL_LOOKUP_FAILED", "The existing profile credential could not be read before signup.", credentialErr)
			return credentialErr
		}
		if storeErr := a.StoreCredential(resolved.ProfileName, secret); storeErr != nil {
			persistenceErr = configError("KEYCHAIN_WRITE_FAILED", "The new credential could not be stored in the OS keychain.", storeErr)
			if restoreErr := a.restoreCredential(resolved.ProfileName, previousCredential); restoreErr != nil {
				persistenceErr.Message += "; credential rollback also failed: " + restoreErr.Error()
			}
			return storeErr
		}
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]Profile{}
		}
		p := cfg.Profiles[resolved.ProfileName]
		p.Environment = "sandbox"
		p.SandboxID = sandboxID
		p.APIKeyID = apiKeyID
		if merchant, ok := firstStringAt(created.Value, "data.merchant_id"); ok {
			p.MerchantID = merchant
		}
		cfg.Profiles[resolved.ProfileName] = p
		if saveErr := a.saveConfigUnlocked(cfg); saveErr != nil {
			persistenceErr = configError("CONFIG_WRITE_FAILED", saveErr.Error(), saveErr)
			if restoreErr := a.restoreCredential(resolved.ProfileName, previousCredential); restoreErr != nil {
				persistenceErr.Message += "; credential rollback also failed: " + restoreErr.Error()
			}
			return saveErr
		}
		return nil
	})
	if lockErr != nil {
		if persistenceErr == nil {
			persistenceErr = configError("CONFIG_WRITE_FAILED", lockErr.Error(), lockErr)
		}
		cleanupErr := a.revokeIssuedSignupKey(baseURL, secret, apiKeyID, opts)
		return a.fail(withSignupKeyCleanupResult(persistenceErr, cleanupErr, apiKeyID), opts)
	}
	if m, ok := created.Value.(map[string]any); ok {
		if data, ok := m["data"].(map[string]any); ok {
			delete(data, "secret_key")
		}
	}
	result := map[string]any{"data": map[string]any{
		"api_key":          valueAt(created.Value, "data"),
		"onboarding":       valueAt(state.Value, "data"),
		"credential_saved": true,
	}}
	return a.outputLocal(result, nilCommand("signup"), opts)
}

func (a *App) advanceSignupUntilKeyAvailable(baseURL, sessionToken string, state *apiResponse, reader *bufio.Reader, email string, opts Options) (*apiResponse, *CLIError) {
	seenSteps := map[string]struct{}{}
	for {
		canIssue, _ := lookupPath(state.Value, "data.can_issue_api_key")
		if canIssue == true {
			return state, nil
		}
		step, ok := valueAt(state.Value, "data.next_step").(map[string]any)
		if !ok || len(step) == 0 {
			return nil, signupStateError(
				"SIGNUP_KEY_NOT_AVAILABLE",
				"Flint did not make the initial sandbox API key available or return a next onboarding step.",
				state,
			)
		}
		machineCompletable, _ := step["machine_completable"].(bool)
		if !machineCompletable {
			return nil, signupStateError(
				"SIGNUP_HUMAN_ACTION_REQUIRED",
				"Initial key issuance is blocked on a human onboarding action. Complete the returned onboarding step, then retry signup.",
				state,
			)
		}
		method, _ := step["submit_method"].(string)
		if method == "" {
			method = http.MethodPost
		}
		if !strings.EqualFold(method, http.MethodPost) {
			return nil, signupStateError("INVALID_SIGNUP_RESPONSE", "Flint returned an unsupported onboarding step method.", state)
		}
		rawEndpoint, _ := step["submit_endpoint"].(string)
		endpoint, endpointErr := signupPublicEndpoint(baseURL, rawEndpoint)
		if endpointErr != nil {
			return nil, invalidResponseError("INVALID_SIGNUP_RESPONSE", endpointErr.Error(), endpointErr)
		}
		stepCode, _ := step["code"].(string)
		stepKey := strings.TrimSpace(stepCode) + "|" + endpoint
		if _, repeated := seenSteps[stepKey]; repeated {
			return nil, signupStateError(
				"SIGNUP_ONBOARDING_STALLED",
				"Flint returned the same machine onboarding step after it was submitted.",
				state,
			)
		}
		seenSteps[stepKey] = struct{}{}
		body, bodyErr := a.signupAdvanceBody(reader, opts, email, step)
		if bodyErr != nil {
			return nil, bodyErr
		}
		idempotencyKey, err := newIdempotencyKey()
		if err != nil {
			return nil, networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate an idempotency key for onboarding.", err)
		}
		state, bodyErr = a.doSignupRequest(baseURL, sessionToken, http.MethodPost, endpoint, body, idempotencyKey, opts)
		if bodyErr != nil {
			return nil, bodyErr
		}
	}
}

func signupStateError(code, message string, state *apiResponse) *CLIError {
	err := configError(code, message, nil)
	if state != nil {
		err.Details = map[string]any{
			"onboarding": valueAt(state.Value, "data"),
			"next_step":  valueAt(state.Value, "data.next_step"),
		}
	}
	return err
}

func signupPublicEndpoint(baseURL, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "/v1/") || raw == "/v1" {
		return raw, nil
	}
	endpoint, err := url.Parse(raw)
	if err != nil || !endpoint.IsAbs() || endpoint.User != nil || endpoint.Fragment != "" {
		return "", fmt.Errorf("onboarding submit_endpoint must be a documented /v1 API URL")
	}
	base, err := url.Parse(baseURL)
	if err != nil || !strings.EqualFold(endpoint.Scheme, base.Scheme) || !strings.EqualFold(endpoint.Host, base.Host) {
		return "", fmt.Errorf("onboarding submit_endpoint must use the configured Flint API origin")
	}
	if !strings.HasPrefix(endpoint.Path, "/v1/") && endpoint.Path != "/v1" {
		return "", fmt.Errorf("onboarding submit_endpoint must be a documented /v1 API URL")
	}
	return endpoint.RequestURI(), nil
}

func (a *App) signupAdvanceBody(reader *bufio.Reader, opts Options, email string, step map[string]any) ([]byte, *CLIError) {
	required := map[string]bool{}
	if values, ok := step["required_fields"].([]any); ok {
		for _, raw := range values {
			if field, ok := raw.(string); ok {
				required[strings.TrimSpace(field)] = true
			}
		}
	}
	body := map[string]any{}
	profile := map[string]any{}
	fields := []struct {
		Field string
		Flag  string
		Label string
	}{
		{Field: "website_url", Flag: "website-url", Label: "Business website URL"},
		{Field: "support_email", Flag: "support-email", Label: "Support email"},
		{Field: "support_phone", Flag: "support-phone", Label: "Support phone"},
		{Field: "support_url", Flag: "support-url", Label: "Support URL"},
	}
	for _, field := range fields {
		value := lastSignupOption(opts, field.Flag)
		if value == "" && required[field.Field] {
			var err *CLIError
			value, err = a.signupValue(reader, opts, field.Flag, field.Label)
			if err != nil {
				return nil, err
			}
		}
		if value != "" {
			profile[field.Field] = value
		}
	}
	if required["email"] {
		profile["email"] = strings.TrimSpace(email)
	}
	if len(profile) > 0 {
		body["profile"] = profile
	}
	country := lastSignupOption(opts, "country")
	if country == "" && required["country"] {
		var err *CLIError
		country, err = a.signupValue(reader, opts, "country", "Business country")
		if err != nil {
			return nil, err
		}
	}
	if country != "" {
		body["country"] = strings.ToUpper(country)
	}
	if values := opts.Raw["requested-capability"]; len(values) > 0 {
		capabilities := make([]string, 0, len(values))
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				capabilities = append(capabilities, value)
			}
		}
		if len(capabilities) > 0 {
			body["requested_capabilities"] = capabilities
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, invalidResponseError("SIGNUP_INPUT_INVALID", "Could not encode onboarding input.", err)
	}
	return encoded, nil
}

func lastSignupOption(opts Options, name string) string {
	values := opts.Raw[name]
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[len(values)-1])
}

func (a *App) compensateIssuedSignupKey(baseURL, secret, apiKeyID string, primary *CLIError, opts Options) *CLIError {
	cleanupErr := a.revokeIssuedSignupKey(baseURL, secret, apiKeyID, opts)
	return withSignupKeyCleanupResult(primary, cleanupErr, apiKeyID)
}

func (a *App) revokeIssuedSignupKey(baseURL, secret, apiKeyID string, opts Options) *CLIError {
	// Revocation must still be attempted when the signup was canceled after
	// issuing a key. Each cleanup request retains its bounded retry timeout.
	cleanup := *a
	cleanup.Context = context.Background()
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		authContext, err := cleanup.doSignupRequest(baseURL, secret, http.MethodGet, "/v1/developer/auth-context", nil, "", opts)
		if err != nil {
			return err
		}
		apiKeyID, _ = firstStringAt(authContext.Value, "data.api_key_id")
	}
	if apiKeyID == "" {
		return invalidResponseError("SIGNUP_KEY_COMPENSATION_FAILED", "Flint did not identify the newly issued API key for revocation.", nil)
	}
	idempotencyKey, err := newIdempotencyKey()
	if err != nil {
		return networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate an idempotency key for signup cleanup.", err)
	}
	_, cliErr := cleanup.doSignupRequest(
		baseURL,
		secret,
		http.MethodPost,
		"/v1/api-keys/"+url.PathEscape(apiKeyID)+"/revoke",
		nil,
		idempotencyKey,
		opts,
	)
	return cliErr
}

func withSignupKeyCleanupResult(primary, cleanupErr *CLIError, apiKeyID string) *CLIError {
	if primary == nil {
		primary = configError("SIGNUP_LOCAL_PERSISTENCE_FAILED", "The new credential could not be persisted locally.", nil)
	}
	if cleanupErr == nil {
		primary.Message += " Flint revoked the newly issued API key."
		return primary
	}
	primary.Message += " Flint could not automatically revoke the newly issued API key."
	reason := "Review the flint-cli API key in the dashboard and revoke it before retrying signup."
	if strings.TrimSpace(apiKeyID) != "" {
		reason = "Revoke API key " + strings.TrimSpace(apiKeyID) + " in the dashboard before retrying signup."
	}
	primary.Details = map[string]any{
		"api_key_id":         strings.TrimSpace(apiKeyID),
		"cleanup_error_code": cleanupErr.Code,
		"remediation": map[string]any{"next_actions": []any{
			map[string]any{"reason_message": reason},
		}},
	}
	return primary
}

func (a *App) doSignupRequest(baseURL, token, method, path string, body []byte, idempotencyKey string, opts Options) (*apiResponse, *CLIError) {
	// The verification-code prompt can legitimately take minutes. Give each
	// network operation its own retry budget so time spent by the human does not
	// consume the next request's transport timeout.
	parent := a.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()
	return a.doRequest(ctx, baseURL, token, method, path, body, idempotencyKey, opts.Debug)
}

func (a *App) signupValue(reader *bufio.Reader, opts Options, flagName, label string) (string, *CLIError) {
	if values := opts.Raw[flagName]; len(values) > 0 {
		if value := strings.TrimSpace(values[len(values)-1]); value != "" {
			return value, nil
		}
	}
	if opts.NoInput || !a.IsTTY() {
		return "", configError("SIGNUP_INPUT_REQUIRED", "flint signup requires --"+flagName+" when input is disabled.", nil)
	}
	fmt.Fprint(a.Stderr, label+": ")
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", configError("SIGNUP_INPUT_FAILED", "Could not read "+strings.ToLower(label)+".", err)
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return "", usageError("MISSING_REQUIRED_ARGUMENT", label+" cannot be empty.", flagName)
	}
	return value, nil
}

func valueAt(value any, path string) any {
	found, _ := lookupPath(value, path)
	return found
}

func nilCommand(name string) *Command { return &Command{Name: name, CanonicalName: name} }
