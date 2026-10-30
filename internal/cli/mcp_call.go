package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (a *App) runMCPTool(ctx context.Context, cmd *Command, args map[string]any, opts Options, policy mcpPolicy, structured bool) map[string]any {
	var extra []string
	if cmd.CanonicalName == "history.clear" {
		extra = append(extra, "--clear=true")
	}
	outcome := a.runMCPCommand(ctx, cmd, args, opts, policy, extra)
	if outcome.exit != ExitOK && !outcome.liveBlocked && !policy.allowDestructive && mcpConfirmationRequired(outcome.value) {
		session := mcpSessionFrom(ctx)
		if !session.canElicit() {
			return mcpWithEnvironment(mcpCommandError(cmd, outcome, policy, mcpConfirmationUnavailable(cmd, mcpErrorText(outcome.value))), outcome.environment)
		}
		approved, message := a.mcpAskConfirmation(ctx, session, cmd, args, outcome)
		if !approved {
			return mcpToolError(message, map[string]any{"error": map[string]any{"type": errorTypeConfirmationRequired, "code": "CONFIRMATION_DECLINED", "message": message}})
		}
		outcome = a.runMCPCommand(ctx, cmd, args, opts, policy, append(extra, "--confirm=true"))
	}
	if outcome.exit != ExitOK {
		return mcpWithEnvironment(mcpCommandError(cmd, outcome, policy, ""), outcome.environment)
	}
	if refused := mcpResultTooLarge(cmd, args, outcome.value); refused != nil {
		return mcpWithWarnings(mcpWithEnvironment(refused, outcome.environment), outcome.warnings)
	}
	if mcpResultIsWrite(cmd, args) {
		if raw, err := mcpCompactJSON(outcome.value); err == nil && len(raw) > mcpMaxResultBytes {
			outcome.warnings += fmt.Sprintf("\nWarning: this result is %d bytes, over the 49,152-byte read limit; it is returned in full because the write completed.", len(raw))
		}
	}
	return mcpWithWarnings(mcpToolResult(outcome.value, structured, outcome.transformed, outcome.environment), outcome.warnings)
}

type mcpEnvironmentObserver struct {
	base        http.RoundTripper
	allowLive   bool
	cancel      context.CancelFunc
	mu          sync.Mutex
	environment string
	blocked     bool
}

type mcpReplayBody struct {
	io.Reader
	io.Closer
}

func (o *mcpEnvironmentObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	authContext := req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/v1/auth-context")
	o.mu.Lock()
	if o.environment == "live" && !o.allowLive && !authContext && req.Header.Get("Authorization") != "" {
		o.blocked = true
		o.mu.Unlock()
		if req.Body != nil {
			_ = req.Body.Close()
		}
		if o.cancel != nil {
			o.cancel()
		}
		return nil, errMCPLiveBlocked
	}
	o.mu.Unlock()
	resp, err := o.base.RoundTrip(req)
	if err != nil || !authContext || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		_ = resp.Body.Close()
		return nil, readErr
	}
	resp.Body = mcpReplayBody{Reader: io.MultiReader(bytes.NewReader(raw), resp.Body), Closer: resp.Body}
	var envelope struct {
		Data struct {
			Environment string `json:"environment"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Data.Environment != "" {
		o.mu.Lock()
		o.environment = normalizeEnvironment(envelope.Data.Environment)
		o.mu.Unlock()
	}
	return resp, nil
}

func (o *mcpEnvironmentObserver) state() (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.environment, o.blocked
}

func (a *App) mcpObservedHTTPClient(observer *mcpEnvironmentObserver) *http.Client {
	var client http.Client
	if a.HTTPClient != nil {
		client = *a.HTTPClient
	} else {
		// Keep the shared pool. A nil redirect policy lets each caller apply
		// its own: API requests reject redirects, help searches follow them.
		client = *defaultAPIHTTPClient
		client.CheckRedirect = nil
	}
	observer.base = client.Transport
	if observer.base == nil {
		observer.base = http.DefaultTransport
	}
	client.Transport = observer
	return &client
}

type mcpProgressKey struct{}

// mcpProgress sends notifications/progress for a tools/call whose request
// carried a progressToken.
type mcpProgress struct {
	session  *mcpSession
	token    any
	messages bool // Progress messages arrived in protocol 2025-03-26.
	mu       sync.Mutex
	count    int
}

func mcpProgressFrom(ctx context.Context) *mcpProgress {
	progress, _ := ctx.Value(mcpProgressKey{}).(*mcpProgress)
	return progress
}

func (p *mcpProgress) notify(message string) {
	if p == nil {
		return
	}
	// Holding the lock while writing keeps progress values in order.
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	params := map[string]any{"progressToken": p.token, "progress": p.count}
	if p.messages && message != "" {
		params["message"] = message
	}
	_ = p.session.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": params})
}

// mcpProgressMessage describes one of the CLI's --progress=json records.
func mcpProgressMessage(record map[string]any) (string, bool) {
	switch record["type"] {
	case "pagination":
		if resources, ok := record["resources"]; ok {
			return fmt.Sprintf("Fetched page %v (%v resources)", record["pages"], resources), true
		}
		return fmt.Sprintf("Fetched page %v", record["pages"]), true
	case "wait":
		return "Waiting for " + strings.Join(mcpStrings(record["conditions"]), ", "), true
	case "upgrade":
		return fmt.Sprint(record["stage"]), true
	}
	return "", false
}

// mcpStderr captures a child command's stderr line by line. With progress,
// the CLI's JSON progress records become notifications; debug lines go to the
// server's own stderr; everything else is kept as warnings for the agent.
type mcpStderr struct {
	mu       sync.Mutex
	kept     mcpCappedBuffer
	partial  []byte
	progress *mcpProgress
	debug    io.Writer
}

func (w *mcpStderr) line(line []byte) {
	if w.progress != nil && bytes.HasPrefix(line, []byte("{")) {
		var record map[string]any
		if json.Unmarshal(line, &record) == nil {
			if message, ok := mcpProgressMessage(record); ok {
				w.progress.notify(message)
				return
			}
		}
	}
	if bytes.HasPrefix(line, []byte("debug: ")) {
		if w.debug != nil {
			_, _ = w.debug.Write(append(append([]byte(nil), line...), '\n'))
		}
		return
	}
	_, _ = w.kept.Write(append(append([]byte(nil), line...), '\n'))
}

func (w *mcpStderr) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) > 0 {
		w.line(w.partial)
		w.partial = nil
	}
	return strings.TrimSpace(w.kept.String())
}

// mcpLineNotifier reports each stream record a command writes as progress.
type mcpLineNotifier struct {
	io.Writer
	progress *mcpProgress
	records  int
}

func (a *App) callMCPCommand(ctx context.Context, cmd *Command, args map[string]any, parent Options) (any, bool, int) {
	outcome := a.runMCPCommand(ctx, cmd, args, parent, mcpPolicyFromOptions(parent), nil)
	return outcome.value, outcome.transformed, outcome.exit
}

func (a *App) runMCPCommand(ctx context.Context, cmd *Command, args map[string]any, parent Options, policy mcpPolicy, extra []string) mcpCallResult {
	if cmd.CanonicalName == "api" {
		method, path := strings.ToUpper(fmt.Sprint(args["method"])), fmt.Sprint(args["path"])
		if _, ok := matchPublicOperation(method, path); !ok {
			e := usageError("UNDOCUMENTED_API_OPERATION", "Unknown raw API routes are available only through terminal flint api.", "path")
			return mcpCallResult{value: mcpCLIErrorEnvelope(e), exit: e.ExitCode}
		}
	}
	if cmd.Stream {
		bound := mcpMaxStreamDuration
		if controls, ok := args["_flint"].(map[string]any); ok {
			if raw, exists := controls["for"]; exists {
				if requested, err := time.ParseDuration(fmt.Sprint(raw)); err == nil && requested > 0 {
					bound = requested
				}
			}
		}
		var cancel context.CancelFunc
		// Let the CLI finish its --for checkpoint and report unmet counts
		// before the outer MCP safety deadline cancels it.
		ctx, cancel = context.WithTimeout(ctx, bound+time.Second)
		defer cancel()
	}
	argv := append([]string(nil), cmd.Path[1:]...)
	argv = appendInheritedMCPOptions(argv, parent)
	transformOptions, transformed, transformErr := mcpOutputTransformOptions(cmd, args)
	if transformErr != nil {
		return mcpCallResult{value: mcpCLIErrorEnvelope(transformErr), exit: transformErr.ExitCode}
	}
	transformOptions.outputLimit = mcpMaxOutputBytes
	controls, _ := args["_flint"].(map[string]any)
	if controls["dry_run"] == "client" {
		transformOptions.DryRun = "client"
	}
	defaultPageSize := cmd.Supports.Pagination && !mcpResultIsWrite(cmd, args) && args["page_size"] == nil && controls["page_size"] == nil
	if defaultPageSize {
		argv = append(argv, "--page-size=10")
	}
	body := map[string]any{}
	positionals := []string{}
	if mcpLocalOperation(cmd) {
		for _, arg := range cmd.Arguments {
			value, ok := args[arg.Name]
			if !ok {
				continue
			}
			if arg.Positional > 0 {
				for len(positionals) < arg.Positional {
					positionals = append(positionals, "")
				}
				positionals[arg.Positional-1] = fmt.Sprint(value)
			} else if arg.Flag != "" {
				argv = appendMCPFlag(argv, arg.Flag, value)
			}
		}
		if cmd.Stream || cmd.CanonicalName == "trigger" {
			if seconds, ok := args["max_seconds"]; ok {
				argv = append(argv, "--for", fmt.Sprint(seconds)+"s")
			} else if cmd.CanonicalName == "trigger" {
				argv = append(argv, "--for", "60s")
			}
			if count, ok := args["max_events"]; ok {
				argv = appendMCPFlag(argv, "--max-events", count)
			}
		}
	} else {
		locations := map[string]string{}
		operation, _ := openAPIOperationByID(cmd.OperationID)
		for _, parameter := range operation.Parameters {
			location, _ := parameter["in"].(string)
			name, _ := parameter["name"].(string)
			if location != "path" && location != "query" {
				continue
			}
			locations[name] = location
			value, ok := args[name]
			if !ok {
				continue
			}
			if location == "path" {
				for _, arg := range cmd.Arguments {
					if arg.Name == name && arg.Positional > 0 {
						for len(positionals) < arg.Positional {
							positionals = append(positionals, "")
						}
						positionals[arg.Positional-1] = fmt.Sprint(value)
					}
				}
			} else {
				flag := "--" + strings.ReplaceAll(name, "_", "-")
				for _, arg := range cmd.Arguments {
					if arg.Query == name && arg.Flag != "" {
						flag = arg.Flag
						break
					}
				}
				argv = appendMCPFlag(argv, flag, value)
			}
		}
		for key, value := range args {
			if locations[key] == "" && key != "_flint" {
				body[key] = value
			}
		}
	}
	progress := mcpProgressFrom(ctx)
	if progress != nil && !cmd.Stream {
		// The CLI reports progress only for these; --progress elsewhere is a
		// usage error.
		controls, _ := args["_flint"].(map[string]any)
		all, _ := controls["all"].(bool)
		paginate, _ := controls["paginate"].(bool)
		if _, waits := controls["wait_for"]; all || paginate || waits || cmd.CanonicalName == "upgrade" {
			argv = append(argv, "--progress=json")
		}
	}
	if globals, ok := args["_flint"].(map[string]any); ok {
		for key, value := range globals {
			if key == "field" || key == "select" || key == "jq" {
				continue
			}
			flag := "--" + strings.ReplaceAll(key, "_", "-")
			argv = appendMCPFlag(argv, flag, value)
		}
	}
	if cmd.Stream {
		globals, _ := args["_flint"].(map[string]any)
		_, hasDuration := globals["for"]
		if _, exists := globals["max_events"]; !exists && args["max_events"] == nil && !hasDuration {
			argv = append(argv, "--max-events", fmt.Sprint(mcpMaxStreamEvents))
		}
		if _, exists := globals["for"]; !exists && args["max_seconds"] == nil {
			argv = append(argv, "--for", mcpMaxStreamDuration.String())
		}
	}
	argv = append(argv, extra...)
	var stdin io.Reader = strings.NewReader("")
	if len(body) > 0 {
		raw, err := json.Marshal(body)
		if err != nil {
			return mcpCallResult{value: map[string]any{"error": "Could not encode MCP tool input: " + err.Error()}, exit: ExitUsage}
		}
		stdin = bytes.NewReader(raw)
		argv = append(argv, "--input", "-")
	}
	argv = append(argv, "--output", "json", "--no-input")
	// Positionals must follow all options and the terminator so data such as
	// a help-search query beginning with -- is never interpreted as a flag.
	if len(positionals) > 0 {
		argv = append(argv, "--")
		for _, value := range positionals {
			if value != "" {
				argv = append(argv, value)
			}
		}
	}
	childCtx, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	if mcpResultIsWrite(cmd, args) {
		childCtx = context.WithValue(childCtx, mcpFullWriteResponseKey{}, true)
	}
	if !mcpResultIsWrite(cmd, args) {
		childCtx = context.WithValue(childCtx, paginationResultCheckKey{}, paginationResultCheck(func(value any, pages int) *CLIError {
			projected, _, e := mcpProjectResult(childCtx, cmd, value, transformOptions)
			if e != nil {
				return e
			}
			raw, err := mcpCompactJSON(projected)
			if err == nil && len(raw) > mcpMaxResultBytes {
				return mcpResultSizeError(len(raw), pages)
			}
			return nil
		}))
	}
	observer := &mcpEnvironmentObserver{allowLive: policy.allowLive, cancel: cancelChild}
	if a.mcpAuth != nil {
		if auth, known := a.mcpAuth.snapshot(); known {
			observer.environment = normalizeEnvironment(auth.Environment)
		}
	}
	// Apply the read cap to the projected result, not the raw command output.
	// A completed write must always retain its full response.
	stdout := &mcpCappedBuffer{}
	stderr := &mcpStderr{kept: mcpCappedBuffer{limit: mcpMaxStderrBytes}, progress: progress, debug: a.Stderr}
	child := *a
	child.mcpSkipScopeCheck = true
	child.Stdout = stdout
	if progress != nil && cmd.Stream {
		child.Stdout = &mcpLineNotifier{Writer: stdout, progress: progress}
	}
	child.Stderr = stderr
	child.Stdin = stdin
	child.IsTTY = func() bool { return false }
	child.Context = childCtx
	child.HTTPClient = a.mcpObservedHTTPClient(observer)
	exit := child.Run(argv)
	environment, blocked := observer.state()
	outcome := mcpCallResult{exit: exit, environment: environment}
	if exit == ExitOK {
		outcome.warnings = stderr.String()
		if defaultPageSize {
			outcome.warnings = "Note: page_size is 10, the default here.\n" + outcome.warnings
		}
	}
	if !policy.allowLive && (blocked || exit != ExitOK) {
		var probe any
		_ = decodeJSONNumbers(bytes.TrimSpace(stdout.Bytes()), &probe)
		if blocked || mcpErrorField(probe, "code") == "LIVE_ACKNOWLEDGEMENT_REQUIRED" {
			outcome.liveBlocked = true
			outcome.environment = "live"
			outcome.value = map[string]any{"error": map[string]any{"type": errorTypeConfiguration, "code": "LIVE_ACCESS_DISABLED", "message": mcpLiveDisabledMessage}}
			outcome.exit = ExitAuth
			return outcome
		}
	}
	if stdout.exceeded {
		outcome.value = map[string]any{"error": fmt.Sprintf("Flint command output exceeded the %d byte MCP safety limit.", mcpMaxOutputBytes)}
		outcome.exit = ExitNetwork
		return outcome
	}
	trimmed := bytes.TrimSpace(stdout.Bytes())
	if len(trimmed) == 0 {
		outcome.value, outcome.warnings = map[string]any{"stderr": stderr.String()}, ""
		return outcome
	}
	var result any
	if err := decodeJSONNumbers(trimmed, &result); err == nil {
		if exit == ExitOK {
			if transformOptions.DryRunClient() {
				result = a.mcpDryRunResult(cmd, args, result)
			}
			projected, warning, transformErr := mcpProjectResult(childCtx, cmd, result, transformOptions)
			if transformErr != nil {
				outcome.value, outcome.exit = mcpCLIErrorEnvelope(transformErr), transformErr.ExitCode
				return outcome
			}
			if warning != "" {
				outcome.warnings += "\n" + warning
			}
			result = projected
		}
		outcome.value, outcome.transformed = result, transformed
		return outcome
	}
	var lines []any
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 64<<10), 20<<20)
	for scanner.Scan() {
		var line any
		if err := decodeJSONNumbers(scanner.Bytes(), &line); err != nil {
			outcome.value = map[string]any{"error": "Could not decode Flint NDJSON output: " + err.Error(), "stderr": strings.TrimSpace(stderr.String())}
			outcome.exit = ExitNetwork
			return outcome
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		outcome.value = map[string]any{"error": "Could not read Flint NDJSON output: " + err.Error(), "stderr": strings.TrimSpace(stderr.String())}
		outcome.exit = ExitNetwork
		return outcome
	}
	if len(lines) > 0 {
		outcome.value = map[string]any{"records": lines}
		return outcome
	}
	outcome.value = map[string]any{"stdout": string(trimmed), "stderr": strings.TrimSpace(stderr.String())}
	return outcome
}

const mcpLiveDisabledMessage = "The selected Flint credential or context is LIVE (production), and this MCP server was started without --allow-live, so it will not act on live data. Do not retry. Tell the user: to allow live access, restart the server with flint mcp serve --allow-live; otherwise switch to a sandbox credential (for example FLINT_API_KEY=flint_test_... or flint context switch to a sandbox context)."

func mcpOutputTransformOptions(cmd *Command, args map[string]any) (Options, bool, *CLIError) {
	opts := defaultOptions()
	controls, _ := args["_flint"].(map[string]any)
	transformed := false
	if raw, exists := controls["field"]; exists {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.field must be a non-empty string.", "field")
		}
		opts.Field = value
		transformed = true
	}
	if raw, exists := controls["jq"]; exists {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.jq must be a non-empty string.", "jq")
		}
		opts.JQ = value
		transformed = true
	}
	if raw, exists := controls["select"]; exists {
		values, ok := raw.([]any)
		if !ok || len(values) == 0 {
			return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.select must be a non-empty string array.", "select")
		}
		for _, rawValue := range values {
			value, ok := rawValue.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.select must be a non-empty string array.", "select")
			}
			opts.Select = append(opts.Select, value)
		}
		transformed = true
	}
	if !transformed {
		return opts, false, nil
	}
	if !cmd.Supports.Field || !cmd.Supports.Select || !cmd.Supports.JQ {
		return opts, false, usageError("UNSUPPORTED_FLAG", "Output transforms do not apply to this command.", "field")
	}
	if opts.Field != "" && opts.JQ != "" {
		return opts, false, usageError("CONFLICTING_FLAGS", "_flint.jq and _flint.field cannot be used together.", "field")
	}
	if paginate, _ := controls["paginate"].(bool); paginate {
		return opts, false, usageError("UNSUPPORTED_FLAG", "Output transforms do not apply to paginated NDJSON output.", "paginate")
	}
	return opts, true, nil
}

func appendMCPFlag(argv []string, flag string, value any) []string {
	switch values := value.(type) {
	case []any:
		for _, item := range values {
			argv = appendMCPFlag(argv, flag, item)
		}
	case bool:
		argv = append(argv, flag+"="+fmt.Sprint(values))
	default:
		argv = append(argv, flag+"="+fmt.Sprint(value))
	}
	return argv
}

func appendInheritedMCPOptions(argv []string, opts Options) []string {
	for _, item := range []struct {
		name  string
		value string
	}{
		{"profile", opts.Profile},
		{"credential-store", opts.CredentialStore},
		{"merchant", opts.Merchant},
		{"color", opts.Color},
	} {
		if len(opts.Raw[item.name]) > 0 && item.value != "" {
			argv = appendMCPFlag(argv, "--"+item.name, item.value)
		}
	}
	for _, item := range []struct {
		name  string
		value bool
	}{
		{"live", opts.Live},
		{"debug", opts.Debug},
		{"quiet", opts.Quiet},
	} {
		if len(opts.Raw[item.name]) > 0 {
			argv = append(argv, "--"+item.name+"="+fmt.Sprint(item.value))
		}
	}
	if len(opts.Raw["timeout"]) > 0 {
		argv = append(argv, "--timeout", opts.Timeout.String())
	}
	return argv
}

// mcpControlFlags maps CLI flags in error remediation to MCP tool controls.
const mcpEnvironmentMetaKey = "com.withflintpay/environment"
