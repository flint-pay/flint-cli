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
	"private": true, "no-open": true,
}

var globalFlags = map[string]bool{
	"output": true, "quiet": true, "debug": true, "no-input": true, "live": true,
	"merchant": true, "profile": true, "color": true, "confirm": true, "dry-run": true,
	"idempotency-key": true, "jq": true, "select": true, "field": true, "expand": true,
	"open": true, "page-size": true, "page-token": true, "all": true, "timeout": true,
	"max-events": true, "for": true, "wait-for": true, "progress": true, "input": true,
	"paginate": true, "preview": true, "clear": true, "stdin": true, "fix": true, "help": true,
}

func parseInvocation(r *Registry, argv []string) (*Command, Options, bool, *CLIError) {
	opts := defaultOptions()
	remaining, err := extractFlags(argv, globalFlags, &opts, false)
	if err != nil {
		return nil, opts, false, err
	}
	if len(remaining) == 0 {
		if len(opts.Raw["help"]) > 0 {
			if cmd, ok := r.ByName("help"); ok {
				return cmd, opts, false, nil
			}
		}
		return nil, opts, false, usageError("MISSING_COMMAND", "A command is required.", "")
	}
	var cmd *Command
	consumed := 0
	for n := min(8, len(remaining)); n >= 1; n-- {
		candidate := append([]string{"flint"}, remaining[:n]...)
		if c, ok := r.ByPath(candidate); ok {
			cmd = c
			consumed = n
			break
		}
	}
	if cmd == nil {
		groupPath := append([]string{"flint"}, remaining...)
		if len(r.commandsUnder(groupPath)) > 0 {
			name := strings.Join(remaining, ".")
			return &Command{Name: name, CanonicalName: name, Path: groupPath, Local: true, Group: true}, opts, true, nil
		}
		input := remaining[0]
		suggestions := r.NounSuggestions(input)
		msg := "Unknown command: " + strings.Join(remaining[:min(2, len(remaining))], " ") + "."
		if len(suggestions) > 0 {
			msg += " Did you mean " + suggestions[0] + "?"
		}
		e := usageError("UNKNOWN_COMMAND", msg, "")
		e.Details = []map[string]any{{"input": input, "suggestions": suggestions}}
		return nil, opts, false, e
	}
	commandFlags := map[string]bool{}
	for k := range globalFlags {
		commandFlags[k] = true
	}
	for _, a := range cmd.Arguments {
		if a.Flag != "" {
			commandFlags[strings.TrimPrefix(a.Flag, "--")] = true
		}
	}
	tail, err := extractKnownFlags(remaining[consumed:], commandFlags, &opts)
	if err != nil {
		return nil, opts, false, err
	}
	opts.Positionals = tail
	help := len(opts.Raw["help"]) > 0
	if opts.Output == "json" || opts.Output == "ndjson" {
		opts.NoInput = true
	}
	if err := validateOptions(cmd, &opts, help); err != nil {
		return nil, opts, help, err
	}
	return cmd, opts, help, nil
}

func defaultOptions() Options {
	return Options{Output: "human", Color: "auto", Progress: "auto", Timeout: 30 * time.Second, Input: "", Raw: map[string][]string{}}
}

func extractKnownFlags(args []string, allowed map[string]bool, opts *Options) ([]string, *CLIError) {
	return extractFlags(args, allowed, opts, true)
}

func extractFlags(args []string, allowed map[string]bool, opts *Options, rejectUnknown bool) ([]string, *CLIError) {
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			remaining = append(remaining, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(token, "--") {
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
			e := usageError("UNKNOWN_FLAG", "Unknown flag: --"+key+".", key)
			e.Details = []map[string]any{{"input": "--" + key, "suggestions": suggestFlags(key, allowed)}}
			return nil, e
		}
		if boolFlags[key] {
			if !hasValue {
				value = "true"
			}
			if value != "true" && value != "false" {
				return nil, usageError("INVALID_FLAG_VALUE", "--"+key+" must be true or false.", key)
			}
		} else if !hasValue {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return nil, usageError("MISSING_FLAG_VALUE", "Missing value for --"+key+".", key)
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
	if help {
		return nil
	}
	if o.Output != "human" && o.Output != "json" && o.Output != "ndjson" {
		return usageError("INVALID_OUTPUT", "--output must be human, json, or ndjson.", "output")
	}
	if o.Output == "ndjson" && !cmd.Stream {
		return usageError("UNSUPPORTED_FLAG", "--output ndjson applies only to streaming commands.", "output")
	}
	if o.Color != "auto" && o.Color != "always" && o.Color != "never" {
		return usageError("INVALID_COLOR", "--color must be auto, always, or never.", "color")
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
	rawMutation := rawMethod != "" && rawMethod != "GET" && rawMethod != "HEAD"
	used := func(name string) bool { return len(o.Raw[name]) > 0 }
	for _, item := range []struct {
		name  string
		value string
	}{
		{"profile", o.Profile},
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
	if o.DryRun != "" && !cmd.Mutation && !rawMutation {
		return usageError("UNSUPPORTED_FLAG", "--dry-run applies only to mutations.", "dry-run")
	}
	if used("idempotency-key") && !cmd.Mutation && !rawMutation {
		return usageError("UNSUPPORTED_FLAG", "--idempotency-key applies only to mutations.", "idempotency-key")
	}
	if used("preview") && !(cmd.Mutation && cmd.Destructive) && !(cmd.CanonicalName == "api" && rawMethod == "DELETE") {
		return usageError("UNSUPPORTED_FLAG", "--preview applies only to destructive mutations.", "preview")
	}
	confirmApplies := cmd.Sensitive || cmd.Destructive || rawMutation || (cmd.CanonicalName == "history" && o.Clear)
	if used("confirm") && !confirmApplies {
		return usageError("UNSUPPORTED_FLAG", "--confirm applies only to sensitive or destructive mutations.", "confirm")
	}
	if used("input") && !cmd.Mutation && !rawMutation {
		return usageError("UNSUPPORTED_FLAG", "--input applies only to mutations.", "input")
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
	if o.For > 0 && !cmd.Stream && len(o.WaitFor) == 0 {
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
	if used("all") && cmd.Stream {
		return usageError("UNSUPPORTED_FLAG", "--all does not apply to streaming commands, which traverse pages continuously.", "all")
	}
	if used("open") && !cmd.Supports.Open {
		return usageError("UNSUPPORTED_FLAG", "--open is not supported by this command.", "open")
	}
	if (used("all") || used("page-size") || used("page-token")) && !cmd.Supports.Pagination && !cmd.Stream && cmd.CanonicalName != "api" {
		return usageError("UNSUPPORTED_FLAG", "Pagination flags are not supported by this command.", "page-size")
	}
	if used("paginate") && cmd.CanonicalName != "api" {
		return usageError("UNSUPPORTED_FLAG", "--paginate applies only to flint api get.", "paginate")
	}
	if used("expand") && cmd.Local {
		return usageError("UNSUPPORTED_FLAG", "--expand applies only to API commands with documented expansions.", "expand")
	}
	if used("expand") {
		if err := validateExpansions(cmd.OperationID, o.Expand); err != nil {
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
	if used("progress") && !cmd.Stream && !o.All && !o.Paginate && len(o.WaitFor) == 0 {
		return usageError("UNSUPPORTED_FLAG", "--progress applies only to pagination, streams, and waits.", "progress")
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
	e := cliError(ExitUsage, "usage_error", code, message)
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

func suggestFlags(input string, allowed map[string]bool) []string {
	var out []string
	for candidate := range allowed {
		if editDistance(input, candidate) <= 3 {
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
	normalized := strings.ReplaceAll(input, "_", "-")
	if normalized == "payments" {
		return []string{"payment-intents", "payment"}
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range r.Commands {
		paths := append([][]string{c.Path}, c.AliasPaths...)
		for _, path := range paths {
			if len(path) < 2 {
				continue
			}
			candidate := path[1]
			if !seen[candidate] && editDistance(normalized, candidate) <= 6 {
				seen[candidate] = true
				out = append(out, candidate)
			}
		}
	}
	sortStringsByDistance(out, input)
	if len(out) > 3 {
		return out[:3]
	}
	return out
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
