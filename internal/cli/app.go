package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
		Info:             info,
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
		Stdin:            os.Stdin,
		IsTTY:            func() bool { return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd())) },
		Now:              time.Now,
		Registry:         NewRegistry(),
		LoadCredential:   loadKeychainCredential,
		StoreCredential:  storeKeychainCredential,
		DeleteCredential: deleteKeychainCredential,
	}
}

func (a *App) Run(argv []string) int {
	cmd, opts, help, parseErr := parseInvocation(a.Registry, argv)
	applyEnvironmentOptions(&opts)
	if parseErr != nil {
		a.writeError(parseErr, opts)
		return parseErr.ExitCode
	}
	if envErr := validateOptions(cmd, &opts, help); envErr != nil {
		return a.fail(envErr, opts)
	}
	if help {
		a.printCommandHelp(cmd)
		return ExitOK
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
	effective := resolveAPICommand(cmd, opts)
	key, baseURL, authEnvelope, authErr := a.authenticateCommand(ctx, effective, opts, resolved)
	if authErr != nil {
		return a.fail(authErr, opts)
	}
	authContext := authEnvelope.Data
	resolved.CredentialScope = authContext.CredentialScope
	resolved.Environment = normalizeEnvironment(authContext.Environment)
	resolved.APIKeyID = authContext.APIKeyID
	resolved.MerchantID = authContext.MerchantID
	resolved.SandboxID = authContext.SandboxID
	if e := validateCredentialIntentForCommand(authContext, opts, resolved.MerchantGuard, resolved.SandboxGuard, effective.EnvironmentAffinity); effective.AuthRequired && e != nil {
		return a.fail(e, opts)
	}
	if cmd.CanonicalName == "auth.status" {
		if e := a.writeResult(map[string]any{"data": authEnvelope.Data, "request_id": authEnvelope.RequestID, "meta": authEnvelope.Meta}, cmd, opts); e != nil {
			return a.fail(e, opts)
		}
		return ExitOK
	}
	if e := a.confirmCommand(effective, authContext, opts); e != nil {
		return a.fail(e, opts)
	}
	value, runErr := a.executeAPI(ctx, effective, opts, resolved, authContext, key, baseURL)
	if runErr != nil {
		if runErr.ExitCode == ExitWait && value != nil {
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
	if !opts.DryRunClient() && (effective.Mutation || effective.Get) {
		if err := a.recordHistory(value, effective.CanonicalName, historyScopeFromResolved(resolved)); err != nil && !opts.Quiet {
			fmt.Fprintln(a.Stderr, "warning: could not record resource history: "+err.Error())
		}
	}
	if e := a.writeResult(value, cmd, opts); e != nil {
		return a.fail(e, opts)
	}
	return ExitOK
}

func (o Options) DryRunClient() bool { return o.DryRun == "client" || o.Preview }

func applyEnvironmentOptions(o *Options) {
	if len(o.Raw["output"]) == 0 {
		if v := strings.TrimSpace(os.Getenv("FLINT_OUTPUT")); v != "" {
			o.Output = v
		}
	}
	if len(o.Raw["color"]) == 0 {
		if v := strings.TrimSpace(os.Getenv("FLINT_COLOR")); v != "" {
			o.Color = v
		}
	}
	if v := strings.TrimSpace(os.Getenv("FLINT_NO_INPUT")); v == "1" || strings.EqualFold(v, "true") {
		o.NoInput = true
	}
	if o.Output == "json" || o.Output == "ndjson" {
		o.NoInput = true
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
	env := normalizeEnvironment(authContext.Environment)
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
	needs := cmd.Destructive || (cmd.Sensitive && normalizeEnvironment(authContext.Environment) == "live")
	if !needs || opts.Confirm || opts.Preview {
		return nil
	}
	message := "This operation requires confirmation."
	code := "CONFIRMATION_REQUIRED"
	if normalizeEnvironment(authContext.Environment) == "live" {
		message = "This operation affects live production data and requires confirmation."
		code = "PRODUCTION_CONFIRMATION_REQUIRED"
	}
	if opts.NoInput || !a.IsTTY() {
		return &CLIError{ExitCode: ExitConfirmation, Type: "confirmation_required", Code: code, Message: message + " Re-run with --confirm."}
	}
	fmt.Fprintf(a.Stderr, "%s Continue? [y/N] ", message)
	line, err := bufio.NewReader(&contextInputReader{ctx: a.commandContext(), reader: a.Stdin}).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return configError("CONFIRMATION_READ_FAILED", "Could not read confirmation.", err)
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line != "y" && line != "yes" {
		return &CLIError{ExitCode: ExitConfirmation, Type: "confirmation_required", Code: "CONFIRMATION_DECLINED", Message: "Operation canceled."}
	}
	return nil
}

func configError(code, message string, cause error) *CLIError {
	if errors.Is(cause, context.Canceled) {
		return networkError("REQUEST_CANCELED", "The command was canceled.", cause)
	}
	return &CLIError{ExitCode: ExitAuth, Type: "configuration_error", Code: code, Message: message, Cause: cause}
}
func (a *App) fail(err *CLIError, opts Options) int { a.writeError(err, opts); return err.ExitCode }

func (a *App) printCommandHelp(cmd *Command) {
	if cmd.Group {
		fmt.Fprintln(a.Stdout, "Usage:")
		fmt.Fprintln(a.Stdout, "  "+strings.Join(cmd.Path, " ")+" <command> [flags]")
		fmt.Fprintln(a.Stdout)
		fmt.Fprintln(a.Stdout, "Commands:")
		for _, child := range a.Registry.commandsUnder(cmd.Path) {
			fmt.Fprintf(a.Stdout, "  %-36s %s\n", strings.Join(child.Path[len(cmd.Path):], " "), child.Description)
		}
		return
	}
	fmt.Fprintln(a.Stdout, cmd.Description)
	fmt.Fprintln(a.Stdout)
	fmt.Fprintln(a.Stdout, "Usage:")
	fmt.Fprintln(a.Stdout, "  "+strings.Join(cmd.Path, " ")+" [flags]")
	if len(cmd.Arguments) > 0 {
		fmt.Fprintln(a.Stdout)
		fmt.Fprintln(a.Stdout, "Arguments and flags:")
		for _, arg := range cmd.Arguments {
			label := arg.Flag
			if label == "" {
				label = arg.Name
				if arg.Variadic {
					label += "..."
				}
			}
			required := ""
			if arg.Required {
				required = " (required)"
			}
			fmt.Fprintf(a.Stdout, "  %-28s %s%s\n", label, arg.Description, required)
		}
	}
	fmt.Fprintln(a.Stdout)
	fmt.Fprintln(a.Stdout, "Global flags:")
	fmt.Fprintln(a.Stdout, "  --output human|json|ndjson  --quiet  --debug  --no-input  --live  --merchant ID  --profile NAME  --color auto|always|never")
	var capabilities []string
	if cmd.Mutation {
		capabilities = append(capabilities, "--idempotency-key KEY", "--dry-run=client")
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
		capabilities = append(capabilities, "--page-size N", "--page-token TOKEN")
		if !cmd.Stream {
			capabilities = append(capabilities, "--all")
		}
	}
	if cmd.Supports.WaitFor {
		capabilities = append(capabilities, "--wait-for FIELD=VALUE", "--for DURATION")
	}
	if cmd.Stream {
		capabilities = append(capabilities, "--max-events N", "--for DURATION")
	}
	if cmd.Supports.Open {
		capabilities = append(capabilities, "--open")
	}
	if cmd.Supports.Field {
		capabilities = append(capabilities, "--field PATH")
	}
	if cmd.Supports.Select {
		capabilities = append(capabilities, "--select FIELDS")
	}
	if cmd.Supports.JQ {
		capabilities = append(capabilities, "--jq EXPR")
	}
	if !cmd.Local || cmd.CanonicalName == "auth.import" || cmd.CanonicalName == "config.validate" || cmd.CanonicalName == "doctor" || cmd.CanonicalName == "init" || cmd.CanonicalName == "signup" || cmd.CanonicalName == "mcp.serve" || cmd.CanonicalName == "help.search" {
		capabilities = append(capabilities, "--timeout DURATION")
	}
	if cmd.Stream || cmd.Supports.Pagination || cmd.Supports.WaitFor || cmd.CanonicalName == "api" {
		capabilities = append(capabilities, "--progress auto|plain|json|quiet")
	}
	if len(capabilities) > 0 {
		fmt.Fprintln(a.Stdout)
		fmt.Fprintln(a.Stdout, "Capability flags:")
		fmt.Fprintln(a.Stdout, "  "+strings.Join(capabilities, "  "))
	}
	if len(cmd.Examples) > 0 {
		fmt.Fprintln(a.Stdout)
		fmt.Fprintln(a.Stdout, "Examples:")
		for _, example := range cmd.Examples {
			fmt.Fprintln(a.Stdout, "  "+example)
		}
	}
}

func (a *App) printRootHelp() {
	fmt.Fprintln(a.Stdout, "Flint Pay command-line interface")
	fmt.Fprintln(a.Stdout)
	fmt.Fprintln(a.Stdout, "Usage:")
	fmt.Fprintln(a.Stdout, "  flint <command> [flags]")
	fmt.Fprintln(a.Stdout, "  --output human|json|ndjson")
	fmt.Fprintln(a.Stdout)
	fmt.Fprintln(a.Stdout, "Start here:")
	for _, line := range []string{"flint init", "flint signup", "flint auth import", "flint doctor", "flint schema commands --output json", "flint help test-cards", "flint help search <question>", "flint support open --request-id <id>"} {
		fmt.Fprintln(a.Stdout, "  "+line)
	}
	fmt.Fprintln(a.Stdout)
	fmt.Fprintln(a.Stdout, "Run flint schema commands --output json for the complete command catalog.")
}
