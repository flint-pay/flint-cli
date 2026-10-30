package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func (a *App) runLocal(cmd *Command, opts Options) int {
	switch cmd.CanonicalName {
	case "version":
		return a.outputLocal(map[string]any{"data": map[string]any{
			"cli_version": a.Info.Version,
			"git_commit":  a.Info.Commit,
			"build_date":  a.Info.BuildDate,
			"api_version": a.Info.APIVersion,
			"schema_hash": a.Info.SchemaHash,
		}}, cmd, opts)
	case "upgrade":
		return a.localUpgrade(cmd, opts)
	case "config.get":
		return a.localConfigGet(cmd, opts)
	case "config.set":
		return a.localConfigSet(cmd, opts)
	case "config.unset":
		return a.localConfigUnset(cmd, opts)
	case "config.validate":
		return a.localConfigValidate(cmd, opts)
	case "history":
		return a.localHistory(cmd, opts)
	case "context.list", "context.switch":
		return a.localContext(cmd, opts)
	case "auth.login", "auth.reauth":
		return a.localAuthLogin(cmd, opts)
	case "auth.token":
		return a.localAuthToken(cmd, opts)
	case "auth.import":
		return a.localAuthImport(cmd, opts)
	case "auth.logout":
		return a.localLogout(cmd, opts)
	case "doctor":
		return a.localDoctor(cmd, opts)
	case "init":
		return a.localInit(cmd, opts)
	case "schema.commands", "schema.command", "schema.input", "schema.output", "schema.errors", "schema.events":
		return a.localSchema(cmd, opts)
	case "help":
		return a.localHelp(cmd, opts)
	case "help.search":
		return a.localHelpSearch(cmd, opts)
	case "support.open":
		return a.localSupportOpen(cmd, opts)
	case "mcp.serve":
		a.Registry.ensureAPICommands()
		return a.serveMCP(opts)
	case "signup":
		return a.localSignup(cmd, opts)
	default:
		return a.fail(cliError(ExitUsage, "usage_error", "NOT_IMPLEMENTED", "Local command is not implemented: "+cmd.Name), opts)
	}
}

func (a *App) outputLocal(value any, cmd *Command, opts Options) int {
	if err := a.writeResult(value, cmd, opts); err != nil {
		return a.fail(err, opts)
	}
	return ExitOK
}

func (a *App) localConfigGet(cmd *Command, opts Options) int {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	return a.outputLocal(map[string]any{"data": resolved}, cmd, opts)
}

func (a *App) localConfigSet(cmd *Command, opts Options) int {
	key, value := opts.Positionals[0], opts.Positionals[1]
	switch key {
	case "profile":
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, " /\\\t\r\n") {
			return a.fail(usageError("INVALID_PROFILE", "Profile name must not contain whitespace, slashes, or backslashes.", "value"), opts)
		}
		if err := a.updateConfig(func(cfg *Config) error {
			cfg.DefaultProfile = value
			if cfg.Profiles == nil {
				cfg.Profiles = map[string]Profile{}
			}
			if _, ok := cfg.Profiles[value]; !ok {
				cfg.Profiles[value] = profileWithoutCredential()
			}
			return nil
		}); err != nil {
			return a.fail(configError("CONFIG_WRITE_FAILED", err.Error(), err), opts)
		}
	case "merchant":
		if !strings.HasPrefix(value, "mer_") {
			return a.fail(usageError("INVALID_MERCHANT_ID", "Merchant guard must be a mer_ ID.", "value"), opts)
		}
		resolved, _, err := a.resolveConfig(opts)
		if err != nil {
			return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
		}
		if err := a.updateConfig(func(cfg *Config) error {
			if cfg.Profiles == nil {
				cfg.Profiles = map[string]Profile{}
			}
			p, exists := cfg.Profiles[resolved.ProfileName]
			if !exists {
				p = profileWithoutCredential()
			}
			p.MerchantGuard = value
			cfg.Profiles[resolved.ProfileName] = p
			return nil
		}); err != nil {
			return a.fail(configError("CONFIG_WRITE_FAILED", err.Error(), err), opts)
		}
	default:
		return a.fail(usageError("INVALID_CONFIG_KEY", "Config key must be profile or merchant.", "key"), opts)
	}
	return a.outputLocal(map[string]any{"data": map[string]any{"key": key, "value": value}}, cmd, opts)
}

func (a *App) localConfigUnset(cmd *Command, opts Options) int {
	key := opts.Positionals[0]
	if key != "merchant" {
		return a.fail(usageError("INVALID_CONFIG_KEY", "Config key must be merchant.", "key"), opts)
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	if err := a.updateConfig(func(cfg *Config) error {
		profile := cfg.Profiles[resolved.ProfileName]
		profile.MerchantGuard = ""
		cfg.Profiles[resolved.ProfileName] = profile
		return nil
	}); err != nil {
		return a.fail(configError("CONFIG_WRITE_FAILED", err.Error(), err), opts)
	}
	return a.outputLocal(map[string]any{"data": map[string]any{"key": key, "value": nil}}, cmd, opts)
}

func (a *App) localConfigValidate(cmd *Command, opts Options) int {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	checks := []map[string]any{{"name": "global_config", "status": "pass", "path": resolved.GlobalConfigPath}, {"name": "project_config", "status": "pass", "path": resolved.ProjectConfigPath}, {"name": "profile", "status": "pass", "value": resolved.ProfileName}}
	checks = append(checks, configUnknownFieldWarnings(resolved)...)
	key, source, credentialErr := a.resolveCheckCredential(resolved.ProfileName)
	if credentialErr != nil {
		return a.fail(configError("CREDENTIAL_LOOKUP_FAILED", credentialErr.Error(), credentialErr), opts)
	}
	if key != "" {
		ctx := withOAuthContext(a.commandContext(), resolved.ContextID)
		_, _, envelope, authErr := a.fetchCredentialContext(ctx, opts.Timeout, resolved.ProfileName, key, opts.Debug)
		authContext := envelope.Data
		if authErr != nil {
			return a.fail(authErr, opts)
		}
		if intentErr := validateResolvedCredentialIntent(authContext, opts, resolved, ""); intentErr != nil {
			return a.fail(intentErr, opts)
		}
		checks = append(checks, map[string]any{"name": "credential", "status": "pass", "source": source, "environment": authContext.Environment, "merchant_id": authContext.MerchantID})
	} else {
		checks = append(checks, map[string]any{"name": "credential", "status": "not_configured"})
	}
	return a.outputLocal(map[string]any{"data": checks}, cmd, opts)
}

func (a *App) localHistory(cmd *Command, opts Options) int {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	h, err := a.loadHistory()
	if err != nil {
		return a.fail(configError("HISTORY_READ_FAILED", err.Error(), err), opts)
	}
	if opts.Clear {
		if e := a.confirmLocal("Clear all Flint CLI resource history?", opts); e != nil {
			return a.fail(e, opts)
		}
		if err := a.updateHistory(func(history *History) error {
			history.Entries = nil
			return nil
		}); err != nil {
			return a.fail(configError("HISTORY_WRITE_FAILED", err.Error(), err), opts)
		}
		return a.outputLocal(map[string]any{"data": []any{}}, cmd, opts)
	}
	if key := credentialFromEnvironment(); key != "" {
		environment, err := credentialEnvironment(key)
		if err != nil {
			return a.fail(configError("CREDENTIAL_FORMAT_INVALID", err.Error(), err), opts)
		}
		// History is an offline view. An environment key supersedes the saved
		// key's context, whose merchant and sandbox may belong to another key.
		// Retain explicit guards, but do not guess the override key's identity.
		resolved.Environment = environment
		resolved.MerchantID = resolved.MerchantGuard
		resolved.SandboxID = resolved.SandboxGuard
	} else {
		if credential, _, credentialErr := a.resolveCredential(resolved.ProfileName); credentialErr == nil && credential != "" {
			if isOAuthCredential(credential) {
				c, e := decodeOAuthCredential(credential)
				if e != nil {
					return a.fail(e, opts)
				}
				if c.PendingValidation {
					return a.fail(configError("OAUTH_CONTEXT_UNAVAILABLE", "The saved token needs validation before reading history. Run flint auth status for the selected context first.", nil), opts)
				}
				resolved.Environment = c.Auth.Environment
				resolved.MerchantID = c.Auth.MerchantID
				resolved.SandboxID = c.Auth.SandboxID
				if resolved.ContextID != "" && (c.Version != 2 || c.ContextID != resolved.ContextID) {
					return a.fail(configError("CONTEXT_NOT_CACHED", "Run flint auth status --context "+resolved.ContextID+" before reading this context's history offline.", nil), opts)
				}
				resolved.CredentialScope = oauthHistoryScope(c.Auth)
			} else if resolved.Environment == "" {
				resolved.Environment, _ = credentialEnvironment(credential)
			}
		}
	}
	scope := historyScopeFromResolved(resolved)
	entries := []any{}
	for _, e := range h.Entries {
		if (resolved.Environment == "" && e.Profile == resolved.ProfileName) || historyEntryMatchesScope(e, scope) {
			entries = append(entries, map[string]any{
				"id":            e.ID,
				"resource_type": e.ResourceType,
				"command":       e.Command,
				"profile":       e.Profile,
				"environment":   e.Environment,
				"merchant_id":   e.MerchantID,
				"sandbox_id":    e.SandboxID,
				"created_at":    e.CreatedAt.Format(time.RFC3339Nano),
			})
		}
	}
	return a.outputLocal(map[string]any{"data": entries}, cmd, opts)
}

func (a *App) localAuthImport(cmd *Command, opts Options) int {
	opts.allowMissingProfile = true
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	var key string
	if opts.Stdin {
		raw, err := io.ReadAll(io.LimitReader(&contextInputReader{ctx: a.commandContext(), reader: a.Stdin}, 16<<10))
		if err != nil {
			return a.fail(configError("CREDENTIAL_READ_FAILED", "Could not read the credential from stdin.", err), opts)
		}
		key = strings.TrimSpace(string(raw))
	} else {
		if opts.NoInput || !a.IsTTY() {
			return a.fail(configError("CREDENTIAL_INPUT_REQUIRED", "flint auth import requires a TTY or --stdin.", nil), opts)
		}
		fmt.Fprint(a.Stderr, "Flint API key: ")
		if f, ok := a.Stdin.(*os.File); ok {
			raw, err := a.readPassword(f)
			fmt.Fprintln(a.Stderr)
			if err != nil {
				return a.fail(configError("CREDENTIAL_READ_FAILED", "Could not read the credential.", err), opts)
			}
			key = strings.TrimSpace(string(raw))
		} else {
			return a.fail(configError("CREDENTIAL_READ_FAILED", "Hidden credential input requires a terminal.", nil), opts)
		}
	}
	if _, err := credentialEnvironment(key); err != nil {
		return a.fail(configError("CREDENTIAL_FORMAT_INVALID", err.Error(), err), opts)
	}
	baseURL, err := a.baseURLForCredential(key)
	if err != nil {
		return a.fail(configError("INVALID_BASE_URL", err.Error(), err), opts)
	}
	ctx, cancel := context.WithTimeout(withOAuthContext(a.commandContext(), resolved.ContextID), opts.Timeout)
	defer cancel()
	authContext, e := a.fetchAuthContext(ctx, baseURL, key, opts.Debug)
	if e != nil {
		return a.fail(e, opts)
	}
	if intentErr := validateResolvedCredentialIntent(authContext, opts, resolved, ""); intentErr != nil {
		return a.fail(intentErr, opts)
	}
	if e := a.saveAuthenticatedCredential(resolved.ProfileName, key, authContext); e != nil {
		return a.fail(e, opts)
	}
	return a.outputLocal(map[string]any{"data": authContext}, cmd, opts)
}

func (a *App) saveAuthenticatedCredential(profile, key string, auth AuthContext) *CLIError {
	var result *CLIError
	err := a.withConfigLock(func() error {
		result = a.saveAuthenticatedCredentialUnlocked(profile, key, auth)
		if result != nil {
			return result
		}
		return nil
	})
	if err != nil && result == nil {
		result = configError("CONFIG_WRITE_FAILED", "Could not lock the profile.", err)
	}
	return result
}

func profileWithAuth(p Profile, auth AuthContext) Profile {
	p.ContextID = auth.ContextID
	p.Environment = normalizeEnvironment(auth.Environment)
	p.APIKeyID = auth.APIKeyID
	p.MerchantID = auth.MerchantID
	p.SandboxID = auth.SandboxID
	return p
}

func (a *App) saveAuthenticatedCredentialUnlocked(profile, key string, auth AuthContext) *CLIError {
	return a.saveCredentialProfileUnlocked(profile, key, func(p Profile) Profile { return profileWithAuth(p, auth) })
}

func (a *App) saveReauthorizedCredentialUnlocked(resolved ResolvedConfig, key string, auth AuthContext) *CLIError {
	return a.saveCredentialProfileUnlocked(resolved.ProfileName, key, func(p Profile) Profile {
		// Reauthorization changes consent, not a newer selection made in another
		// terminal. Project pins and explicit contexts must not overwrite the
		// profile's default either.
		if p.ContextID != resolved.ProfileContextID || resolved.Sources["context"] == "project" || resolved.Sources["context"] == "flag" {
			return p
		}
		return profileWithAuth(p, auth)
	})
}

func (a *App) saveCredentialProfileUnlocked(profile, key string, updateProfile func(Profile) Profile) *CLIError {
	var persistenceErr *CLIError
	lockErr := func() error {
		if err := a.commandContext().Err(); err != nil {
			persistenceErr = networkError("CLIENT_CANCELED", "Authentication was canceled before the credential was stored.", err)
			return persistenceErr
		}
		cfg, loadErr := a.loadConfig()
		if loadErr != nil {
			persistenceErr = configError("CONFIG_INVALID", loadErr.Error(), loadErr)
			return loadErr
		}
		persistence := *a
		persistence.credentialStore = a.selectedCredentialStore(profile, cfg.Profiles[profile])
		previousCredential, previousErr := a.loadSavedCredentialUnlocked(profile)
		if previousErr != nil {
			persistenceErr = credentialStoreError("CREDENTIAL_LOOKUP_FAILED", "The existing profile credential could not be read before authentication.", previousErr)
			return previousErr
		}
		if storeErr := persistence.storeSavedCredential(profile, key); storeErr != nil {
			persistenceErr = credentialStoreError("KEYCHAIN_WRITE_FAILED", "The validated credential could not be stored in the selected credential store.", storeErr)
			if restoreErr := persistence.restoreCredential(profile, previousCredential); restoreErr != nil {
				persistenceErr.Message += "; credential rollback also failed: " + restoreErr.Error()
			}
			return storeErr
		}
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]Profile{}
		}
		cfg.Profiles[profile] = a.profileWithCredential(profile, updateProfile(cfg.Profiles[profile]), true)
		if saveErr := a.saveConfigUnlocked(cfg); saveErr != nil {
			if restoreErr := persistence.restoreCredential(profile, previousCredential); restoreErr != nil {
				persistenceErr = configError("CONFIG_WRITE_FAILED", saveErr.Error()+"; credential rollback also failed: "+restoreErr.Error(), saveErr)
				return saveErr
			}
			persistenceErr = configError("CONFIG_WRITE_FAILED", saveErr.Error(), saveErr)
			return saveErr
		}
		return nil
	}()
	if lockErr != nil {
		if persistenceErr == nil {
			persistenceErr = configError("CONFIG_WRITE_FAILED", lockErr.Error(), lockErr)
		}
		return persistenceErr
	}
	a.credentialSavedNotice(profile)
	return nil
}

func (a *App) localLogout(cmd *Command, opts Options) int {
	for _, name := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_WORKLOAD_IDENTITY_ID"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return a.fail(configError("ENVIRONMENT_CREDENTIAL_ACTIVE", name+" is active and cannot be removed by flint auth logout. Unset it in the calling environment.", nil), opts)
		}
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	if e := a.confirmLocal("Sign out and remove the active credential (revokes OAuth sessions)?", opts); e != nil {
		return a.fail(e, opts)
	}
	var persistenceErr *CLIError
	lockErr := a.withConfigLock(func() error {
		raw, credentialErr := a.loadSavedCredentialUnlocked(resolved.ProfileName)
		if credentialErr != nil {
			persistenceErr = credentialStoreError("CREDENTIAL_LOOKUP_FAILED", "Could not read the credential before logout.", credentialErr)
			return persistenceErr
		}
		if isOAuthCredential(raw) {
			credential, e := decodeOAuthCredential(raw)
			if e == nil {
				e = a.oauthBaseURL(credential)
			}
			if e != nil {
				persistenceErr = e
				return persistenceErr
			}
			ctx, cancel := context.WithTimeout(withOAuthContext(a.commandContext(), resolved.ContextID), opts.Timeout)
			defer cancel()
			if e := a.revokeOAuth(ctx, credential); e != nil {
				persistenceErr = configError("OAUTH_REVOCATION_FAILED", "Could not revoke the OAuth session. Your local credential was retained so you can retry flint auth logout.", nil)
				return persistenceErr
			}
		}
		cfg, loadErr := a.loadConfig()
		if loadErr != nil {
			persistenceErr = configError("CONFIG_INVALID", loadErr.Error(), loadErr)
			return loadErr
		}
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]Profile{}
		}
		previousProfile := cfg.Profiles[resolved.ProfileName]
		p := previousProfile
		p.ContextID = ""
		p.Environment = ""
		p.APIKeyID = ""
		p.MerchantID = ""
		p.SandboxID = ""
		cfg.Profiles[resolved.ProfileName] = a.profileWithCredential(resolved.ProfileName, p, false)
		if saveErr := a.saveConfigUnlocked(cfg); saveErr != nil {
			persistenceErr = configError("CONFIG_WRITE_FAILED", saveErr.Error(), saveErr)
			return saveErr
		}
		if deleteErr := a.deleteSavedCredential(resolved.ProfileName); deleteErr != nil {
			cfg.Profiles[resolved.ProfileName] = previousProfile
			if restoreErr := a.saveConfigUnlocked(cfg); restoreErr != nil {
				persistenceErr = credentialStoreError("KEYCHAIN_DELETE_FAILED", "Could not remove the credential; profile metadata rollback also failed: "+restoreErr.Error(), deleteErr)
				return deleteErr
			}
			persistenceErr = credentialStoreError("KEYCHAIN_DELETE_FAILED", "Could not remove the credential.", deleteErr)
			return deleteErr
		}
		return nil
	})
	if lockErr != nil {
		if persistenceErr == nil {
			persistenceErr = configError("CONFIG_WRITE_FAILED", lockErr.Error(), lockErr)
		}
		return a.fail(persistenceErr, opts)
	}
	return a.outputLocal(map[string]any{"data": map[string]any{"profile": resolved.ProfileName, "authenticated": false}}, cmd, opts)
}

func (a *App) restoreCredential(profile, previous string) error {
	if previous == "" {
		return a.deleteSavedCredential(profile)
	}
	return a.storeSavedCredential(profile, previous)
}

func (a *App) confirmLocal(message string, opts Options) *CLIError {
	if opts.Confirm {
		return nil
	}
	if opts.NoInput || !a.IsTTY() {
		return &CLIError{ExitCode: ExitConfirmation, Type: "confirmation_required", Code: "CONFIRMATION_REQUIRED", Message: message + " Re-run with --confirm."}
	}
	fmt.Fprint(a.Stderr, message+" [y/N] ")
	line, err := bufio.NewReader(&contextInputReader{ctx: a.commandContext(), reader: a.Stdin}).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return configError("CONFIRMATION_READ_FAILED", "Could not read confirmation.", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "y" && answer != "yes" {
		return &CLIError{ExitCode: ExitConfirmation, Type: "confirmation_required", Code: "CONFIRMATION_DECLINED", Message: "Operation canceled."}
	}
	return nil
}

func (a *App) localSchema(cmd *Command, opts Options) int {
	a.Registry.ensureAPICommands()
	var value any
	switch cmd.CanonicalName {
	case "schema.commands":
		commands := commandList(a.Registry)
		if prefix := commandNameFromWords(opts.Positionals); prefix != "" {
			filtered := []map[string]any{}
			for _, command := range commands {
				if name := command["name"].(string); name == prefix || strings.HasPrefix(name, prefix+".") {
					filtered = append(filtered, command)
				}
			}
			if len(filtered) == 0 {
				suggestions := commandNameSuggestions(a.Registry, prefix, true)
				message := "No commands under " + prefix + "."
				if len(suggestions) > 0 {
					message += " Did you mean " + suggestions[0] + "?"
				}
				e := usageError("UNKNOWN_COMMAND_PREFIX", message, "prefix")
				e.Details = suggestionDetails(prefix, suggestions)
				return a.fail(e, opts)
			}
			commands = filtered
		}
		value = map[string]any{"data": commands}
	case "schema.errors":
		value = errorCatalog()
	case "schema.events":
		v, err := eventCatalog()
		if err != nil {
			return a.fail(cliError(ExitUsage, "internal_error", "SCHEMA_SNAPSHOT_INVALID", err.Error()), opts)
		}
		value = v
	default:
		requested := commandNameFromWords(opts.Positionals)
		target, ok := a.Registry.ByName(requested)
		if !ok {
			suggestions := commandNameSuggestions(a.Registry, requested, false)
			message := "Unknown command: " + requested + "."
			if requested != "" && len(a.Registry.commandsUnder(pathWithFlint(strings.Split(requested, ".")...))) > 0 {
				message = requested + " is a command group, not a command. Run flint schema commands " + requested + " to list its commands."
			} else if len(suggestions) > 0 {
				message += " Did you mean " + suggestions[0] + "?"
			}
			e := usageError("UNKNOWN_COMMAND_SCHEMA", message, "command")
			e.Details = suggestionDetails(requested, suggestions)
			return a.fail(e, opts)
		}
		var e *CLIError
		switch cmd.CanonicalName {
		case "schema.command":
			value = map[string]any{"data": commandDocument(target, requested)}
		case "schema.input":
			value, e = requestBodySchema(target)
		default:
			value, e = schemaForCommand(target, false)
		}
		if e != nil {
			return a.fail(e, opts)
		}
	}
	if opts.Output == "human" && (cmd.CanonicalName == "schema.input" || cmd.CanonicalName == "schema.output" || cmd.CanonicalName == "schema.command") {
		if opts.JQ != "" || opts.Field != "" || len(opts.Select) > 0 {
			return a.outputLocal(value, cmd, opts)
		}
		enc := json.NewEncoder(a.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(value); err != nil {
			return a.fail(networkError("OUTPUT_WRITE_FAILED", "Could not write schema output.", err), opts)
		}
		return ExitOK
	}
	opts.Output = "json"
	return a.outputLocal(value, cmd, opts)
}

func (a *App) localHelp(cmd *Command, opts Options) int {
	a.Registry.ensureAPICommands()
	topic := ""
	if len(opts.Positionals) > 0 {
		topic = opts.Positionals[0]
	}
	if topic == "" {
		if opts.Output == "json" {
			return a.outputLocal(map[string]any{"data": rootHelpIndex(a.Registry)}, cmd, opts)
		}
		a.printRootHelp()
		return ExitOK
	}
	if target, ok := a.Registry.ByName(topic); ok {
		if opts.Output == "json" {
			return a.outputLocal(map[string]any{"data": commandDocument(target, topic)}, cmd, opts)
		}
		a.printCommandHelp(target)
		return ExitOK
	}
	value, ok := helpTopic(topic)
	if topic == "events" && len(opts.Positionals) > 1 {
		var e *CLIError
		value, e = eventHelpDetail(opts.Positionals[1])
		if e != nil {
			return a.fail(e, opts)
		}
	} else if len(opts.Positionals) > 1 {
		return a.fail(usageError("UNEXPECTED_ARGUMENT", "Only help events accepts an event type.", "event_type"), opts)
	}
	if !ok {
		// The offline topics are a short fixed list. Anything else is a question,
		// and questions belong in Flint Help rather than in a dead end here.
		unknown := usageError("UNKNOWN_HELP_TOPIC", "Unknown help topic: "+topic+". Topics: "+strings.Join(helpTopicNames, ", ")+".", "topic")
		unknown.Details = map[string]any{"topics": helpTopicNames, "remediation": map[string]any{"next_actions": []any{
			map[string]any{"command": "flint help search " + topic},
		}}}
		return a.fail(unknown, opts)
	}
	if opts.Output == "json" {
		return a.outputLocal(map[string]any{"data": value}, cmd, opts)
	}
	if topic == "events" && len(opts.Positionals) == 1 {
		for _, event := range value["events"].([]map[string]any) {
			fmt.Fprintf(a.Stdout, "%-56s %s\n", event["type"], event["description"])
		}
		fmt.Fprintln(a.Stdout, "Run flint help events <event_type> for details.")
		return ExitOK
	}
	renderHelpTopic(a.Stdout, topic, value)
	return ExitOK
}

func (a *App) localInit(cmd *Command, opts Options) int {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	key, _, err := a.resolveCheckCredential(resolved.ProfileName)
	if err != nil {
		return a.fail(credentialStoreError("CREDENTIAL_LOOKUP_FAILED", "Could not read the credential.", err), opts)
	}
	if key == "" {
		return a.outputLocal(map[string]any{"data": map[string]any{"authenticated": false, "recommended_auth": "flint login", "existing_account": "flint login", "manual_api_key": "flint auth import", "headless_api_key": "Set FLINT_API_KEY", "new_account": "flint signup", "dashboard_api_keys_url": dashboardAPIKeysURL}}, cmd, opts)
	}
	checks, exit := a.doctorChecks(opts)
	value := map[string]any{"data": map[string]any{
		"checks": checks,
		"next_steps": []map[string]string{
			{"workflow": "commerce_order", "command": "flint orders create --input order.json"},
			{"workflow": "hosted_checkout", "command": "flint checkout create --quick-pay-name T-shirt --amount 2500 --currency USD --open"},
			{"workflow": "standalone_payment", "command": "flint payment create --amount 2500 --currency USD --payment-option card"},
		},
	}}
	if outputErr := a.writeResult(value, cmd, opts); outputErr != nil {
		return a.fail(outputErr, opts)
	}
	return exit
}

func (a *App) localDoctor(cmd *Command, opts Options) int {
	checks, exit := a.doctorChecks(opts)
	if outputErr := a.writeResult(map[string]any{"data": checks}, cmd, opts); outputErr != nil {
		return a.fail(outputErr, opts)
	}
	return exit
}

func (a *App) doctorChecks(opts Options) (checks []map[string]any, exit int) {
	checks = []map[string]any{}
	discoveryBaseURL := ""
	defer func() {
		checks = a.finishDoctorChecks(checks, opts, discoveryBaseURL)
	}()
	if opts.Fix {
		changes, err := a.applyDoctorFixes()
		if err != nil {
			checks = append(checks, map[string]any{"name": "config_fix", "status": "fail", "fix": err.Error()})
			return checks, ExitAuth
		}
		for _, change := range changes {
			checks = append(checks, map[string]any{"name": "config_fix", "status": "pass", "change": change})
		}
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		checks = append(checks, map[string]any{"name": "config", "status": "fail", "fix": err.Error()})
		return checks, ExitAuth
	}
	checks = append(checks, map[string]any{"name": "config", "status": "pass", "profile": resolved.ProfileName})
	key, source, err := a.resolveCheckCredential(resolved.ProfileName)
	if err != nil {
		checks = append(checks, map[string]any{"name": "credential", "status": "fail", "fix": credentialStoreError("CREDENTIAL_LOOKUP_FAILED", "Could not read the credential.", err).Message})
		return checks, ExitAuth
	}
	if key == "" {
		checks = append(checks, map[string]any{"name": "credential", "status": "fail", "fix": missingCredentialMessage(resolved.ProfileName)})
		return checks, ExitAuth
	}
	checks = append(checks, map[string]any{"name": "credential", "status": "pass", "source": source, "note": "Credential found; API validation follows."})
	ctx := withOAuthContext(a.commandContext(), resolved.ContextID)
	_, baseURL, authResponse, e := a.fetchCredentialContext(ctx, opts.Timeout, resolved.ProfileName, key, opts.Debug)
	discoveryBaseURL = baseURL
	if e != nil {
		switch e.Code {
		case "CREDENTIAL_FORMAT_INVALID", "INVALID_CREDENTIAL", "INVALID_OAUTH_CREDENTIAL", "OAUTH_SERVER_MISMATCH", "CREDENTIAL_LOOKUP_FAILED", "KEYCHAIN_WRITE_FAILED", "OAUTH_REFRESH_FAILED", "OAUTH_SESSION_CHANGED":
			checks = append(checks, map[string]any{"name": "authentication", "status": "fail", "fix": e.Message})
			return checks, e.ExitCode
		}
		if e.ExitCode == ExitAuth {
			fix := "Run flint login --profile " + resolved.ProfileName + " to sign in again (recommended), or flint auth import --profile " + resolved.ProfileName + " to replace the saved API key."
			if source == "environment_access_token" {
				fix = "Replace or unset FLINT_ACCESS_TOKEN. To save an OAuth session, unset the override and run flint login --profile " + resolved.ProfileName + "."
			} else if source == "workload_identity" {
				fix = "Check FLINT_WORKLOAD_IDENTITY_ID, the OIDC source, and the workload identity trust conditions."
			} else if source != "keychain" && source != "file" {
				fix = "Replace or unset FLINT_API_KEY. To save a valid API key, run flint auth import --profile " + resolved.ProfileName + "."
			}
			message := e.Message
			if !isOAuthCredential(key) {
				message = "API rejected the credential: " + e.Message
			}
			checks = append(checks, map[string]any{"name": "authentication", "status": "fail", "message": message, "fix": fix})
			return checks, e.ExitCode
		}
		checks = append(checks, map[string]any{"name": "connectivity", "status": "fail", "fix": e.Message})
		return checks, e.ExitCode
	}
	authContext := authResponse.Data
	if intentErr := validateResolvedCredentialIntent(authContext, opts, resolved, ""); intentErr != nil {
		checks = append(checks, map[string]any{"name": "context_guard", "status": "fail", "fix": intentErr.Message})
		return checks, intentErr.ExitCode
	}
	checks = append(
		checks,
		map[string]any{"name": "connectivity", "status": "pass"},
		map[string]any{"name": "authentication", "status": "pass", "message": "API accepted the credential"},
		map[string]any{"name": "environment", "status": "pass", "value": authContext.Environment},
		map[string]any{"name": "merchant", "status": "pass", "value": authContext.MerchantID},
	)
	if len(authContext.Scopes) == 0 {
		checks = append(checks, map[string]any{
			"name":                 "scopes",
			"status":               "fail",
			"count":                0,
			"missing_cli_baseline": initialCLIScopes,
			"fix":                  "Sign in again or issue an API key with the scopes required by the commands you intend to run.",
		})
		return checks, ExitAuth
	}
	grantedScopes := make(map[string]struct{}, len(authContext.Scopes))
	for _, scope := range authContext.Scopes {
		grantedScopes[strings.TrimSpace(scope)] = struct{}{}
	}
	missingBaselineScopes := []string{}
	for _, scope := range initialCLIScopes {
		if _, ok := grantedScopes[scope]; !ok {
			missingBaselineScopes = append(missingBaselineScopes, scope)
		}
	}
	scopeCheck := map[string]any{"name": "scopes", "status": "pass", "count": len(authContext.Scopes)}
	if len(missingBaselineScopes) > 0 {
		scopeCheck["limited"] = true
		scopeCheck["missing_cli_baseline"] = missingBaselineScopes
		if authContext.AuthType == "oauth" {
			scopeCheck["note"] = "This browser session is missing CLI baseline scopes. Commands outside its granted scopes will fail authorization."
		} else {
			scopeCheck["note"] = "This is a limited key. Commands outside its granted scopes will fail authorization."
		}
	} else if authContext.AuthType == "oauth" {
		scopeCheck["status"] = "info"
		scopeCheck["browser_login_baseline"] = true
		scopeCheck["note"] = "Browser login grants the CLI baseline scopes. Use flint reauth --scope for commands that need additional access."
	}
	checks = append(checks, scopeCheck)
	return checks, ExitOK
}

func (a *App) applyDoctorFixes() ([]string, error) {
	var changes []string
	if err := a.updateConfig(func(cfg *Config) error {
		if cfg.DefaultProfile == "" || cfg.DefaultProfile == "default" {
			return nil
		}
		if _, exists := cfg.Profiles[cfg.DefaultProfile]; exists {
			return nil
		}
		if _, exists := cfg.Profiles["default"]; !exists && len(cfg.Profiles) > 0 {
			return fmt.Errorf("default profile %q is stale, but no default profile exists; choose one with flint config set profile NAME", cfg.DefaultProfile)
		}
		previous := cfg.DefaultProfile
		cfg.DefaultProfile = "default"
		changes = append(changes, "Changed stale default profile "+previous+" to default.")
		return nil
	}); err != nil {
		return nil, err
	}
	return changes, nil
}

func (a *App) localSignup(cmd *Command, opts Options) int { return a.runSignup(opts) }

var helpTopicNames = []string{"exit-codes", "errors", "events", "test-cards"}

func helpTopic(topic string) (map[string]any, bool) {
	switch topic {
	case "exit-codes":
		return errorCatalog(), true
	case "errors":
		return errorEnvelopeHelp(), true
	case "events":
		v, err := eventHelpIndex()
		return map[string]any{"events": v}, err == nil
	case "test-cards":
		return map[string]any{"sandbox_tokens": []map[string]string{{"scenario": "success", "token": "pm_card_visa"}, {"scenario": "3d_secure", "token": "pm_card_threeDSecure2Required"}, {"scenario": "authentication_required", "token": "pm_card_authenticationRequired"}, {"scenario": "insufficient_funds", "token": "pm_card_chargeDeclinedInsufficientFunds"}, {"scenario": "generic_decline", "token": "pm_card_chargeDeclined"}}, "provider_test_cards": []map[string]string{{"scenario": "success", "number": "4242 4242 4242 4242"}, {"scenario": "3d_secure", "number": "4000 0000 0000 3220"}, {"scenario": "authentication_required", "number": "4000 0025 0000 3155"}, {"scenario": "insufficient_funds", "number": "4000 0000 0000 9995"}, {"scenario": "generic_decline", "number": "4000 0000 0000 0002"}}}, true
	default:
		return nil, false
	}
}
func renderHelpTopic(w io.Writer, topic string, value map[string]any) {
	fmt.Fprintln(w, strings.ToUpper(strings.ReplaceAll(topic, "-", " ")))
	b, _ := json.MarshalIndent(value, "", "  ")
	fmt.Fprintln(w, string(b))
}
