package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const signupNextCommand = `flint payment-links create --name "Test payment" --item-name "Test payment" --amount 1000 --currency USD --open`

// SignupChallenge is saved only in the private profile config, never in command
// output. Bind the token to its API origin so a resumed run cannot disclose it
// to a different server.
type SignupChallenge struct {
	VerificationToken string              `json:"verification_token"`
	Email             string              `json:"email"`
	FirstName         string              `json:"first_name"`
	LastName          string              `json:"last_name"`
	BaseURL           string              `json:"base_url"`
	ExpiresAt         time.Time           `json:"expires_at,omitempty"`
	Attempts          int                 `json:"attempts,omitempty"`
	Inputs            map[string][]string `json:"inputs,omitempty"`
	VerificationKey   string              `json:"verification_key,omitempty"`
	VerificationCode  string              `json:"verification_code,omitempty"`
	SessionToken      string              `json:"onboarding_session_token,omitempty"`
	SessionExpiresAt  time.Time           `json:"onboarding_session_expires_at,omitempty"`
}

func sameSignupChallenge(left, right *SignupChallenge) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.VerificationToken == right.VerificationToken && left.BaseURL == right.BaseURL
}

func signupChallengeChanged() *CLIError {
	return configError("SIGNUP_CHALLENGE_CHANGED", "Signup progress changed in another terminal. Resume the current challenge with flint signup --verification-code <code>.", nil)
}

func (a *App) replaceSignupChallenge(profile string, expected, pending *SignupChallenge) *CLIError {
	if err := a.commandContext().Err(); err != nil {
		return networkError("REQUEST_CANCELED", "Signup was canceled before progress was stored.", err)
	}
	if err := a.updateConfig(func(cfg *Config) error {
		p := cfg.Profiles[profile]
		if !sameSignupChallenge(p.PendingSignup, expected) {
			return signupChallengeChanged()
		}
		p.PendingSignup = pending
		cfg.Profiles[profile] = p
		return nil
	}); err != nil {
		if e, ok := err.(*CLIError); ok {
			return e
		}
		return configError("CONFIG_WRITE_FAILED", "Could not save signup progress locally. Start again with flint signup.", err)
	}
	return nil
}

func (a *App) signupChallenge(reader *bufio.Reader, opts Options, profile string, saved *SignupChallenge) (*SignupChallenge, *CLIError) {
	baseURL := strings.TrimSpace(os.Getenv("FLINT_BASE_URL"))
	if lastSignupOption(opts, "verification-code") != "" && saved != nil {
		if saved.VerificationToken == "" || saved.Email == "" || saved.FirstName == "" || saved.LastName == "" || saved.BaseURL == "" {
			return nil, configError("SIGNUP_VERIFICATION_MISSING", "Saved signup progress is incomplete. Start again with flint signup without --verification-code.", nil)
		}
		if baseURL == "" {
			baseURL = saved.BaseURL
		}
		validated, err := validateBaseURL(baseURL)
		if err != nil || validated != saved.BaseURL {
			return nil, configError("SIGNUP_API_MISMATCH", "Saved signup belongs to a different API server. Use the original FLINT_BASE_URL or start again with flint signup without --verification-code.", nil)
		}
		for flag, value := range map[string]string{"email": saved.Email, "first-name": saved.FirstName, "last-name": saved.LastName} {
			if input := lastSignupOption(opts, flag); input != "" && input != value {
				return nil, usageError("SIGNUP_INPUT_MISMATCH", "--"+flag+" differs from saved signup progress. Start again without --verification-code to use new details.", flag)
			}
		}
		return saved, nil
	}
	if lastSignupOption(opts, "verification-code") != "" && (lastSignupOption(opts, "email") == "" || lastSignupOption(opts, "first-name") == "" || lastSignupOption(opts, "last-name") == "") {
		return nil, configError("SIGNUP_VERIFICATION_MISSING", "No saved email verification is available for this profile. Start again with flint signup without --verification-code (provide --email, --first-name and --last-name with --no-input).", nil)
	}
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}
	baseURL, err := validateBaseURL(baseURL)
	if err != nil {
		return nil, configError("INVALID_BASE_URL", err.Error(), err)
	}
	email, e := a.signupValue(reader, opts, "email", "Email")
	if e != nil {
		return nil, e
	}
	if !strings.Contains(email, "@") {
		return nil, usageError("INVALID_EMAIL", "Enter a valid email address.", "email")
	}
	first, e := a.signupValue(reader, opts, "first-name", "First name")
	if e != nil {
		return nil, e
	}
	last, e := a.signupValue(reader, opts, "last-name", "Last name")
	if e != nil {
		return nil, e
	}
	return a.startSignupChallenge(profile, &SignupChallenge{Email: email, FirstName: first, LastName: last, BaseURL: baseURL, Inputs: signupInputs(opts)}, saved, opts)
}

func (a *App) startSignupChallenge(profile string, details, expected *SignupChallenge, opts Options) (*SignupChallenge, *CLIError) {
	body, _ := json.Marshal(map[string]any{"email": details.Email, "first_name": details.FirstName, "last_name": details.LastName})
	key, err := newIdempotencyKey()
	if err != nil {
		return nil, networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate an idempotency key.", err)
	}
	start, e := a.doSignupRequest(details.BaseURL, "", http.MethodPost, "/v1/onboarding/start", body, key, opts)
	if e != nil {
		return nil, e
	}
	token, ok := firstStringAt(start.Value, "data.verification_token")
	if !ok {
		return nil, invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint did not return an email verification token.", nil)
	}
	pending := *details
	pending.VerificationToken, pending.Attempts, pending.ExpiresAt = token, 0, time.Time{}
	pending.VerificationKey, pending.VerificationCode = "", ""
	pending.SessionToken, pending.SessionExpiresAt = "", time.Time{}
	if expires, ok := firstStringAt(start.Value, "data.expires_at"); ok {
		pending.ExpiresAt, err = time.Parse(time.RFC3339, expires)
		if err != nil {
			return nil, invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint returned an invalid verification expiry.", err)
		}
	}
	if e := a.replaceSignupChallenge(profile, expected, &pending); e != nil {
		return nil, e
	}
	return &pending, nil
}

func (a *App) signupVerificationRequired(profile string, baseURL string, opts Options) int {
	next := a.signupCommand("flint signup --verification-code <code>", profile, baseURL)
	if opts.Output == "human" && opts.Field == "" && len(opts.Select) == 0 && opts.JQ == "" {
		if !opts.Quiet {
			if _, err := fmt.Fprintf(a.Stdout, "A verification code was sent to your email. Finish signup with:\n  %s\n", next); err != nil {
				return a.fail(networkError("OUTPUT_WRITE_FAILED", "Could not display verification instructions.", err), opts)
			}
		}
		return ExitOK
	}
	return a.outputLocal(map[string]any{"data": map[string]any{"status": "verification_required", "message": "A verification code was sent to your email.", "next_command": next}}, nilCommand("signup"), opts)
}

func (a *App) verifySignupChallenge(reader *bufio.Reader, opts Options, profile string, pending *SignupChallenge) (*apiResponse, *CLIError) {
	interactive := !opts.NoInput && a.IsTTY()
	for {
		if pending.SessionToken != "" {
			if !pending.SessionExpiresAt.IsZero() && !a.Now().Before(pending.SessionExpiresAt) {
				return nil, configError("SIGNUP_SESSION_EXPIRED", "Your onboarding session has expired. Start again with flint signup without --verification-code.", nil)
			}
			return &apiResponse{Value: map[string]any{"data": map[string]any{"onboarding_session_token": pending.SessionToken}}}, nil
		}
		var verificationErr *CLIError
		switch {
		case pending.VerificationKey == "" && !pending.ExpiresAt.IsZero() && !a.Now().Before(pending.ExpiresAt):
			verificationErr = configError("SIGNUP_VERIFICATION_EXPIRED", "Your email verification code has expired. Start again with flint signup without --verification-code to send a new code.", nil)
		case pending.VerificationKey == "" && pending.Attempts >= 5:
			return nil, configError("VERIFICATION_ATTEMPTS_EXCEEDED", "All 5 verification attempts have been used. Start again with flint signup without --verification-code to send a new code.", nil)
		default:
			code, e := a.signupValue(reader, opts, "verification-code", "Verification code")
			if e != nil {
				return nil, e
			}
			verify, e, replayedCode := a.verifySignupAttempt(profile, pending, code, opts)
			if e == nil {
				return verify, nil
			}
			if signupExistingAccount(e) {
				return nil, e
			}
			// Replay the exact unresolved request first. If it confirmed an old
			// wrong code, the new code supplied by this run is still unused.
			incorrect, _ := signupVerificationFailure(e)
			if replayedCode != "" && replayedCode != code && incorrect && pending.Attempts < 5 {
				continue
			}
			verificationErr = e
		}
		incorrect, expired := signupVerificationFailure(verificationErr)
		if expired {
			if !interactive {
				return nil, configError("SIGNUP_VERIFICATION_EXPIRED", "Your email verification code has expired. Start again with flint signup without --verification-code to send a new code.", nil)
			}
			fmt.Fprint(a.Stderr, "Your email verification code has expired. Send a new code? [y/N]: ")
			answer, err := reader.ReadString('\n')
			if err != nil || (strings.ToLower(strings.TrimSpace(answer)) != "y" && strings.ToLower(strings.TrimSpace(answer)) != "yes") {
				return nil, configError("SIGNUP_VERIFICATION_EXPIRED", "Start again with flint signup without --verification-code to send a new code.", err)
			}
			fresh, e := a.startSignupChallenge(profile, pending, pending, opts)
			if e != nil {
				return nil, e
			}
			*pending = *fresh
			fmt.Fprintln(a.Stderr, "A new verification code was sent to your email.")
		} else {
			if verificationErr.Code == "VERIFICATION_ATTEMPTS_EXCEEDED" || (incorrect && pending.Attempts >= 5) {
				return nil, configError("VERIFICATION_ATTEMPTS_EXCEEDED", "All 5 verification attempts have been used. Start again with flint signup without --verification-code to send a new code.", nil)
			}
			if !incorrect || !interactive {
				return nil, verificationErr
			}
			fmt.Fprintf(a.Stderr, "That verification code is incorrect. %d attempts remaining.\n", 5-pending.Attempts)
		}
		// A code supplied by flag must be consumed only once, then prompt again.
		opts.Raw = copySignupOptions(opts.Raw)
		delete(opts.Raw, "verification-code")
	}
}

// Keep only onboarding inputs, never unrelated command flags or credentials.
var signupInputFlags = []string{"country", "website-url", "support-email", "support-phone", "support-url", "requested-capability"}

func signupInputs(opts Options) map[string][]string {
	inputs := map[string][]string{}
	for _, flag := range signupInputFlags {
		if values, ok := opts.Raw[flag]; ok {
			inputs[flag] = append([]string(nil), values...)
		}
	}
	return inputs
}

func mergeSignupInputs(opts Options, inputs map[string][]string) Options {
	opts.Raw = copySignupOptions(opts.Raw)
	for _, flag := range signupInputFlags {
		if _, overridden := opts.Raw[flag]; !overridden {
			if values, ok := inputs[flag]; ok {
				opts.Raw[flag] = append([]string(nil), values...)
			}
		}
	}
	return opts
}

func (a *App) signupCommand(command, profile, baseURL string) string {
	unflagged, _, err := a.resolveConfig(Options{})
	if profile != "default" || err != nil || profile != unflagged.ProfileName {
		command += " --profile '" + strings.ReplaceAll(profile, "'", "'\"'\"'") + "'"
	}
	// Raw API keys do not carry an origin in the credential store. Include the
	// origin in continuation instructions so they survive environment changes.
	if baseURL != defaultAPIBaseURL {
		command = "FLINT_BASE_URL='" + strings.ReplaceAll(baseURL, "'", "'\"'\"'") + "' " + command
	}
	return command
}

func signupVerificationFailure(e *CLIError) (incorrect, expired bool) {
	if e.ExitCode == ExitNetwork {
		return false, false
	}
	text := strings.ToLower(e.Code + " " + e.Message)
	expired = strings.Contains(text, "expired") && (strings.Contains(text, "verif") || strings.Contains(text, "code") || strings.Contains(text, "token"))
	incorrect = e.Code == "INVALID_VERIFICATION" || ((strings.Contains(text, "code") || strings.Contains(text, "verification")) && (strings.Contains(text, "incorrect") || strings.Contains(text, "invalid") || strings.Contains(text, "wrong")))
	return
}

// Serialize requests as well as counter updates. Another process can neither
// submit the same unresolved request concurrently nor replace its challenge
// while its response is being recorded. Human input happens outside the lock.
func (a *App) verifySignupAttempt(profile string, pending *SignupChallenge, code string, opts Options) (response *apiResponse, verificationErr *CLIError, replayedCode string) {
	err := a.withConfigLock(func() error {
		cfg, err := a.loadConfig()
		if err != nil {
			return err
		}
		p := cfg.Profiles[profile]
		if !sameSignupChallenge(p.PendingSignup, pending) {
			return signupChallengeChanged()
		}
		current := p.PendingSignup
		*pending = *current
		if current.SessionToken != "" {
			if !current.SessionExpiresAt.IsZero() && !a.Now().Before(current.SessionExpiresAt) {
				return configError("SIGNUP_SESSION_EXPIRED", "Your onboarding session has expired. Start again with flint signup without --verification-code.", nil)
			}
			response = &apiResponse{Value: map[string]any{"data": map[string]any{"onboarding_session_token": current.SessionToken}}}
			return nil
		}
		if current.VerificationKey == "" {
			if current.Attempts >= 5 {
				return configError("VERIFICATION_ATTEMPTS_EXCEEDED", "All 5 verification attempts have been used. Start again with flint signup without --verification-code to send a new code.", nil)
			}
			if !current.ExpiresAt.IsZero() && !a.Now().Before(current.ExpiresAt) {
				return configError("SIGNUP_VERIFICATION_EXPIRED", "Your email verification code has expired. Start again with flint signup without --verification-code.", nil)
			}
			key, err := newIdempotencyKey()
			if err != nil {
				return networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate an idempotency key.", err)
			}
			current.VerificationKey, current.VerificationCode = key, code
		} else {
			replayedCode = current.VerificationCode
		}
		current.Inputs = signupInputs(mergeSignupInputs(opts, current.Inputs))
		// Persist the exact request before sending it, even if this process
		// later times out or is interrupted before receiving the response.
		if err := a.saveConfigUnlocked(cfg); err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]any{"verification_token": current.VerificationToken, "verification_code": current.VerificationCode})
		response, verificationErr = a.doSignupRequest(current.BaseURL, "", http.MethodPost, "/v1/onboarding/verify-email", body, current.VerificationKey, opts)
		confirmed := verificationErr == nil || (verificationErr.HTTPStatus >= 400 && verificationErr.HTTPStatus < 500 && verificationErr.HTTPStatus != http.StatusTooManyRequests)
		if verificationErr == nil {
			token, ok := firstStringAt(response.Value, "data.onboarding_session_token")
			if !ok {
				return invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint did not return an onboarding session token.", nil)
			}
			current.SessionToken = token
			if expiry, ok := firstStringAt(response.Value, "data.onboarding_session_expires_at"); ok {
				current.SessionExpiresAt, err = time.Parse(time.RFC3339, expiry)
				if err != nil {
					return invalidResponseError("INVALID_SIGNUP_RESPONSE", "Flint returned an invalid onboarding session expiry.", err)
				}
			}
		}
		if confirmed {
			if verificationErr == nil {
				current.Attempts++
			} else {
				incorrect, expired := signupVerificationFailure(verificationErr)
				// Schema validation failures do not consume a server code attempt.
				if incorrect && verificationErr.Code != "INVALID_REQUEST" {
					current.Attempts++
				}
				if expired {
					current.ExpiresAt = a.Now()
				}
				if verificationErr.Code == "VERIFICATION_ATTEMPTS_EXCEEDED" {
					current.Attempts = 5
				}
			}
			current.VerificationKey, current.VerificationCode = "", ""
		}
		*pending = *current
		return a.saveConfigUnlocked(cfg)
	})
	if err != nil {
		if e, ok := err.(*CLIError); ok {
			verificationErr = e
		} else {
			verificationErr = configError("CONFIG_WRITE_FAILED", "Could not save signup verification progress.", err)
		}
	}
	return
}

func copySignupOptions(raw map[string][]string) map[string][]string {
	copy := make(map[string][]string, len(raw))
	for key, values := range raw {
		copy[key] = values
	}
	return copy
}

func signupExistingAccount(e *CLIError) bool {
	message := strings.ToLower(e.Message)
	return strings.Contains(message, "flint login") || strings.Contains(message, "flint auth login")
}

func signupAccountError(e *CLIError) *CLIError {
	if signupExistingAccount(e) {
		// The server's login instruction is definitive. Do not render signup
		// remediation attached to the error envelope.
		copy := *e
		if details, ok := e.Details.(map[string]any); ok {
			copy.Details = copyMap(details)
			delete(copy.Details.(map[string]any), "remediation")
		}
		return &copy
	}
	return e
}
