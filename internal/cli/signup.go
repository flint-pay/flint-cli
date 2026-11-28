package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Login defaults belong to the client. The API advertises allowed scopes,
// but no longer publishes the retired bootstrap-scope extension.
// Defaults cover shop operations and financial reads. Credential secrets,
// money-out, risk, platform, environment, and hardware access stay explicit.
var initialCLIScopes = []string{
	"accounts.api_keys.read",
	"accounts.api_keys.write",
	"checkouts.checkout_sessions.read",
	"checkouts.checkout_sessions.write",
	"checkouts.payment_links.read",
	"checkouts.payment_links.write",
	"commerce.invoices.read",
	"commerce.invoices.write",
	"merchants.profile.read",
	"merchants.profile.write",
	"merchants.onboarding.read",
	"merchants.onboarding.write",
	"merchants.account_sessions.write",
	"commerce.orders.read",
	"commerce.orders.write",
	"commerce.delivery.read",
	"commerce.delivery.write",
	"commerce.refunds.read",
	"commerce.refunds.write",
	"customers.read",
	"customers.write",
	"developer.request_logs.read",
	"developer.resource_timelines.read",
	"payments.payment_intents.read",
	"payments.payment_intents.write",
	"payments.payment_methods.read",
	"payments.payment_methods.write",
	"payments.payment_method_domains.read",
	"payments.payment_method_domains.write",
	"webhooks.read",
	"webhooks.write",
	"commerce.products.read",
	"commerce.products.write",
	"commerce.catalog.read",
	"commerce.catalog.write",
	"commerce.bundles.read",
	"commerce.bundles.write",
	"commerce.inventory.read",
	"commerce.inventory.write",
	"merchants.locations.read",
	"merchants.locations.write",
	"commerce.promotions.read",
	"commerce.promotions.write",
	"commerce.gift_cards.read",
	"commerce.gift_cards.write",
	"settings.read",
	"settings.write",
	"reports.read",
	"reports.write",
	"commerce.subscription_plans.read",
	"commerce.subscription_plans.write",
	"commerce.subscriptions.read",
	"commerce.subscriptions.write",
	"commerce.credit_notes.read",
	"commerce.credit_notes.write",
	"commerce.returns.read",
	"commerce.returns.write",
	"commerce.inventory.policies.write",
	"commerce.inventory.allocation.write",
	"commerce.gift_cards.redemptions.write",
	"analytics.read",
	"commerce.returns.operations.write",
	"commerce.returns.resolutions.write",
	"commerce.returns.policies.write",
	"commerce.returns.reasons.write",
	"money_movement.balances.read",
	"money_movement.balance_transactions.read",
	"money_movement.payouts.read",
	"money_movement.payout_settings.read",
	"merchant_billing.read",
	"payments.disputes.read",
}

func (a *App) runSignup(opts Options) int {
	opts.allowMissingProfile = true
	if credentialFromEnvironment() != "" {
		return a.fail(configError("ENVIRONMENT_CREDENTIAL_ACTIVE", "Unset FLINT_API_KEY before signup; it would override the saved credential.", nil), opts)
	}
	if workloadIdentityID() != "" {
		return a.fail(configError("ENVIRONMENT_CREDENTIAL_ACTIVE", "Unset FLINT_WORKLOAD_IDENTITY_ID before signup; it would override the saved credential.", nil), opts)
	}
	// Validate the local destination before creating remote account resources or
	// issuing a one-time API key that this process may be unable to persist.
	resolved, cfg, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	previousStore := a.credentialStore
	defer func() { a.credentialStore = previousStore }()
	savedSignup := cfg.Profiles[resolved.ProfileName].PendingSignup
	retryBaseURL := ""
	if lastSignupOption(opts, "verification-code") != "" && savedSignup != nil {
		retryBaseURL = savedSignup.BaseURL
		if savedSignup.CredentialStore != "" && savedSignup.CredentialStore != "keyring" && savedSignup.CredentialStore != "file" {
			return a.fail(configError("CONFIG_INVALID", "The pending signup credential_store must be keyring or file.", nil), opts)
		}
		if a.credentialStore == "" {
			a.credentialStore = savedSignup.CredentialStore
		}
	}
	if a.credentialStore == "" && cfg.Profiles[resolved.ProfileName].CredentialStore == "file" {
		a.credentialStore = "file"
	}
	project, _, err := a.findProjectConfig()
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	if project.ContextID != "" {
		return a.fail(configError("SIGNUP_PROJECT_CONTEXT", "This project pins an OAuth context in .flint/config.json. Run signup outside the project or remove the pinned context before signing up.", nil), opts)
	}
	_, previousErr := a.loadSavedCredential(resolved.ProfileName)
	if previousErr != nil {
		return a.fail(a.signupCredentialError("CREDENTIAL_LOOKUP_FAILED", "The existing profile credential could not be read before signup.", previousErr, resolved.ProfileName, retryBaseURL, a.credentialStore), opts)
	}
	reader := bufio.NewReader(&contextInputReader{ctx: a.commandContext(), reader: a.Stdin})
	pending, e := a.signupChallenge(reader, opts, resolved.ProfileName, savedSignup)
	if e != nil {
		return a.fail(e, opts)
	}
	if lastSignupOption(opts, "verification-code") == "" && (opts.NoInput || !a.IsTTY()) {
		return a.signupVerificationRequired(resolved.ProfileName, pending.BaseURL, opts)
	}
	verify, e := a.verifySignupChallenge(reader, opts, resolved.ProfileName, pending)
	if e != nil {
		return a.fail(e, opts)
	}
	baseURL, email := pending.BaseURL, pending.Email
	sessionToken, ok := firstStringAt(verify.Value, "data.onboarding_session_token")
	if !ok {
		return a.fail(invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint did not return an onboarding session token.", nil), opts)
	}
	// Current verify responses identify the default sandbox directly. Older
	// servers expose it through onboarding state and readiness actions.
	state := verify
	if sandboxID, _ := firstStringAt(state.Value, "data.default_sandbox_id"); sandboxID == "" {
		state, e = a.doSignupRequest(baseURL, sessionToken, http.MethodGet, "/v1/onboarding/state", nil, "", opts)
		if e != nil {
			return a.fail(e, opts)
		}
		if _, legacy := lookupPath(state.Value, "data.can_issue_api_key"); legacy {
			state, e = a.advanceSignupUntilKeyAvailable(baseURL, sessionToken, state, reader, email, mergeSignupInputs(opts, pending.Inputs))
			if e != nil {
				return a.fail(e, opts)
			}
		}
	}

	sandboxID, _ := firstStringAt(state.Value, "data.default_sandbox_id")
	if sandboxID == "" {
		return a.fail(invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint did not return a default sandbox for the initial API key.", nil), opts)
	}
	// Mint explicitly in the default sandbox. Only an absent route permits the
	// compatibility fallback; authentication and grant failures must stop signup.
	keyBody, _ := json.Marshal(map[string]any{"name": "Flint CLI", "access": "full"})
	issueKey, err := newIdempotencyKey()
	if err != nil {
		return a.fail(networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate an idempotency key.", err), opts)
	}
	created, e := a.doSignupRequest(baseURL, sessionToken, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(sandboxID)+"/api-keys", keyBody, issueKey, opts)
	if e != nil && e.HTTPStatus == http.StatusNotFound {
		keyBody, _ = json.Marshal(map[string]any{"name": "Flint CLI", "sandbox_id": sandboxID, "access": "full"})
		created, e = a.doSignupRequest(baseURL, sessionToken, http.MethodPost, "/v1/api-keys", keyBody, issueKey, opts)
	}
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
			persistenceErr = networkError("CLIENT_CANCELED", "Signup was canceled before the credential was stored.", a.Context.Err())
			return persistenceErr
		}
		cfg, loadErr := a.loadConfig()
		if loadErr != nil {
			persistenceErr = configError("CONFIG_INVALID", loadErr.Error(), loadErr)
			return loadErr
		}
		profile := resolved.ProfileName
		p := cfg.Profiles[profile]
		persistence := *a
		persistence.credentialStore = a.selectedCredentialStore(profile, p)
		previousCredential, credentialErr := a.loadSavedCredentialUnlocked(profile)
		if credentialErr != nil {
			persistenceErr = a.signupCredentialError("CREDENTIAL_LOOKUP_FAILED", "The existing profile credential could not be read before signup.", credentialErr, profile, baseURL, persistence.credentialStore)
			return credentialErr
		}
		storeErr := persistence.storeSavedCredential(profile, secret)
		keyringUnavailable := persistence.credentialStore == "keyring" && keyringUnavailableBeforeWrite(storeErr)
		if keyringUnavailable && a.credentialStore == "" && previousCredential == "" &&
			(p.CredentialPresent == nil || !*p.CredentialPresent) {
			persistence.credentialStore = "file"
			fileCredential, fileErr := persistence.rawSavedCredential(profile, p)
			if fileErr != nil {
				persistenceErr = a.signupCredentialError("CREDENTIAL_LOOKUP_FAILED", "The fallback credential file could not be read before signup.", fileErr, profile, baseURL, "file")
				return fileErr
			}
			if fileCredential != "" {
				persistenceErr = configError("KEYCHAIN_WRITE_FAILED", "The OS keyring is unavailable and this profile already has a credential in the file store. To replace it, retry with --credential-store file.", storeErr)
				persistenceErr.Details = a.signupFileRetry(profile, baseURL)
				return storeErr
			}
			storeErr = persistence.storeSavedCredential(profile, secret)
			keyringUnavailable = false
		}
		if storeErr != nil {
			persistenceErr = a.signupCredentialError("KEYCHAIN_WRITE_FAILED", "The new credential could not be stored.", storeErr, profile, baseURL, persistence.credentialStore)
			if !keyringUnavailable {
				if restoreErr := persistence.restoreCredential(profile, previousCredential); restoreErr != nil {
					persistenceErr.Message += " The previous local credential could not be restored: " + terminalSafe(restoreErr.Error()) + "."
				}
			}
			return storeErr
		}
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]Profile{}
		}
		p.ContextID = ""
		p.MerchantGuard = ""
		if sameSignupChallenge(p.PendingSignup, pending) {
			p.PendingSignup = nil
		}
		p.MerchantID = ""
		p.Environment = "sandbox"
		p.SandboxID = sandboxID
		p.APIKeyID = apiKeyID
		p.bindAPIKeyServer(persistence.credentialStore, secret, baseURL)
		if merchant, ok := firstStringAt(created.Value, "data.merchant_id"); ok {
			p.MerchantID = merchant
		}
		cfg.Profiles[profile] = persistence.profileWithCredential(profile, p, true)
		if saveErr := a.saveConfigUnlocked(cfg); saveErr != nil {
			persistenceErr = configError("CONFIG_WRITE_FAILED", saveErr.Error(), saveErr)
			if restoreErr := persistence.restoreCredential(profile, previousCredential); restoreErr != nil {
				persistenceErr.Message += " The previous local credential could not be restored: " + terminalSafe(restoreErr.Error()) + "."
			}
			return saveErr
		}
		a.credentialStore = persistence.credentialStore
		return nil
	})
	if lockErr != nil {
		if persistenceErr == nil {
			persistenceErr = configError("CONFIG_WRITE_FAILED", lockErr.Error(), lockErr)
		}
		cleanupErr := a.revokeIssuedSignupKey(baseURL, secret, apiKeyID, opts)
		return a.fail(withSignupKeyCleanupResult(persistenceErr, cleanupErr, apiKeyID), opts)
	}
	a.credentialSavedNotice(resolved.ProfileName)
	if m, ok := created.Value.(map[string]any); ok {
		if data, ok := m["data"].(map[string]any); ok {
			delete(data, "secret_key")
		}
	}
	nextCommand := a.signupCommand(signupNextCommand, resolved.ProfileName, baseURL)
	merchant := signupMerchantSummary(verify.Value)
	guidance := "To change the shared business profile, edit it in the Dashboard, or run flint login --live --new-session, approve live access, then run flint merchant update --context <live context ID> --business-name <name> --confirm."
	warnings := []string{}
	for _, flag := range signupCreationProfileFlags {
		if values := pending.Inputs[flag]; len(values) > 0 {
			want := values[len(values)-1]
			if flag == "business-name" || flag == "timezone" {
				want = strings.TrimSpace(want)
			}
			if merchant[strings.ReplaceAll(flag, "-", "_")] != want {
				warnings = append(warnings, "Requested --"+flag+" was not applied. Signup preserves an existing merchant's profile. Check the returned merchant details.")
			}
		}
	}
	result := map[string]any{"data": map[string]any{
		"api_key":          valueAt(created.Value, "data"),
		"onboarding":       signupOnboardingSummary(state.Value),
		"credential_saved": true,
		"merchant":         merchant,
		"profile_guidance": guidance,
		"profile_warnings": warnings,
		"next_command":     nextCommand,
	}}
	if opts.Output == "human" && opts.Field == "" && len(opts.Select) == 0 && opts.JQ == "" {
		if !opts.Quiet {
			if name := merchant["business_name"]; name != "" {
				fmt.Fprintf(a.Stdout, "Business name buyers see: %s\n", name)
			}
			if zone := merchant["timezone"]; zone != "" {
				fmt.Fprintf(a.Stdout, "Business time zone: %s\n", zone)
			}
			for _, warning := range warnings {
				fmt.Fprintln(a.Stdout, warning)
			}
			fmt.Fprintln(a.Stdout, guidance)
			if _, err := fmt.Fprintf(a.Stdout, "You're signed up. Your sandbox is ready for test payments.\n\nMake a test payment:\n  %s\n\nPay with card 4242 4242 4242 4242, any future expiry date, and any CVC.\n", nextCommand); err != nil {
				return a.fail(networkError("OUTPUT_WRITE_FAILED", "Could not display signup instructions.", err), opts)
			}
		}
		return ExitOK
	}
	return a.outputLocal(result, nilCommand("signup"), opts)
}

// Verification results can include a bearer token. Publish only known scalar
// onboarding facts so all output formats and projections share the safe value.
func signupOnboardingSummary(value any) map[string]any {
	summary := map[string]any{}
	for _, field := range []string{"status", "default_sandbox_id"} {
		if fact, ok := firstStringAt(value, "data."+field); ok {
			summary[field] = fact
		}
	}
	if fact, ok := valueAt(value, "data.can_issue_api_key").(bool); ok {
		summary["can_issue_api_key"] = fact
	}
	return summary
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
		endpoint, issueKey, stepErr := signupStepEndpoint(baseURL, step, state)
		if stepErr != nil {
			return nil, stepErr
		}
		if issueKey {
			return state, nil
		}
		stepKey := signupStepKey(step, endpoint)
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

// signupOnboardingAdvancePath is where every integrator-owned
// advance_onboarding action is submitted.
const signupOnboardingAdvancePath = "/v1/onboarding/advance"

// signupStepEndpoint decides what signup does with the onboarding next step.
// It returns the endpoint to submit an automatable step to, or issueKey when
// the step is to create the initial API key.
//
// Flint describes the step as a readiness action (action_type and owner).
// Servers that predate that shape send code, machine_completable,
// submit_method, and submit_endpoint instead; those are still honored when
// action_type is absent.
func signupStepEndpoint(baseURL string, step map[string]any, state *apiResponse) (endpoint string, issueKey bool, err *CLIError) {
	actionType, _ := step["action_type"].(string)
	actionType = strings.TrimSpace(actionType)
	if actionType == "" {
		return signupLegacyStepEndpoint(baseURL, step, state)
	}
	owner, _ := step["owner"].(string)
	if owner = strings.TrimSpace(owner); owner != "" && owner != "integrator" {
		return "", false, signupHumanActionRequired(state)
	}
	switch actionType {
	case "advance_onboarding":
		return signupOnboardingAdvancePath, false, nil
	case "create_api_key":
		return "", true, nil
	default:
		// create_merchant_account_session needs the account owner in embedded
		// onboarding. Any other action is one this CLI cannot complete.
		return "", false, signupHumanActionRequired(state)
	}
}

func signupLegacyStepEndpoint(baseURL string, step map[string]any, state *apiResponse) (string, bool, *CLIError) {
	if code, _ := step["code"].(string); strings.TrimSpace(code) == "create_api_key" {
		return "", true, nil
	}
	machineCompletable, _ := step["machine_completable"].(bool)
	if !machineCompletable {
		return "", false, signupHumanActionRequired(state)
	}
	method, _ := step["submit_method"].(string)
	if method == "" {
		method = http.MethodPost
	}
	if !strings.EqualFold(method, http.MethodPost) {
		return "", false, signupStateError("INVALID_SIGNUP_RESPONSE", "Flint returned an unsupported onboarding step method.", state)
	}
	rawEndpoint, _ := step["submit_endpoint"].(string)
	endpoint, endpointErr := signupPublicEndpoint(baseURL, rawEndpoint)
	if endpointErr != nil {
		return "", false, invalidResponseError("INVALID_SIGNUP_RESPONSE", endpointErr.Error(), endpointErr)
	}
	return endpoint, false, nil
}

func signupHumanActionRequired(state *apiResponse) *CLIError {
	return signupStateError(
		"SIGNUP_HUMAN_ACTION_REQUIRED",
		"Initial key issuance is blocked on a human onboarding action. Complete the returned onboarding step, then retry signup.",
		state,
	)
}

// signupStepKey identifies a submitted step so signup can stop when Flint
// returns it again unchanged. Readiness actions reuse advance_onboarding for
// every profile step, so the fields it asks for are part of the identity.
func signupStepKey(step map[string]any, endpoint string) string {
	name, _ := step["action_type"].(string)
	if strings.TrimSpace(name) == "" {
		name, _ = step["code"].(string)
	}
	fields := []string{}
	if values, ok := step["required_fields"].([]any); ok {
		for _, raw := range values {
			if field, ok := raw.(string); ok {
				fields = append(fields, strings.TrimSpace(field))
			}
		}
	}
	sort.Strings(fields)
	return strings.TrimSpace(name) + "|" + endpoint + "|" + strings.Join(fields, ",")
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
		authContext, err := cleanup.doSignupRequest(baseURL, secret, http.MethodGet, "/v1/auth-context", nil, "", opts)
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

func (a *App) signupCredentialError(code, message string, err error, profile, baseURL, store string) *CLIError {
	message = strings.TrimSuffix(message, ".") + ": " + terminalSafe(err.Error()) + "."
	if store == "file" {
		return configError(code, message, err)
	}
	result := configError(code, message+" To use the private credential file, retry with --credential-store file.", err)
	result.Details = a.signupFileRetry(profile, baseURL)
	return result
}

func (a *App) signupFileRetry(profile, baseURL string) map[string]any {
	retry := *a
	retry.credentialStore = "file"
	command := "flint signup"
	if baseURL != "" {
		command += " --verification-code <code>"
	} else {
		baseURL = defaultAPIBaseURL
	}
	command = retry.signupCommand(command, profile, baseURL)
	return map[string]any{"remediation": map[string]any{"next_actions": []any{
		map[string]any{"command": command},
	}}}
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
	response, e := a.doRequest(ctx, baseURL, token, method, path, body, idempotencyKey, opts.Debug)
	if e != nil {
		return nil, signupAccountError(e)
	}
	return response, nil
}

func (a *App) signupValue(reader *bufio.Reader, opts Options, flagName, label string) (string, *CLIError) {
	if values := opts.Raw[flagName]; len(values) > 0 {
		if value := strings.TrimSpace(values[len(values)-1]); value != "" {
			return value, nil
		}
	}
	if opts.NoInput || !a.IsTTY() {
		return "", usageError("SIGNUP_INPUT_REQUIRED", "flint signup requires --"+flagName+" when input is disabled.", flagName)
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
