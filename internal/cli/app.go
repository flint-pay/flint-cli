package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"
)

const dashboardAPIKeysURL = "https://app.withflintpay.com/developers/api-keys"

func (a *App) commandContext() context.Context {
	if a.Context != nil {
		return a.Context
	}
	return context.Background()
}

func New(info BuildInfo) *App {
	return &App{
		Info:                      info,
		Stdout:                    os.Stdout,
		Stderr:                    os.Stderr,
		Stdin:                     os.Stdin,
		IsTTY:                     func() bool { return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd())) },
		Now:                       time.Now,
		Registry:                  newLocalRegistry(),
		LoadCredential:            loadKeychainCredential,
		StoreCredential:           storeKeychainCredential,
		DeleteCredential:          deleteKeychainCredential,
		upgradeReleaseLatestURL:   defaultUpgradeReleaseLatestURL,
		upgradeReleaseAPIURL:      defaultUpgradeReleaseAPIURL,
		upgradeReleaseDownloadURL: defaultUpgradeReleaseDownloadURL,
		executablePath:            os.Executable,
		runCommand:                runUpgradeCommand,
		runtimeGOOS:               runtime.GOOS,
		runtimeGOARCH:             runtime.GOARCH,
	}
}

func (a *App) Run(argv []string) int {
	if len(argv) > 0 && argv[0] == "__complete" {
		return a.runCompletionWords(argv[1:])
	}
	cmd, opts, help, parseErr := parseInvocation(a.Registry, argv)
	applyEnvironmentOptions(&opts)
	if parseErr != nil {
		return a.fail(parseErr, opts)
	}
	if envErr := validateOptions(cmd, &opts, help); envErr != nil {
		return a.fail(envErr, opts)
	}
	previousStore := a.credentialStore
	a.credentialStore = opts.CredentialStore
	defer func() { a.credentialStore = previousStore }()
	if value := strings.TrimSpace(os.Getenv("FLINT_BASE_URL")); value != "" {
		if _, err := validateBaseURL(value); err != nil {
			return a.fail(configError("INVALID_BASE_URL", err.Error(), err), opts)
		}
		if strings.HasSuffix(strings.TrimRight(value, "/"), "/v1") && !opts.Quiet {
			fmt.Fprintln(a.Stderr, "warning: stripped trailing /v1 from FLINT_BASE_URL; Flint adds the API version path.")
		}
	}
	if help {
		if cmd.CanonicalName == "help" {
			return a.localHelp(cmd, opts)
		}
		return a.showCommandHelp(cmd, opts)
	}

	if cmd.Local {
		return a.runLocal(cmd, opts)
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	ctx := a.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = withOAuthContext(ctx, resolved.ContextID)
	effective := resolveAPICommand(cmd, opts)
	paths, pathErr := outputPaths(opts)
	if pathErr != nil {
		return a.fail(pathErr, opts)
	}
	if !opts.DryRunClient() && !opts.Preview {
		if pathErr = validateOutputPaths(outputCommand(effective, opts), paths); pathErr != nil {
			return a.fail(pathErr, opts)
		}
	}
	idempotencyKey, errKey := requestIdempotencyKey(effective, opts)
	if errKey != nil {
		return a.fail(errKey, opts)
	}
	opts.IdempotencyKey = idempotencyKey
	key, baseURL, authEnvelope, authErr := a.authenticateCommand(ctx, effective, opts, resolved)
	if authErr != nil {
		return a.fail(authErr, opts)
	}
	if !a.mcpSkipScopeCheck && authEnvelope.fromCache && !opts.DryRunClient() && commandPermissionError(effective, authEnvelope.Data, resolved.ProfileName) != nil {
		// Revalidate a cached denial before any guards, confirmation, or target
		// request. The refreshed context must pass the normal checks below.
		authCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		authEnvelope, authErr = a.fetchFreshAuthContextResponse(authCtx, baseURL, key, opts.Debug)
		cancel()
		if authErr != nil {
			return a.fail(authErr, opts)
		}
	}
	authContext := authEnvelope.Data
	a.observeCachedAuthContext(authContext)
	if a.mcpAuth != nil && !opts.DryRunClient() && authContext.AuthType != "" && authContext.AuthType != "none" && authContext.AuthType != "session" {
		a.mcpAuth.store(authContext)
	}
	if authContext.OAuthGrantID != "" && strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN")) == "" && credentialFromEnvironment() == "" && workloadIdentityID() == "" {
		ctx = context.WithValue(ctx, oauthSessionKey{}, &oauthSession{Profile: resolved.ProfileName, GrantID: authContext.OAuthGrantID, ContextID: authContext.ContextID, SessionID: authContext.OAuthSessionID, MerchantID: authContext.MerchantID, SandboxID: authContext.SandboxID, Environment: authContext.Environment})
	}
	if authContext.SelectedContext && authContext.Environment == "live" && !opts.Quiet {
		fmt.Fprintln(a.Stderr, "LIVE context:", terminalSafe(authContext.ContextID), "merchant:", terminalSafe(authContext.MerchantID))
	}
	resolved.CredentialScope = authContext.CredentialScope
	resolved.Environment = normalizeEnvironment(authContext.Environment)
	resolved.APIKeyID = authContext.APIKeyID
	resolved.MerchantID = authContext.MerchantID
	resolved.SandboxID = authContext.SandboxID
	if a.mcpGate != nil {
		if e := a.mcpGate.afterAuth(authContext); e != nil {
			return a.fail(e, opts)
		}
		// MCP checks the public per-call acknowledgement itself. Keep the
		// terminal credential guard aligned with the authenticated environment.
		opts.Live = normalizeEnvironment(authContext.Environment) == "live"
	}
	affinity := effective.EnvironmentAffinity
	if effective.OperationID == "authorizePartnerInstall" {
		// The browser entry point is public, but a live authorization still
		// requires the same explicit acknowledgement as before.
		affinity = EnvironmentAffinityAccount
	}
	// MCP dry runs never send requests and need no live acknowledgement.
	if a.mcpSkipScopeCheck && opts.DryRunClient() {
		opts.Live = normalizeEnvironment(authContext.Environment) == "live"
	}
	if !(opts.DryRun == "client" && authContext.AuthType == "none") {
		if e := validateResolvedCredentialIntent(authContext, opts, resolved, affinity); (effective.AuthRequired || effective.OperationID == "authorizePartnerInstall") && e != nil {
			return a.fail(e, opts)
		}
	}
	if cmd.CanonicalName == "auth.status" {
		if e := a.writeResult(map[string]any{"data": authStatusData{AuthContext: authEnvelope.Data, CLI: a.identityInfo(resolved, baseURL, a.identityCredentialSource(resolved, authEnvelope.Data))}, "request_id": authEnvelope.RequestID, "meta": authEnvelope.Meta}, cmd, opts); e != nil {
			return a.fail(e, opts)
		}
		return ExitOK
	}
	if !a.mcpSkipScopeCheck && !opts.DryRunClient() {
		if e := commandPermissionError(effective, authContext, resolved.ProfileName); e != nil {
			return a.fail(e, opts)
		}
	}
	if a.mcpGate == nil {
		if e := a.confirmCommand(effective, authContext, opts); e != nil {
			return a.fail(e, opts)
		}
	}
	value, runErr := a.executeAPI(ctx, effective, opts, resolved, authContext, key, baseURL)
	if runErr != nil {
		if runErr.ExitCode == ExitWait && value != nil && opts.Output == "human" {
			if e := a.writeResult(value, cmd, opts); e != nil {
				return a.fail(e, opts)
			}
			fmt.Fprintln(a.Stderr, runErr.Message)
			return runErr.ExitCode
		}
		return a.fail(runErr, opts)
	}
	if _, ok := value.(outputHandled); ok {
		return ExitOK
	}
	if !opts.DryRunClient() && effective.Mutation {
		if err := a.recordHistory(value, effective.CanonicalName, historyScopeFromResolved(resolved)); err != nil && !opts.Quiet {
			fmt.Fprintln(a.Stderr, "warning: could not record resource history: "+err.Error())
		}
	}
	if e := a.writeResult(value, cmd, opts); e != nil {
		// Preserve recovery information if presentation fails after the write.
		e.requestSent = effective.Mutation && !opts.DryRunClient()
		return a.fail(withIdempotencyRetryContext(e, opts.IdempotencyKey), opts)
	}
	return ExitOK
}

func (o Options) DryRunClient() bool { return o.DryRun == "client" || o.Preview }

func applyEnvironmentOptions(o *Options) {
	if len(o.Raw["credential-store"]) == 0 {
		o.CredentialStore = strings.TrimSpace(os.Getenv("FLINT_CREDENTIAL_STORE"))
	}
	if len(o.Raw["output"]) == 0 {
		if v := strings.TrimSpace(os.Getenv("FLINT_OUTPUT")); v != "" {
			o.Output = strings.ToLower(v)
			o.outputFromEnvironment = true
		}
	}
	if len(o.Raw["color"]) == 0 {
		if v := strings.TrimSpace(os.Getenv("FLINT_COLOR")); v != "" {
			o.Color = v
		}
	}
	if environmentBool(os.Getenv("FLINT_NO_INPUT")) {
		o.NoInput = true
	}
	if o.Output == "json" || o.Output == "ndjson" {
		o.NoInput = true
	}
}

// environmentUnverified marks an access token whose environment the API
// would not report. Sensitive operations then require confirmation.
const environmentUnverified = "unverified"

func environmentBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func normalizeEnvironment(value string) string {
	switch strings.ToLower(value) {
	case "test", "sandbox":
		return "sandbox"
	case "live", "production":
		return "live"
	default:
		return strings.ToLower(value)
	}
}

func validateCredentialIntent(authContext AuthContext, opts Options, merchantGuard, sandboxGuard string) *CLIError {
	return validateCredentialIntentForCommand(authContext, opts, merchantGuard, sandboxGuard, "")
}

func validateCredentialIntentForCommand(authContext AuthContext, opts Options, merchantGuard, sandboxGuard string, affinity EnvironmentAffinity) *CLIError {
	if authContext.SelectedContext && len(opts.Raw["live"]) == 0 && authContext.Environment == "live" {
		opts.Live = true
	}
	env := normalizeEnvironment(authContext.Environment)
	if env == environmentUnverified && authContext.AuthType == "session" {
		// Guards were rejected before an unverifiable token got this far;
		// confirmCommand treats sensitive operations as live.
		return nil
	}
	if env != "sandbox" && env != "live" {
		return configError("UNKNOWN_CREDENTIAL_ENVIRONMENT", "The credential returned an unknown environment: "+authContext.Environment, nil)
	}
	if env == "live" && !opts.Live && affinity != EnvironmentAffinityNeutral {
		return configError("LIVE_ACKNOWLEDGEMENT_REQUIRED", "This credential is live. Re-run with --live to acknowledge production access.", nil)
	}
	if env == "sandbox" && opts.Live {
		return configError("CREDENTIAL_MODE_MISMATCH", "--live cannot be used with a sandbox credential.", nil)
	}
	if merchantGuard != "" && authContext.MerchantID != "" && merchantGuard != authContext.MerchantID {
		return configError("MERCHANT_GUARD_MISMATCH", fmt.Sprintf("Merchant guard %s does not match credential merchant %s.", merchantGuard, authContext.MerchantID), nil)
	}
	if sandboxGuard != "" && sandboxGuard != authContext.SandboxID {
		actual := authContext.SandboxID
		if actual == "" {
			actual = "none"
		}
		return configError("SANDBOX_GUARD_MISMATCH", fmt.Sprintf("Sandbox guard %s does not match credential sandbox %s.", sandboxGuard, actual), nil)
	}
	return nil
}

func (a *App) confirmCommand(cmd *Command, authContext AuthContext, opts Options) *CLIError {
	if opts.DryRun == "client" {
		return nil
	}
	environment := normalizeEnvironment(authContext.Environment)
	index, err := a.operationIndex()
	if err != nil {
		return cliError(ExitSoftware, errorTypeInternal, "SCHEMA_SNAPSHOT_INVALID", "The operation's confirmation rule could not be read: "+err.Error())
	}
	entry, found := index.Lookup(cmd.CanonicalName)
	if cmd.OperationID != "" && !cmd.Local {
		entry, found = index.OperationSafety(cmd.OperationID)
	}
	needs := found && terminalConfirmationRequired(entry, environment)
	if unknownRawOperation(cmd) {
		// No contract classification exists for forward-compatible raw routes.
		needs = cmd.Method == "DELETE" || cmd.Mutation && (environment == "live" || environment == environmentUnverified)
	}
	// Keep the earlier irreversible bridge's error code stable. Legacy hints
	// affect only this diagnostic, never whether confirmation is required.
	bridgeCode := entry.Irreversible && !cmd.Destructive && !cmd.Sensitive
	if !needs || opts.Confirm || opts.Preview {
		return nil
	}
	message := "This operation requires confirmation."
	code := "CONFIRMATION_REQUIRED"
	switch environment {
	case "live":
		message = "This operation affects live production data and requires confirmation."
		if !bridgeCode {
			code = "PRODUCTION_CONFIRMATION_REQUIRED"
		}
	case environmentUnverified:
		message = "Flint cannot verify whether FLINT_ACCESS_TOKEN is a live credential, so this operation requires confirmation."
		if !bridgeCode {
			code = "UNVERIFIED_ENVIRONMENT_CONFIRMATION_REQUIRED"
		}
	}
	if opts.NoInput || !a.IsTTY() {
		return &CLIError{ExitCode: ExitConfirmation, Type: errorTypeConfirmationRequired, Code: code, Message: message + " Re-run with --confirm."}
	}
	fmt.Fprintf(a.Stderr, "%s Continue? [y/N] ", message)
	line, err := bufio.NewReader(&contextInputReader{ctx: a.commandContext(), reader: a.Stdin}).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return configError("CONFIRMATION_READ_FAILED", "Could not read confirmation.", err)
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line != "y" && line != "yes" {
		return &CLIError{ExitCode: ExitConfirmation, Type: errorTypeConfirmationRequired, Code: "CONFIRMATION_DECLINED", Message: "Operation canceled."}
	}
	return nil
}

func configError(code, message string, cause error) *CLIError {
	if errors.Is(cause, errLocalFileLockTimeout) {
		return networkError("CONFIG_LOCK_TIMEOUT", "Another Flint process is updating this profile. Retry the command.", cause)
	}
	if errors.Is(cause, context.Canceled) {
		return networkError("CLIENT_CANCELED", "The command was canceled.", cause)
	}
	return &CLIError{ExitCode: ExitAuth, Type: errorTypeConfiguration, Code: code, Message: message, Cause: cause}
}
func (a *App) fail(err *CLIError, opts Options) int {
	if a.mcpErrorObserver != nil {
		a.mcpErrorObserver(err)
	}
	a.writeError(err, opts)
	return err.ExitCode
}

func (a *App) printCommandHelp(cmd *Command) {
	if cmd.Group {
		fmt.Fprintln(a.Stdout, cmd.Description)
		fmt.Fprintln(a.Stdout, "\nUsage:")
		fmt.Fprintln(a.Stdout, "  "+strings.Join(cmd.Path, " ")+" <command> [flags]")
		fmt.Fprintln(a.Stdout, "\nCommands:")
		for _, child := range a.Registry.commandTree().Groups[cmd.Name].Commands {
			label := strings.TrimPrefix(commandUsage(child), strings.Join(cmd.Path, " ")+" ")
			description := child.Description
			if child.Audience == "buyer" {
				description += " (" + buyerCredentialName(child) + ")"
			}
			fmt.Fprintf(a.Stdout, "  %-44s %-40s %s %s\n", label, description, child.Method, child.APIPath)
		}
		fmt.Fprintln(a.Stdout, "\nRun "+strings.Join(cmd.Path, " ")+" <command> --help for flags and examples.")
		return
	}

	fmt.Fprintln(a.Stdout, cmd.Description)
	if cmd.Audience == "buyer" {
		fmt.Fprintln(a.Stdout, "Credential: set FLINT_ACCESS_TOKEN to a "+buyerCredentialName(cmd)+". Known incompatible credentials stop locally before any request.")
	}
	fmt.Fprintln(a.Stdout)
	usage := append([]string{}, cmd.Path...)
	for _, arg := range cmd.Arguments {
		if arg.Positional == 0 {
			continue
		}
		name := "<" + arg.Name + ">"
		if arg.Variadic {
			name += "..."
		}
		if !arg.Required {
			name = "[" + name + "]"
		}
		usage = append(usage, name)
	}
	fmt.Fprintln(a.Stdout, "Usage:")
	fmt.Fprintln(a.Stdout, "  "+strings.Join(usage, " ")+" [flags]")
	if len(cmd.Arguments) > 0 {
		fmt.Fprintln(a.Stdout)
		fmt.Fprintln(a.Stdout, "Arguments and flags:")
		for _, arg := range cmd.Arguments {
			printArgumentHelp(a.Stdout, cmd, arg)
		}
	}
	fmt.Fprintln(a.Stdout)
	if !cmd.Local && commandHasRequestBody(cmd) {
		fmt.Fprintln(a.Stdout, "Request body:")
		if required := requiredBodyFields(cmd); len(required) > 0 {
			fmt.Fprintln(a.Stdout, "  Required top-level fields: "+strings.Join(required, ", "))
		}
		fmt.Fprintf(a.Stdout, "  Run flint schema input %s --output json for the full schema.\n", cmd.CanonicalName)
		fmt.Fprintln(a.Stdout, "  --input takes FILE.json, - for stdin, or inline JSON.")
		fmt.Fprintln(a.Stdout)
	}
	fmt.Fprintln(a.Stdout, "Global flags:")
	if supportsContextSelection(cmd) {
		fmt.Fprintln(a.Stdout, "  --context ID  Select an authorized context for this command")
	}
	fmt.Fprintln(a.Stdout, "  --output human|json|ndjson  --quiet  --debug  --no-input  --live  --merchant ID  --profile NAME")
	fmt.Fprintln(a.Stdout, "  --credential-store keyring|file  Credential backend (or FLINT_CREDENTIAL_STORE; defaults to the profile store or keyring)")
	var capabilities []string
	if cmd.Mutation {
		capabilities = append(capabilities, "--idempotency-key KEY", "--dry-run=client", "--confirm")
	}
	if cmd.CanonicalName == "api" {
		capabilities = append(capabilities, "--idempotency-key KEY", "--dry-run=client", "--confirm", "--preview", "--paginate", "--page-size N", "--page-token TOKEN", "--all")
	}
	if cmd.Mutation && cmd.Destructive {
		capabilities = append(capabilities, "--confirm", "--preview")
	} else if cmd.Destructive {
		capabilities = append(capabilities, "--confirm")
	} else if cmd.Sensitive {
		capabilities = append(capabilities, "--confirm")
	}
	if cmd.CanonicalName == "history" {
		capabilities = append(capabilities, "--confirm (with --clear)")
	}
	if cmd.Supports.Pagination {
		capabilities = append(capabilities, "--page-size N", "--page-token TOKEN", "--limit N")
		if !cmd.Stream {
			capabilities = append(capabilities, "--all")
		}
	}
	if expansions, _ := expansionValues(cmd.OperationID); len(expansions) > 0 {
		capabilities = append(capabilities, "--expand "+strings.Join(expansions, ","))
	}
	if cmd.Supports.WaitFor {
		capabilities = append(capabilities, "--wait-for FIELD=VALUE (inside the resource)", "--for DURATION")
	}
	if cmd.Stream || cmd.Follow {
		capabilities = append(capabilities, "--max-events N", "--for DURATION")
	}
	if cmd.Supports.Open {
		capabilities = append(capabilities, "--open (open the checkout URL in an interactive terminal with human output)")
	}
	if cmd.Supports.Field {
		capabilities = append(capabilities, "--field PATH (inside the resource; strings raw, other values JSON)")
	}
	if cmd.Supports.Select {
		capabilities = append(capabilities, "--select FIELDS (inside each resource)")
	}
	if cmd.Supports.JQ {
		capabilities = append(capabilities, "--jq EXPR")
	}
	if !cmd.Local || cmd.CanonicalName == "auth.import" || cmd.CanonicalName == "auth.token" || cmd.CanonicalName == "auth.login" || cmd.CanonicalName == "auth.reauth" || strings.HasPrefix(cmd.CanonicalName, "context.") || cmd.CanonicalName == "auth.logout" || cmd.CanonicalName == "config.validate" || cmd.CanonicalName == "doctor" || cmd.CanonicalName == "upgrade" || cmd.CanonicalName == "init" || cmd.CanonicalName == "signup" || cmd.CanonicalName == "mcp.serve" || cmd.CanonicalName == "help.search" {
		capabilities = append(capabilities, "--timeout DURATION")
	}
	if cmd.Stream || cmd.Supports.Pagination || cmd.Supports.WaitFor || cmd.CanonicalName == "api" || cmd.CanonicalName == "upgrade" {
		capabilities = append(capabilities, "--progress auto|plain|json|quiet")
	}
	seenCapabilities := map[string]bool{}
	unique := capabilities[:0]
	for _, capability := range capabilities {
		key := strings.Fields(capability)[0]
		if !seenCapabilities[key] {
			seenCapabilities[key] = true
			unique = append(unique, capability)
		}
	}
	capabilities = unique
	if len(capabilities) > 0 {
		fmt.Fprintln(a.Stdout)
		fmt.Fprintln(a.Stdout, "Capability flags:")
		fmt.Fprintln(a.Stdout, "  "+strings.Join(capabilities, "  "))
		if !cmd.Local {
			fmt.Fprintln(a.Stdout, "  --timeout is the total request budget including retries (default 150s).")
		}
	}
	if len(cmd.Examples) > 0 {
		fmt.Fprintln(a.Stdout)
		fmt.Fprintln(a.Stdout, "Examples:")
		for _, example := range cmd.Examples {
			fmt.Fprintln(a.Stdout, "  "+example)
		}
	}
}

func (a *App) printRootHelp() { a.printTreeRootHelp(false) }
