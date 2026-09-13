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

	"golang.org/x/term"
)

func (a *App) runLocal(cmd *Command, opts Options) int {
	switch cmd.CanonicalName {
	case "version":
		return a.outputLocal(map[string]any{"data": a.Info}, cmd, opts)
	case "config.get":
		return a.localConfigGet(cmd, opts)
	case "config.set":
		return a.localConfigSet(cmd, opts)
	case "config.validate":
		return a.localConfigValidate(cmd, opts)
	case "feedback.configure":
		return a.localFeedbackConfigure(cmd, opts)
	case "history":
		return a.localHistory(cmd, opts)
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
		if exit := a.prepareMCPFeedbackConsent(opts); exit != ExitOK {
			return exit
		}
		return a.serveMCP(opts)
	case "signup":
		return a.localSignup(cmd, opts)
	default:
		return a.fail(cliError(ExitUsage, "usage_error", "NOT_IMPLEMENTED", "Local command is not implemented: "+cmd.Name), opts)
	}
}

const agentFeedbackConsentText = `Allow connected AI agents to send feedback to Flint using this profile? [y/N]

Agents can report a Flint bug or point of friction with text they provide. A report may include request IDs, resource IDs, and code locations.

Flint does not automatically add files, command output, API responses, environment variables, or shell history. The CLI blocks recognized secrets before sending, and Flint scans the report again before storage.

This allows submission only when this profile has the feedback write scope. It never lets agents read existing feedback.`

func (a *App) localFeedbackConfigure(cmd *Command, opts Options) int {
	state := strings.TrimSpace(opts.Positionals[0])
	if state != "enabled" && state != "disabled" {
		return a.fail(usageError("INVALID_FEEDBACK_SETTING", "Feedback submission state must be enabled or disabled.", "state"), opts)
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	if state == "enabled" {
		if opts.NoInput || !a.IsTTY() {
			return a.fail(configError("INTERACTIVE_CONSENT_REQUIRED", "Enabling agent feedback requires an interactive terminal. Run flint feedback configure enabled in a terminal.", nil), opts)
		}
		fmt.Fprintln(a.Stderr, agentFeedbackConsentText)
		line, readErr := bufio.NewReader(a.Stdin).ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return a.fail(configError("CONSENT_READ_FAILED", "Could not read feedback consent.", readErr), opts)
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			return a.fail(&CLIError{ExitCode: ExitConfirmation, Type: "confirmation_required", Code: "CONSENT_DECLINED", Message: "Agent feedback remains disabled."}, opts)
		}
	}
	if err := a.updateConfig(func(cfg *Config) error {
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]Profile{}
		}
		profile := cfg.Profiles[resolved.ProfileName]
		profile.AgentFeedbackSubmission = state
		cfg.Profiles[resolved.ProfileName] = profile
		return nil
	}); err != nil {
		return a.fail(configError("CONFIG_WRITE_FAILED", err.Error(), err), opts)
	}
	return a.outputLocal(map[string]any{"data": map[string]any{"profile": resolved.ProfileName, "agent_feedback_submission": state}}, cmd, opts)
}

func (a *App) prepareMCPFeedbackConsent(opts Options) int {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	if resolved.AgentFeedbackSubmission != "" || opts.NoInput || !a.IsTTY() {
		return ExitOK
	}
	fmt.Fprintln(a.Stderr, agentFeedbackConsentText)
	line, readErr := bufio.NewReader(a.Stdin).ReadString('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return a.fail(configError("CONSENT_READ_FAILED", "Could not read feedback consent.", readErr), opts)
	}
	state := "disabled"
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "y" || answer == "yes" {
		state = "enabled"
	}
	if err := a.updateConfig(func(cfg *Config) error {
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]Profile{}
		}
		profile := cfg.Profiles[resolved.ProfileName]
		profile.AgentFeedbackSubmission = state
		cfg.Profiles[resolved.ProfileName] = profile
		return nil
	}); err != nil {
		return a.fail(configError("CONFIG_WRITE_FAILED", err.Error(), err), opts)
	}
	return ExitOK
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
				cfg.Profiles[value] = Profile{}
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
			p := cfg.Profiles[resolved.ProfileName]
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

func (a *App) localConfigValidate(cmd *Command, opts Options) int {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	checks := []map[string]any{{"name": "global_config", "status": "pass", "path": resolved.GlobalConfigPath}, {"name": "project_config", "status": "pass", "path": resolved.ProjectConfigPath}, {"name": "profile", "status": "pass", "value": resolved.ProfileName}}
	key, source, credentialErr := a.resolveCredential(resolved.ProfileName)
	if credentialErr != nil {
		return a.fail(configError("CREDENTIAL_LOOKUP_FAILED", credentialErr.Error(), credentialErr), opts)
	}
	if key != "" {
		baseURL, baseURLErr := a.baseURLForCredential(key)
		if baseURLErr != nil {
			return a.fail(configError("INVALID_CREDENTIAL", baseURLErr.Error(), baseURLErr), opts)
		}
		ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
		defer cancel()
		authContext, authErr := a.fetchAuthContext(ctx, baseURL, key, opts.Debug)
		if authErr != nil {
			return a.fail(authErr, opts)
		}
		if intentErr := validateCredentialIntent(authContext, opts, resolved.MerchantGuard, resolved.SandboxGuard); intentErr != nil {
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
	if resolved.Environment == "" {
		if credential, _, credentialErr := a.resolveCredential(resolved.ProfileName); credentialErr == nil && credential != "" {
			resolved.Environment, _ = credentialEnvironment(credential)
		}
	}
	scope := historyScopeFromResolved(resolved)
	var entries []any
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
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	var key string
	if opts.Stdin {
		raw, err := io.ReadAll(io.LimitReader(a.Stdin, 16<<10))
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
			raw, err := term.ReadPassword(int(f.Fd()))
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
		return a.fail(configError("INVALID_CREDENTIAL", err.Error(), err), opts)
	}
	baseURL, err := a.baseURLForCredential(key)
	if err != nil {
		return a.fail(configError("INVALID_CREDENTIAL", err.Error(), err), opts)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()
	authContext, e := a.fetchAuthContext(ctx, baseURL, key, opts.Debug)
	if e != nil {
		return a.fail(e, opts)
	}
	if intentErr := validateCredentialIntent(authContext, opts, resolved.MerchantGuard, resolved.SandboxGuard); intentErr != nil {
		return a.fail(intentErr, opts)
	}
	var persistenceErr *CLIError
	lockErr := a.withConfigLock(func() error {
		cfg, loadErr := a.loadConfig()
		if loadErr != nil {
			persistenceErr = configError("CONFIG_INVALID", loadErr.Error(), loadErr)
			return loadErr
		}
		previousCredential, previousErr := a.LoadCredential(resolved.ProfileName)
		if previousErr != nil {
			persistenceErr = configError("CREDENTIAL_LOOKUP_FAILED", "The existing profile credential could not be read before import.", previousErr)
			return previousErr
		}
		if storeErr := a.StoreCredential(resolved.ProfileName, key); storeErr != nil {
			persistenceErr = configError("KEYCHAIN_WRITE_FAILED", "The validated credential could not be stored in the OS keychain.", storeErr)
			return storeErr
		}
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]Profile{}
		}
		p := cfg.Profiles[resolved.ProfileName]
		p.Environment = normalizeEnvironment(authContext.Environment)
		p.APIKeyID = authContext.APIKeyID
		p.MerchantID = authContext.MerchantID
		p.SandboxID = authContext.SandboxID
		cfg.Profiles[resolved.ProfileName] = p
		if saveErr := a.saveConfigUnlocked(cfg); saveErr != nil {
			if restoreErr := a.restoreCredential(resolved.ProfileName, previousCredential); restoreErr != nil {
				persistenceErr = configError("CONFIG_WRITE_FAILED", saveErr.Error()+"; credential rollback also failed: "+restoreErr.Error(), saveErr)
				return saveErr
			}
			persistenceErr = configError("CONFIG_WRITE_FAILED", saveErr.Error(), saveErr)
			return saveErr
		}
		return nil
	})
	if lockErr != nil {
		if persistenceErr == nil {
			persistenceErr = configError("CONFIG_WRITE_FAILED", lockErr.Error(), lockErr)
		}
		return a.fail(persistenceErr, opts)
	}
	return a.outputLocal(map[string]any{"data": authContext}, cmd, opts)
}

func (a *App) localLogout(cmd *Command, opts Options) int {
	if credentialFromEnvironment() != "" {
		return a.fail(configError("ENVIRONMENT_CREDENTIAL_ACTIVE", "FLINT_API_KEY is active and cannot be removed by flint auth logout. Unset it in the calling environment.", nil), opts)
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	if e := a.confirmLocal("Remove the active Flint credential from the OS keychain?", opts); e != nil {
		return a.fail(e, opts)
	}
	var persistenceErr *CLIError
	lockErr := a.withConfigLock(func() error {
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
		p.Environment = ""
		p.APIKeyID = ""
		p.MerchantID = ""
		p.SandboxID = ""
		cfg.Profiles[resolved.ProfileName] = p
		if saveErr := a.saveConfigUnlocked(cfg); saveErr != nil {
			persistenceErr = configError("CONFIG_WRITE_FAILED", saveErr.Error(), saveErr)
			return saveErr
		}
		if deleteErr := a.DeleteCredential(resolved.ProfileName); deleteErr != nil {
			cfg.Profiles[resolved.ProfileName] = previousProfile
			if restoreErr := a.saveConfigUnlocked(cfg); restoreErr != nil {
				persistenceErr = configError("KEYCHAIN_DELETE_FAILED", deleteErr.Error()+"; profile metadata rollback also failed: "+restoreErr.Error(), deleteErr)
				return deleteErr
			}
			persistenceErr = configError("KEYCHAIN_DELETE_FAILED", deleteErr.Error(), deleteErr)
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
		return a.DeleteCredential(profile)
	}
	return a.StoreCredential(profile, previous)
}

func (a *App) confirmLocal(message string, opts Options) *CLIError {
	if opts.Confirm {
		return nil
	}
	if opts.NoInput || !a.IsTTY() {
		return &CLIError{ExitCode: ExitConfirmation, Type: "confirmation_required", Code: "CONFIRMATION_REQUIRED", Message: message + " Re-run with --confirm."}
	}
	fmt.Fprint(a.Stderr, message+" [y/N] ")
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
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
	var value any
	switch cmd.CanonicalName {
	case "schema.commands":
		value = map[string]any{"data": commandList(a.Registry)}
	case "schema.errors":
		value = errorCatalog()
	case "schema.events":
		v, err := eventCatalog()
		if err != nil {
			return a.fail(cliError(ExitUsage, "internal_error", "SCHEMA_SNAPSHOT_INVALID", err.Error()), opts)
		}
		value = v
	default:
		requested := opts.Positionals[0]
		target, ok := a.Registry.ByName(requested)
		if !ok {
			return a.fail(usageError("UNKNOWN_COMMAND_SCHEMA", "Unknown command: "+requested, "command"), opts)
		}
		if cmd.CanonicalName == "schema.command" {
			value = commandDocument(target, requested)
		} else {
			schema, e := schemaForCommand(target, cmd.CanonicalName == "schema.input")
			if e != nil {
				return a.fail(e, opts)
			}
			value = schema
		}
	}
	opts.Output = "json"
	return a.outputLocal(value, cmd, opts)
}

func (a *App) localHelp(cmd *Command, opts Options) int {
	topic := ""
	if len(opts.Positionals) > 0 {
		topic = opts.Positionals[0]
	}
	if topic == "" {
		if opts.Output == "json" {
			return a.outputLocal(map[string]any{"data": commandList(a.Registry)}, cmd, opts)
		}
		a.printRootHelp()
		return ExitOK
	}
	if target, ok := a.Registry.ByName(topic); ok {
		a.printCommandHelp(target)
		return ExitOK
	}
	value, ok := helpTopic(topic)
	if !ok {
		// The offline topics are a short fixed list. Anything else is a question,
		// and questions belong in Flint Help rather than in a dead end here.
		unknown := usageError("UNKNOWN_HELP_TOPIC", "Unknown help topic: "+topic, "topic")
		unknown.Details = map[string]any{"remediation": map[string]any{"next_actions": []any{
			map[string]any{"command": "flint help search " + topic},
		}}}
		return a.fail(unknown, opts)
	}
	if opts.Output == "json" {
		return a.outputLocal(value, cmd, opts)
	}
	renderHelpTopic(a.Stdout, topic, value)
	return ExitOK
}

func (a *App) localInit(cmd *Command, opts Options) int {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	key, _, err := a.resolveCredential(resolved.ProfileName)
	if err != nil {
		return a.fail(configError("CREDENTIAL_LOOKUP_FAILED", err.Error(), err), opts)
	}
	if key == "" {
		return a.outputLocal(map[string]any{"data": map[string]any{"authenticated": false, "existing_account": "flint auth import", "new_account": "flint signup", "dashboard_api_keys_url": dashboardAPIKeysURL}}, cmd, opts)
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

func (a *App) doctorChecks(opts Options) ([]map[string]any, int) {
	checks := []map[string]any{}
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
	key, source, err := a.resolveCredential(resolved.ProfileName)
	if err != nil {
		checks = append(checks, map[string]any{"name": "credential", "status": "fail", "fix": err.Error()})
		return checks, ExitAuth
	}
	if key == "" {
		checks = append(checks, map[string]any{"name": "credential", "status": "fail", "fix": "Run flint auth import or flint signup."})
		return checks, ExitAuth
	}
	checks = append(checks, map[string]any{"name": "credential", "status": "pass", "source": source})
	baseURL, err := a.baseURLForCredential(key)
	if err != nil {
		checks = append(checks, map[string]any{"name": "credential", "status": "fail", "fix": err.Error()})
		return checks, ExitAuth
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()
	authResponse, e := a.fetchAuthContextResponse(ctx, baseURL, key, opts.Debug)
	if e != nil {
		checks = append(checks, map[string]any{"name": "connectivity", "status": "fail", "fix": e.Message})
		return checks, e.ExitCode
	}
	authContext := authResponse.Data
	if intentErr := validateCredentialIntent(authContext, opts, resolved.MerchantGuard, resolved.SandboxGuard); intentErr != nil {
		checks = append(checks, map[string]any{"name": "context_guard", "status": "fail", "fix": intentErr.Message})
		return checks, intentErr.ExitCode
	}
	checks = append(
		checks,
		map[string]any{"name": "connectivity", "status": "pass"},
		map[string]any{"name": "environment", "status": "pass", "value": authContext.Environment},
		map[string]any{"name": "merchant", "status": "pass", "value": authContext.MerchantID},
	)
	if len(authContext.Scopes) == 0 {
		checks = append(checks, map[string]any{
			"name":   "scopes",
			"status": "fail",
			"count":  0,
			"fix":    "Issue an API key with the scopes required by the commands you intend to run.",
		})
		return checks, ExitAuth
	}
	grantedScopes := make(map[string]struct{}, len(authContext.Scopes))
	for _, scope := range authContext.Scopes {
		grantedScopes[strings.TrimSpace(scope)] = struct{}{}
	}
	missingBaselineScopes := 0
	for _, scope := range initialCLIScopes {
		if _, ok := grantedScopes[scope]; !ok {
			missingBaselineScopes++
		}
	}
	scopeCheck := map[string]any{"name": "scopes", "status": "pass", "count": len(authContext.Scopes)}
	if missingBaselineScopes > 0 {
		scopeCheck["limited"] = true
		scopeCheck["missing_cli_baseline_count"] = missingBaselineScopes
		scopeCheck["note"] = "This is a limited key. Commands outside its granted scopes will fail authorization."
	}
	checks = append(checks, scopeCheck)
	observedVersion, discoveryError := a.fetchCurrentAPIVersion(ctx, baseURL, opts.Debug)
	versionCheck := map[string]any{"name": "api_version", "expected": a.Info.APIVersion, "observed": observedVersion, "status": "pass"}
	if discoveryError != nil {
		versionCheck["status"] = "fail"
		versionCheck["fix"] = discoveryError.Message
		checks = append(checks, versionCheck)
		return checks, discoveryError.ExitCode
	}
	if observedVersion != a.Info.APIVersion {
		versionCheck["status"] = "info"
		versionCheck["note"] = "The server's current API version is " + observedVersion + ". This CLI requests " + a.Info.APIVersion + "."
		versionCheck["changelog_url"] = "https://developers.withflintpay.com/changelog"
	}
	checks = append(checks, versionCheck, map[string]any{"name": "cli_version", "status": "pass", "installed": a.Info.Version, "note": "No public CLI release feed is configured for staleness checks."})
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

func helpTopic(topic string) (map[string]any, bool) {
	switch topic {
	case "exit-codes":
		return errorCatalog(), true
	case "errors":
		return errorCatalog(), true
	case "events":
		v, err := eventCatalog()
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
