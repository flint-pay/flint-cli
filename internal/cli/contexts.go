package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const oauthContextsPath = "/v1/oauth/contexts"
const oauthReauthorizePath = "/v1/oauth/device/reauthorize"

type oauthIdentityKey struct{}
type oauthIdentity struct {
	Version                     int
	BaseURL, SessionID, GrantID string
}

func withOAuthIdentity(ctx context.Context, c oauthCredential) context.Context {
	return context.WithValue(ctx, oauthIdentityKey{}, oauthIdentity{Version: c.Version, BaseURL: c.BaseURL, SessionID: c.SessionID, GrantID: c.Auth.OAuthGrantID})
}
func validateOAuthIdentity(ctx context.Context, c oauthCredential) *CLIError {
	expected, ok := ctx.Value(oauthIdentityKey{}).(oauthIdentity)
	if !ok {
		return nil
	}
	if c.Version != expected.Version || c.BaseURL != expected.BaseURL || c.SessionID != expected.SessionID || (c.Version == 1 && c.Auth.OAuthGrantID != expected.GrantID) {
		return configError("OAUTH_SESSION_CHANGED", "The saved browser session changed. Retry the command.", nil)
	}
	return nil
}

type oauthContextKey struct{}

func withOAuthContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, oauthContextKey{}, id)
}
func requestedOAuthContext(ctx context.Context) string {
	id, _ := ctx.Value(oauthContextKey{}).(string)
	return id
}
func contextSessionRequired() *CLIError {
	return configError("CONTEXT_SESSION_REQUIRED", "Context selection requires a browser session with context support. Unset credential overrides and run flint reauth (or flint login if signed out).", nil)
}
func oauthHistoryScope(auth AuthContext) string {
	if auth.OAuthSessionID != "" {
		return "oauth:" + auth.OAuthSessionID + ":" + auth.ContextID
	}
	return "oauth:" + auth.OAuthGrantID
}
func matchesOAuthCredential(c oauthCredential, auth AuthContext) bool {
	if !validOAuthContext(auth) {
		return false
	}
	if c.Version == 2 {
		if auth.OAuthSessionID != c.SessionID || auth.ContextID != c.ContextID {
			return false
		}
		// A newly selected context is authorized by the server, not the previous
		// token's tenant. Refreshing the same context must preserve its identity.
		if c.Auth.ContextID != c.ContextID {
			return true
		}
	}
	return auth.OAuthGrantID == c.Auth.OAuthGrantID && auth.Environment == c.Auth.Environment && auth.MerchantID == c.Auth.MerchantID && auth.SandboxID == c.Auth.SandboxID
}

type authorizedContext struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MerchantID  string `json:"merchant_id"`
	Environment string `json:"environment"`
	SandboxID   string `json:"sandbox_id,omitempty"`
}
type contextList struct {
	SessionID string              `json:"oauth_session_id"`
	Contexts  []authorizedContext `json:"contexts"`
	Active    string              `json:"active_context_id"`
}

func (a *App) savedContextSession(opts Options) (ResolvedConfig, oauthCredential, *CLIError) {
	for _, name := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_CHECKOUT_SESSION_SECRET"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return ResolvedConfig{}, oauthCredential{}, configError("ENVIRONMENT_CREDENTIAL_ACTIVE", "Unset "+name+" to manage the saved browser session.", nil)
		}
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return resolved, oauthCredential{}, configError("CONFIG_INVALID", err.Error(), err)
	}
	raw, err := a.LoadCredential(resolved.ProfileName)
	if err != nil {
		return resolved, oauthCredential{}, configError("CREDENTIAL_LOOKUP_FAILED", "Could not read the saved session.", err)
	}
	if !isOAuthCredential(raw) {
		return resolved, oauthCredential{}, contextSessionRequired()
	}
	c, e := decodeOAuthCredential(raw)
	if e == nil {
		e = a.oauthBaseURL(c)
	}
	return resolved, c, e
}

// Control-plane requests authenticate the session directly, so contexts can
// still be listed when the cached access token's context has been removed.
// They never rotate the refresh token or grant access to business endpoints.
func (a *App) listOAuthContexts(ctx context.Context, profile string) (contextList, *CLIError) {
	var list contextList
	var result *CLIError
	locked := *a
	locked.Context = ctx
	err := locked.withConfigLock(func() error {
		raw, err := a.LoadCredential(profile)
		if err != nil {
			result = configError("CREDENTIAL_LOOKUP_FAILED", "Could not read the saved session.", err)
			return result
		}
		c, e := decodeOAuthCredential(raw)
		if e != nil {
			result = e
			return e
		}
		if e = a.oauthBaseURL(c); e != nil {
			result = e
			return e
		}
		if e = validateOAuthIdentity(ctx, c); e != nil {
			result = e
			return e
		}

		if c.Version != 2 {
			result = contextSessionRequired()
			return result
		}
		data, e := a.oauthRequest(ctx, c.BaseURL, oauthContextsPath, url.Values{"client_id": {oauthClientID}, "refresh_token": {c.RefreshToken}})
		if e != nil {
			result = contextEndpointError(e)
			return result
		}
		if json.Unmarshal(data, &list) != nil || list.SessionID != c.SessionID || list.Contexts == nil {
			result = configError("INVALID_CONTEXT_RESPONSE", "Flint returned an invalid authorized context list.", nil)
			return result
		}
		seen := map[string]bool{}
		for _, item := range list.Contexts {
			if item.ID == "" || seen[item.ID] || item.MerchantID == "" || (item.Environment != "sandbox" && item.Environment != "live") || (item.Environment == "sandbox" && item.SandboxID == "") || (item.Environment == "live" && item.SandboxID != "") {
				result = configError("INVALID_CONTEXT_RESPONSE", "Flint returned invalid or duplicate contexts.", nil)
				return result
			}
			seen[item.ID] = true
		}
		return nil
	})
	if err != nil && result == nil {
		result = configError("CONTEXT_LOOKUP_FAILED", "Could not lock the browser session.", err)
	}
	return list, result
}
func contextEndpointError(e *CLIError) *CLIError {
	if e.Code == "LOGIN_UNAVAILABLE" {
		return configError("CONTEXTS_UNAVAILABLE", "This Flint server does not support multi-context sessions yet. Your existing login remains usable.", nil)
	}
	if e.Code == "invalid_context" || e.Code == "context_access_denied" {
		return configError("CONTEXT_ACCESS_DENIED", "This context is unavailable. Run flint context list, select another context, or run flint reauth.", nil)
	}
	return e
}

func (a *App) localContext(cmd *Command, opts Options) int {
	resolved, c, e := a.savedContextSession(opts)
	if e != nil {
		return a.fail(e, opts)
	}
	if c.Version != 2 {
		return a.fail(contextSessionRequired(), opts)
	}
	ctx, cancel := context.WithTimeout(withOAuthIdentity(a.commandContext(), c), opts.Timeout)
	defer cancel()
	list, e := a.listOAuthContexts(ctx, resolved.ProfileName)
	if e != nil {
		return a.fail(e, opts)
	}
	if list.SessionID != c.SessionID {
		return a.fail(configError("OAUTH_SESSION_CHANGED", "The saved session changed. Retry the command.", nil), opts)
	}
	list.Active = resolved.ContextID
	if list.Active == "" {
		list.Active = c.ContextID
	}
	if cmd.CanonicalName == "context.list" {
		return a.outputLocal(map[string]any{"data": list}, cmd, opts)
	}
	id := ""
	interactive := len(opts.Positionals) == 0
	if !interactive {
		id = opts.Positionals[0]
	} else {
		if opts.NoInput || !a.IsTTY() {
			return a.fail(usageError("CONTEXT_REQUIRED", "Specify a context ID. Run flint context list to see authorized contexts.", "context_id"), opts)
		}
		if len(list.Contexts) == 0 {
			return a.fail(configError("NO_AUTHORIZED_CONTEXTS", "No contexts are authorized. Run flint reauth.", nil), opts)
		}
		for i, item := range list.Contexts {
			fmt.Fprintf(a.Stderr, "%d. %s / %s [%s] (%s)\n", i+1, terminalSafe(item.Name), terminalSafe(item.MerchantID), strings.ToUpper(item.Environment), terminalSafe(item.ID))
		}
		fmt.Fprint(a.Stderr, "Choose a context: ")
		line, err := bufio.NewReader(&contextInputReader{ctx: ctx, reader: a.Stdin}).ReadString('\n')
		if err != nil {
			return a.fail(configError("CONTEXT_READ_FAILED", "Could not read the context selection.", err), opts)
		}
		index, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || index < 1 || index > len(list.Contexts) {
			return a.fail(usageError("INVALID_CONTEXT", "Choose a number from the context list.", "context_id"), opts)
		}
		id = list.Contexts[index-1].ID
	}
	var selected *authorizedContext
	for i := range list.Contexts {
		if list.Contexts[i].ID == id {
			selected = &list.Contexts[i]
			break
		}
	}
	if selected == nil {
		return a.fail(configError("CONTEXT_ACCESS_DENIED", "This context is not authorized. Run flint reauth to change access.", nil), opts)
	}
	if selected.Environment == "live" && !opts.Live && !interactive {
		return a.fail(usageError("LIVE_ACKNOWLEDGEMENT_REQUIRED", "Selecting a live default requires flint context switch "+id+" --live.", "live"), opts)
	}
	intent := opts
	if interactive && selected.Environment == "live" {
		intent.Live = true
	}
	auth := AuthContext{Environment: selected.Environment, MerchantID: selected.MerchantID, SandboxID: selected.SandboxID}
	if e := validateCredentialIntent(auth, intent, resolved.MerchantGuard, resolved.SandboxGuard); e != nil {
		return a.fail(e, opts)
	}
	next, e := a.oauthAccess(withOAuthContext(ctx, id), resolved.ProfileName)
	if e != nil {
		return a.fail(e, opts)
	}
	if next.BaseURL != c.BaseURL || next.SessionID != list.SessionID || next.Auth.MerchantID != selected.MerchantID || next.Auth.Environment != selected.Environment || next.Auth.SandboxID != selected.SandboxID {
		return a.fail(configError("OAUTH_CONTEXT_MISMATCH", "The selected context changed. Retry the command.", nil), opts)
	}
	// Changing the default is separate from caching an access token. A command
	// with --context must never modify the user's default selection.
	locked := *a
	locked.Context = ctx
	err := locked.withConfigLock(func() error {
		raw, err := a.LoadCredential(resolved.ProfileName)
		if err != nil {
			return err
		}
		current, e := decodeOAuthCredential(raw)
		if e != nil {
			return e
		}
		if current.SessionID != next.SessionID || current.BaseURL != next.BaseURL {
			return fmt.Errorf("the saved session changed")
		}
		cfg, err := a.loadConfig()
		if err != nil {
			return err
		}
		p := cfg.Profiles[resolved.ProfileName]
		p.ContextID = id
		p.Environment = next.Auth.Environment
		p.MerchantID = next.Auth.MerchantID
		p.SandboxID = next.Auth.SandboxID
		cfg.Profiles[resolved.ProfileName] = p
		return a.saveConfigUnlocked(cfg)
	})
	if err != nil {
		return a.fail(configError("CONTEXT_SAVE_FAILED", "Could not save the active context.", err), opts)
	}
	if resolved.Sources["context"] == "project" {
		fmt.Fprintln(a.Stderr, "The project context overrides this profile default; update its .flint/config.json context to change this project's selection.")
	}
	return a.outputLocal(map[string]any{"data": map[string]any{"active_context": selected, "profile": resolved.ProfileName}}, cmd, opts)
}

func terminalSafe(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || (r >= 127 && r <= 159) {
			return -1
		}
		return r
	}, value)
}

func supportsContextSelection(cmd *Command) bool {
	if !cmd.Local {
		return true
	}
	switch cmd.CanonicalName {
	case "doctor", "init", "config.get", "config.validate", "history", "context.list":
		return true
	default:
		return false
	}
}
