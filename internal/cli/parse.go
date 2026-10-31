package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var boolFlags = map[string]bool{
	"quiet": true, "debug": true, "no-input": true, "live": true, "confirm": true,
	"open": true, "all": true, "paginate": true, "preview": true, "clear": true,
	"stdin": true, "fix": true, "help": true,
	// Command-scoped booleans. They stay out of globalFlags so only the command
	// that declares them accepts them, but the parser still has to know they
	// take no value.
	"list": true, "new-session": true, "private": true, "no-open": true, "ai-agent": true,
	"read-only": true, "allow-live": true, "allow-destructive": true, "no-wait": true, "check": true, "print-secret": true,
}

var globalFlags = map[string]bool{
	"credential-store": true,
	"output":           true, "quiet": true, "debug": true, "no-input": true, "live": true,
	"context": true, "merchant": true, "profile": true, "color": true, "confirm": true, "dry-run": true,
	"idempotency-key": true, "jq": true, "select": true, "field": true, "expand": true,
	"limit": true, "open": true, "page-size": true, "page-token": true, "all": true, "timeout": true,
	"max-events": true, "for": true, "wait-for": true, "progress": true, "input": true,
	"paginate": true, "preview": true, "clear": true, "stdin": true, "fix": true, "help": true,
}

func parseInvocation(r *Registry, argv []string) (*Command, Options, bool, *CLIError) {
	opts := defaultOptions()
	remaining, err := extractFlags(argv, globalFlags, &opts, false)
	if err != nil {
		return nil, opts, false, err
	}
	flagsTerminated := len(remaining) > 0 && remaining[0] == "--"
	if flagsTerminated {
		remaining = remaining[1:]
	}
	if len(remaining) == 1 && remaining[0] == "--version" {
		remaining = []string{"version"}
	}
	if len(remaining) > 0 && !flagsTerminated && strings.HasPrefix(remaining[0], "-") {
		return nil, opts, false, unknownFlagError(remaining[0], globalFlags)
	}
	if len(remaining) == 0 {
		if cmd, ok := r.ByName("help"); ok {
			return cmd, opts, false, nil
		}
		return nil, opts, false, usageError("MISSING_COMMAND", "A command is required.", "")
	}
	resolution := r.resolveWords(remaining)
	cmd, consumed := resolution.Command, resolution.Consumed
	if cmd == nil {
		if resolution.Group != nil {
			return groupCommand(resolution.Group), opts, true, nil
		}
		input := strings.Join(commandWords(remaining), " ")
		e := usageError("UNKNOWN_COMMAND", "Unknown command: "+input+". Run flint help search "+input+" to find commands.", "")
		e.Details = suggestionDetails(input, resolution.Suggestions)
		return nil, opts, false, e
	}

	opts.streamCommand = cmd.Stream
	commandFlags := map[string]bool{}
	for k := range globalFlags {
		commandFlags[k] = true
	}
	for _, a := range cmd.Arguments {
		if a.Flag != "" {
			commandFlags[strings.TrimPrefix(a.Flag, "--")] = true
		}
	}
	tail := remaining[consumed:]
	if !flagsTerminated {
		tail, err = extractKnownFlags(tail, commandFlags, &opts)
		if err != nil {
			return nil, opts, false, err
		}
	}
	opts.Positionals = tail
	for _, arg := range cmd.Arguments {
		if arg.Positional > 0 && arg.IDPrefix != "" && len(opts.Positionals) >= arg.Positional {
			value := strings.TrimSpace(opts.Positionals[arg.Positional-1])
			opts.Positionals[arg.Positional-1] = value
			if e := validateIDPrefix(value, arg); e != nil {
				return nil, opts, false, e
			}
		}
	}
	help := helpRequested(opts)
	if opts.Output == "json" || opts.Output == "ndjson" {
		opts.NoInput = true
	}
	if err := validateOptions(cmd, &opts, help); err != nil {
		return nil, opts, help, err
	}
	return cmd, opts, help, nil
}

func helpRequested(opts Options) bool {
	values := opts.Raw["help"]
	return len(values) > 0 && values[len(values)-1] == "true"
}

var defaultRequestTimeout = 150 * time.Second

func defaultOptions() Options {
	return Options{Output: "human", Color: "auto", Progress: "auto", Timeout: defaultRequestTimeout, Input: "", Raw: map[string][]string{}}
}

func extractKnownFlags(args []string, allowed map[string]bool, opts *Options) ([]string, *CLIError) {
	return extractFlags(args, allowed, opts, true)
}

func extractFlags(args []string, allowed map[string]bool, opts *Options, rejectUnknown bool) ([]string, *CLIError) {
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		token := args[i]
		switch token {
		case "-h":
			token = "--help"
		case "-o":
			token = "--output"
		}
		if token == "--" {
			// The command-specific pass must see the terminator too, so it
			// cannot reinterpret literal positional values as flags.
			if !rejectUnknown {
				remaining = append(remaining, token)
			}
			remaining = append(remaining, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(token, "--") {
			if rejectUnknown && isShortFlag(token) {
				return nil, unknownFlagError(token, allowed)
			}
			remaining = append(remaining, token)
			continue
		}
		keyValue := strings.TrimPrefix(token, "--")
		key, value, hasValue := strings.Cut(keyValue, "=")
		if !allowed[key] {
			if !rejectUnknown {
				remaining = append(remaining, token)
				continue
			}
			return nil, unknownFlagError(token, allowed)
		}
		if boolFlags[key] {
			if !hasValue {
				value = "true"
				if i+1 < len(args) && (args[i+1] == "true" || args[i+1] == "false") {
					i++
					value = args[i]
				}
			}
			if value != "true" && value != "false" {
				return nil, usageError("INVALID_FLAG_VALUE", "--"+key+" must be true or false.", key)
			}
		} else if !hasValue {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				if key == "dry-run" {
					return nil, usageError("MISSING_FLAG_VALUE", "Missing value for --dry-run. Use --dry-run=client.", key)
				}
				return nil, usageError("MISSING_FLAG_VALUE", "Missing value for --"+key+". If the value starts with --, use --"+key+"=VALUE.", key)
			}
			i++
			value = args[i]
		}
		if err := setOption(opts, key, value); err != nil {
			return nil, err
		}
	}
	return remaining, nil
}

func setOption(o *Options, key, value string) *CLIError {
	o.Raw[key] = append(o.Raw[key], value)
	boolValue := value == "true"
	switch key {
	case "context":
		o.ContextID = value
	case "output":
		o.Output = value
	case "quiet":
		o.Quiet = boolValue
	case "debug":
		o.Debug = boolValue
	case "no-input":
		o.NoInput = boolValue
	case "live":
		o.Live = boolValue
	case "merchant":
		o.Merchant = value
	case "credential-store":
		o.CredentialStore = value
	case "profile":
		o.Profile = value
	case "color":
		o.Color = value
	case "confirm":
		o.Confirm = boolValue
	case "dry-run":
		o.DryRun = value
	case "idempotency-key":
		o.IdempotencyKey = value
	case "jq":
		o.JQ = value
	case "select":
		o.Select = append(o.Select, splitCSV(value)...)
	case "field":
		o.Field = value
	case "expand":
		o.Expand = append(o.Expand, splitCSV(value)...)
	case "open":
		o.Open = boolValue
	case "limit":
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 {
			return usageError("INVALID_LIMIT", "--limit must be a positive integer.", key)
		}
		o.Limit = n
	case "page-size":
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 || n > 100 {
			return usageError("INVALID_PAGE_SIZE", "--page-size must be between 1 and 100.", key)
		}
		o.PageSize = n
	case "page-token":
		o.PageToken = value
	case "all":
		o.All = boolValue
	case "timeout":
		d, e := time.ParseDuration(value)
		if e != nil || d <= 0 {
			return usageError("INVALID_DURATION", "--timeout must be a positive duration.", key)
		}
		o.Timeout = d
	case "max-events":
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 {
			return usageError("INVALID_MAX_EVENTS", "--max-events must be a positive integer.", key)
		}
		o.MaxEvents = n
	case "for":
		d, e := time.ParseDuration(value)
		if e != nil || d <= 0 {
			return usageError("INVALID_DURATION", "--for must be a positive duration.", key)
		}
		o.For = d
	case "wait-for":
		o.WaitFor = append(o.WaitFor, value)
	case "progress":
		o.Progress = value
	case "input":
		o.Input = value
	case "paginate":
		o.Paginate = boolValue
	case "preview":
		o.Preview = boolValue
	case "clear":
		o.Clear = boolValue
	case "stdin":
		o.Stdin = boolValue
	case "fix":
		o.Fix = boolValue
	case "help":
	default: // Command-specific values remain available through Raw.
	}
	return nil
}

func validateOptions(cmd *Command, o *Options, help bool) *CLIError {
	if o.CredentialStore != "" && o.CredentialStore != "keyring" && o.CredentialStore != "file" {
		return usageError("INVALID_CREDENTIAL_STORE", "--credential-store or FLINT_CREDENTIAL_STORE must be keyring or file.", "credential-store")
	}
	if help {
		return nil
	}
	for _, arg := range cmd.Arguments {
		if arg.Query == "" || len(arg.Enum) == 0 || cmd.CanonicalName == "listen" {
			continue
		}
		for _, value := range o.Raw[strings.TrimPrefix(arg.Flag, "--")] {
			if !containsString(arg.Enum, value) {
				return usageError("INVALID_ARGUMENT", arg.Flag+" must be one of: "+strings.Join(arg.Enum, ", ")+".", arg.Name)
			}
		}
	}
	if o.Output != "human" && o.Output != "json" && o.Output != "ndjson" {
		source := "--output"
		if o.outputFromEnvironment {
			source = "FLINT_OUTPUT"
		}
		return usageError("INVALID_OUTPUT", source+" must be human, json, or ndjson.", "output")
	}
	if o.Output == "ndjson" && !cmd.Supports.NDJSON && o.outputFromEnvironment {
		o.Output = "json"
	}
	if o.Output == "ndjson" && !cmd.Supports.NDJSON {
		return usageError("UNSUPPORTED_FLAG", "--output ndjson applies only to streaming and paginated list commands.", "output")
	}
	if o.Output == "ndjson" && !cmd.Stream && (o.JQ != "" || o.Field != "" || len(o.WaitFor) > 0) {
		return usageError("UNSUPPORTED_FLAG", "--jq, --field, and --wait-for do not apply to list NDJSON output; use --select to choose resource fields.", "output")
	}
	if o.Progress != "auto" && o.Progress != "plain" && o.Progress != "json" && o.Progress != "quiet" {
		return usageError("INVALID_PROGRESS", "--progress must be auto, plain, json, or quiet.", "progress")
	}
	if o.DryRun != "" && o.DryRun != "client" {
		return usageError("INVALID_DRY_RUN", "Only --dry-run=client is supported.", "dry-run")
	}
	rawMethod := ""
	if cmd.CanonicalName == "api" && len(o.Positionals) > 0 {
		rawMethod = strings.ToUpper(o.Positionals[0])
	}
	effective := resolveAPICommand(cmd, *o)
	if cmd.CanonicalName == "api" && len(o.Positionals) >= 2 {
		if e := validateRawPublicRoute(effective.Method, effective.APIPath); e != nil {
			return e
		}
	}
	rawMutation := cmd.CanonicalName == "api" && effective.Mutation
	used := func(name string) bool { return len(o.Raw[name]) > 0 }
	for _, item := range []struct {
		name  string
		value string
	}{
		{"context", o.ContextID},
		{"profile", o.Profile},
		{"credential-store", o.CredentialStore},
		{"merchant", o.Merchant},
		{"dry-run", o.DryRun},
		{"idempotency-key", o.IdempotencyKey},
		{"field", o.Field},
		{"jq", o.JQ},
		{"page-token", o.PageToken},
		{"input", o.Input},
	} {
		if used(item.name) && strings.TrimSpace(item.value) == "" {
			return usageError("INVALID_FLAG_VALUE", "--"+item.name+" must not be empty.", item.name)
		}
	}
	if used("select") && len(o.Select) == 0 {
		return usageError("INVALID_FLAG_VALUE", "--select must contain at least one field.", "select")
	}
	if used("expand") && len(o.Expand) == 0 {
		return usageError("INVALID_FLAG_VALUE", "--expand must contain at least one relationship.", "expand")
	}
	if o.DryRun != "" && cmd.CanonicalName == "upgrade" {
		return usageError("UNSUPPORTED_FLAG", "Use flint upgrade --check to check for an upgrade.", "dry-run")
	}
	if o.DryRun != "" && !cmd.Mutation && !rawMutation {
		return usageError("UNSUPPORTED_FLAG", "--dry-run applies only to mutations.", "dry-run")
	}
	if used("idempotency-key") && !cmd.Mutation && !rawMutation {
		return usageError("UNSUPPORTED_FLAG", "--idempotency-key applies only to mutations.", "idempotency-key")
	}
	if used("preview") && !(effective.Mutation && effective.Destructive) {
		return usageError("UNSUPPORTED_FLAG", "--preview applies only to destructive mutations.", "preview")
	}
	if used("context") && !supportsContextSelection(cmd) {
		return usageError("UNSUPPORTED_FLAG", "--context does not apply to this command.", "context")
	}

	confirmApplies := cmd.Mutation || cmd.Sensitive || cmd.Destructive || rawMutation || (cmd.CanonicalName == "history" && o.Clear)
	if used("confirm") && !confirmApplies {
		return usageError("UNSUPPORTED_FLAG", "--confirm applies only to mutations.", "confirm")
	}
	if used("input") && !cmd.Mutation && !rawMutation && cmd.InputSchema == "" {
		return usageError("UNSUPPORTED_FLAG", "--input requires a command with a request body.", "input")
	}
	if len(o.WaitFor) > 0 && !cmd.Supports.WaitFor {
		return usageError("UNSUPPORTED_FLAG", "--wait-for applies only to single-resource get commands.", "wait-for")
	}
	if len(o.WaitFor) > 0 {
		if _, err := parseWaitConditions(o.WaitFor); err != nil {
			return err
		}
	}
	if o.MaxEvents > 0 && !cmd.Stream {
		return usageError("UNSUPPORTED_FLAG", "--max-events and --for apply only to streaming commands.", "for")
	}
	if o.For > 0 && !cmd.Stream && len(o.WaitFor) == 0 && cmd.CanonicalName != "trigger" {
		return usageError("UNSUPPORTED_FLAG", "--for applies only to streaming commands and bounded waits.", "for")
	}
	if o.JQ != "" && o.Field != "" {
		return usageError("CONFLICTING_FLAGS", "--jq and --field cannot be used together.", "field")
	}
	if (used("field") && !cmd.Supports.Field) || (used("select") && !cmd.Supports.Select) || (used("jq") && !cmd.Supports.JQ) {
		return usageError("UNSUPPORTED_FLAG", "--field, --select, and --jq do not apply to this command.", "field")
	}
	if o.Paginate && (used("field") || used("select") || used("jq")) {
		return usageError("UNSUPPORTED_FLAG", "Output transforms do not apply to --paginate NDJSON output.", "paginate")
	}
	if o.All && o.PageToken != "" {
		return usageError("CONFLICTING_FLAGS", "--all and --page-token cannot be used together.", "page-token")
	}
	if used("limit") && (!cmd.Supports.Pagination || cmd.Stream || cmd.CanonicalName == "api") {
		return usageError("UNSUPPORTED_FLAG", "--limit applies only to paginated list commands.", "limit")
	}
	if used("all") && cmd.Stream {
		return usageError("UNSUPPORTED_FLAG", "--all does not apply to streaming commands, which traverse pages continuously.", "all")
	}
	if used("open") && !cmd.Supports.Open {
		return usageError("UNSUPPORTED_FLAG", "--open is not supported by this command.", "open")
	}
	if (used("limit") || used("all") || used("page-size") || used("page-token")) && !cmd.Supports.Pagination && !cmd.Stream && cmd.CanonicalName != "api" && cmd.CanonicalName != "help" {
		return usageError("UNSUPPORTED_FLAG", "Pagination flags are not supported by this command.", "page-size")
	}
	if used("paginate") && cmd.CanonicalName != "api" {
		return usageError("UNSUPPORTED_FLAG", "--paginate applies only to flint api get.", "paginate")
	}
	if cmd.CanonicalName == "api" && (o.All || o.Paginate) && (rawMethod != "GET" || rawMutation) {
		return usageError("PAGINATION_REQUIRES_GET", "--all and --paginate require a read-only flint api get operation.", "method")
	}
	if used("expand") && cmd.Local {
		return usageError("UNSUPPORTED_FLAG", "--expand applies only to API commands with documented expansions.", "expand")
	}
	if used("expand") {
		if err := validateExpansions(effective.OperationID, o.Expand); err != nil {
			return err
		}
	}
	if used("stdin") && cmd.CanonicalName != "auth.import" {
		return usageError("UNSUPPORTED_FLAG", "--stdin applies only to flint auth import.", "stdin")
	}
	if used("clear") && cmd.CanonicalName != "history" {
		return usageError("UNSUPPORTED_FLAG", "--clear applies only to flint history.", "clear")
	}
	if used("fix") && cmd.CanonicalName != "doctor" {
		return usageError("UNSUPPORTED_FLAG", "--fix applies only to flint doctor.", "fix")
	}
	if used("progress") && !cmd.Stream && !o.All && !o.Paginate && o.Limit == 0 && len(o.WaitFor) == 0 && cmd.CanonicalName != "upgrade" && cmd.CanonicalName != "trigger" {
		return usageError("UNSUPPORTED_FLAG", "--progress applies only to pagination, streams, waits, and upgrades.", "progress")
	}
	maxPositional := 0
	variadic := false
	for _, a := range cmd.Arguments {
		maxPositional = max(maxPositional, a.Positional)
		if a.Positional > 0 && a.Variadic && a.Positional == maxPositional {
			variadic = true
		}
		if a.Positional > 0 && a.Required && len(o.Positionals) < a.Positional {
			return usageError("MISSING_REQUIRED_ARGUMENT", fmt.Sprintf("Missing required argument: %s", a.Name), a.Name)
		}
		if a.Flag != "" && a.Required && len(o.Raw[strings.TrimPrefix(a.Flag, "--")]) == 0 && (o.Input == "" || a.Query != "") {
			return usageError("MISSING_REQUIRED_ARGUMENT", "Missing required argument: "+a.Flag, a.Name)
		}
	}
	// Only a variadic argument in the last position consumes the rest of the
	// line; anything else past the declared positionals is a usage error.
	if !variadic && len(o.Positionals) > maxPositional {
		return usageError("UNEXPECTED_ARGUMENT", "Unexpected argument: "+o.Positionals[maxPositional]+".", o.Positionals[maxPositional])
	}
	return nil
}

func usageError(code, message, param string) *CLIError {
	e := cliError(ExitUsage, errorTypeUsage, code, message)
	e.Param = param
	return e
}

func splitCSV(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Flags that people and agents commonly guess from other CLIs.
var flagAliases = map[string]string{
	"data": "--input", "body": "--input", "json-body": "--input", "json": "--output json", "format": "--output", "version": "flint version",
	"yes": "--confirm", "force": "--confirm", "verbose": "--debug",
}

// isShortFlag reports whether a single-dash token looks like a flag rather
// than a positional value such as "-" (stdin) or a negative number.
func isShortFlag(token string) bool {
	if len(token) < 2 || token[0] != '-' || strings.HasPrefix(token, "--") {
		return false
	}
	_, err := strconv.ParseFloat(token, 64)
	return err != nil
}

func unknownFlagError(token string, allowed map[string]bool) *CLIError {
	key, _, _ := strings.Cut(strings.TrimLeft(token, "-"), "=")
	shown := "--" + key
	message := "Unknown flag: " + shown + "."
	if !strings.HasPrefix(token, "--") {
		shown = token
		message = "Unknown flag: " + token + ". Flint flags use the long form, such as --help or --output json."
		if len(key) == 1 {
			key = ""
		}
	}
	e := usageError("UNKNOWN_FLAG", message, key)
	e.Details = suggestionDetails(shown, suggestFlags(key, allowed))
	return e
}

func suggestionDetails(input string, suggestions []string) map[string]any {
	if suggestions == nil {
		suggestions = []string{}
	}
	return map[string]any{"input": input, "suggestions": suggestions}
}

func suggestFlags(input string, allowed map[string]bool) []string {
	if input == "" {
		return nil
	}
	if alias, ok := flagAliases[input]; ok {
		return []string{alias}
	}
	var out []string
	for candidate := range allowed {
		if editDistance(input, candidate) <= max(1, len(input)/3) || (len(input) >= 4 && strings.HasPrefix(candidate, input)) {
			out = append(out, "--"+candidate)
		}
	}
	sortStringsByDistance(out, "--"+input)
	if len(out) > 3 {
		return out[:3]
	}
	return out
}

func (r *Registry) NounSuggestions(input string) []string {
	return r.commandTree().Suggestions([]string{input})
}

func (r *Registry) subcommandSuggestions(group string, rest []string) []string {
	return r.commandTree().Suggestions(append([]string{group}, rest...))
}

func sortStringsByDistance(values []string, input string) {
	for i := 0; i < len(values); i++ {
		for j := i + 1; j < len(values); j++ {
			di, dj := editDistance(input, values[i]), editDistance(input, values[j])
			if dj < di || (dj == di && values[j] < values[i]) {
				values[i], values[j] = values[j], values[i]
			}
		}
	}
}

func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ca := range ar {
		cur := make([]int, len(br)+1)
		cur[0] = i + 1
		for j, cb := range br {
			cost := 0
			if ca != cb {
				cost = 1
			}
			cur[j+1] = min(cur[j]+1, prev[j+1]+1, prev[j]+cost)
		}
		prev = cur
	}
	return prev[len(br)]
}
