package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

func triggerCommand() *Command {
	return &Command{Name: "trigger", Path: pathWithFlint("trigger"), Local: true, AuthRequired: true, Mutation: true, MCPHidden: true, Description: "Create sandbox resources that produce a webhook event; no automatic cleanup (reset the sandbox when done)", Arguments: []Arg{{Name: "event_type", Type: "string", Positional: 1, Description: "Primary event to produce"}, {Name: "list", Flag: "--list", Type: "boolean", Description: "List supported events offline"}}, Examples: ex("flint trigger --list", "flint trigger order.paid", "flint trigger dispute.won --for 2m", "flint trigger test_clock.ready --for 5m")}
}

func (a *App) localTrigger(cmd *Command, opts Options) int {
	entries := triggerEntries()
	if len(opts.Raw["live"]) > 0 {
		return a.fail(usageError("TEST_MODE_REQUIRED", "flint trigger runs only with a sandbox credential; --live is not supported.", "live"), opts)
	}
	if len(opts.Raw["input"]) > 0 {
		return a.fail(usageError("UNSUPPORTED_FLAG", "Trigger recipes do not accept --input.", "input"), opts)
	}
	if len(opts.Raw["list"]) > 0 && opts.Raw["list"][0] != "false" {
		if opts.Output == "human" {
			for _, entry := range entries {
				fmt.Fprintf(a.Stdout, "%-40s %-20s %2d calls  %s\n", entry.Event, entry.Recipe, entry.Calls, entry.WaitsOn)
			}
			return ExitOK
		}
		return a.outputLocal(map[string]any{"data": entries}, cmd, opts)
	}
	if len(opts.Positionals) == 0 {
		if opts.Output == "human" {
			for _, entry := range entries {
				fmt.Fprintf(a.Stdout, "%-40s %-20s %2d calls  %s\n", entry.Event, entry.Recipe, entry.Calls, entry.WaitsOn)
			}
		}
		e := usageError("EVENT_TYPE_REQUIRED", "Choose an event type, or run flint trigger --list.", "event_type")
		e.Details = map[string]any{"supported": entries}
		return a.fail(e, opts)
	}
	event := opts.Positionals[0]
	if e := validateListenEventType([]string{event}); e != nil {
		return a.fail(e, opts)
	}
	var entry triggerEntry
	supported := []string{}
	for _, candidate := range entries {
		supported = append(supported, candidate.Event)
		if candidate.Event == event {
			entry = candidate
		}
	}
	if entry.Event == "" {
		e := usageError("TRIGGER_EVENT_UNSUPPORTED", "No trigger recipe for "+event+". Produce it with the API calls that cause it (flint help events "+event+") and watch with flint listen --event-type "+event+". For a synthetic payload, register an endpoint and run flint webhook-endpoints test-events create.", "event_type")
		e.Details = map[string]any{"supported": supported}
		return a.fail(e, opts)
	}
	recipe := triggerRecipes()[entry.Recipe]
	if opts.IdempotencyKey != "" && !recipe.Resumable {
		return a.fail(usageError("TRIGGER_NOT_RESUMABLE", "This recipe can't resume; start a new run.", "idempotency-key"), opts)
	}
	for i := range recipe.Steps {
		step := &recipe.Steps[i]
		for path, value := range entry.Set {
			if rest, ok := strings.CutPrefix(path, step.Name+"."); ok {
				if err := setBodyPath(step.Body, rest, value); err != nil {
					return a.fail(usageError("INVALID_ARGUMENT", err.Error(), "event_type"), opts)
				}
			}
		}
		if entry.Through == step.Name {
			recipe.Steps = recipe.Steps[:i+1]
			break
		}
	}
	for _, step := range recipe.Steps {
		if _, ok := triggerOperation(step.OperationID); !ok {
			return a.fail(usageError("TRIGGER_EVENT_UNSUPPORTED", "Recipe requires an operation missing from this CLI: "+step.OperationID, "event_type"), opts)
		}
	}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return a.fail(configError("CONFIG_INVALID", err.Error(), err), opts)
	}
	ctx := withOAuthContext(a.commandContext(), resolved.ContextID)
	authOpts := opts
	authOpts.DryRun = ""
	authOpts.Preview = false
	// auth.status always introspects bearer credentials. Never infer sandbox mode from a key prefix.
	authCmd := *cmd
	authCmd.CanonicalName = "auth.status"
	key, baseURL, envelope, e := a.authenticateCommand(ctx, &authCmd, authOpts, resolved)
	if e != nil {
		return a.fail(e, opts)
	}
	auth := envelope.Data
	if normalizeEnvironment(auth.Environment) != "sandbox" {
		return a.fail(cliError(ExitAuth, errorTypeAuthorization, "TEST_MODE_REQUIRED", "flint trigger creates test payments and runs only with a sandbox credential. Use a flint_test_ key, or switch to a sandbox context with flint context switch."), opts)
	}
	if e := validateResolvedCredentialIntent(auth, opts, resolved, EnvironmentAffinityMerchant); e != nil {
		return a.fail(e, opts)
	}
	if auth.OAuthGrantID != "" && !environmentCredentialActive() {
		ctx = context.WithValue(ctx, oauthSessionKey{}, &oauthSession{Profile: resolved.ProfileName, GrantID: auth.OAuthGrantID, ContextID: auth.ContextID, SessionID: auth.OAuthSessionID, MerchantID: auth.MerchantID, SandboxID: auth.SandboxID, Environment: auth.Environment})
	}
	for _, step := range recipe.Steps {
		operation, _ := triggerOperation(step.OperationID)
		if step.Credential == "" {
			if e := commandPermissionError(operation, auth, resolved.ProfileName); e != nil {
				return a.fail(e, opts)
			}
		}
	}
	prefix := opts.IdempotencyKey
	if prefix == "" {
		generated, err := newIdempotencyKey()
		if err != nil {
			return a.fail(networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate a trigger run ID.", err), opts)
		}
		prefix = "flint-trigger-" + strings.TrimPrefix(generated, "flint-cli-")
	}
	// Pin the resolved selection, including contexts selected by the saved session.
	// Environment credentials cannot take --context; their merchant guard still applies.
	opts.Profile = resolved.ProfileName
	opts.Merchant = auth.MerchantID
	if auth.OAuthGrantID != "" && !environmentCredentialActive() {
		opts.ContextID = auth.ContextID
	}
	value, e := a.runTrigger(ctx, recipe, entry, opts, auth, key, baseURL, prefix)
	if e != nil {
		if opts.Output == "human" {
			if recipe.Resumable {
				fmt.Fprintln(a.Stderr, "Resume: "+triggerResumeCommand(entry.Event, prefix, opts))
			} else {
				fmt.Fprintln(a.Stderr, "This recipe can't resume; start a new run.")
			}
		}
		return a.fail(e, opts)
	}
	if opts.Output == "human" {
		data := value["data"].(map[string]any)
		if opts.DryRunClient() {
			return a.outputLocal(value, cmd, Options{Output: "json"})
		}
		fmt.Fprintf(a.Stdout, "Triggered %s in sandbox %s (run %s)\n", event, terminalSafe(auth.SandboxID), terminalSafe(prefix))
		for _, record := range data["steps"].([]map[string]any) {
			fmt.Fprintf(a.Stdout, "  %s  %s  %s %s  %v  %v\n", record["step"], record["operation_id"], record["method"], record["path"], record["status_code"], record["request_id"])
		}
		names := make([]string, 0, len(data["ids"].(map[string]any)))
		for name := range data["ids"].(map[string]any) {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(a.Stdout, "  %s: %s\n", name, terminalSafe(fmt.Sprint(data["ids"].(map[string]any)[name])))
		}
		fmt.Fprintf(a.Stdout, "Expected event: %s\n", event)
		if also := data["also_arrive"].([]string); len(also) > 0 {
			fmt.Fprintf(a.Stdout, "Also arrive: %s\n", strings.Join(also, ", "))
		}
		fmt.Fprintf(a.Stdout, "Each event's request.idempotency_key starts with %s.\nWatch them: flint listen --event-type %s\n", terminalSafe(prefix), event)
		return ExitOK
	}
	if opts.Output == "ndjson" {
		return a.outputTriggerRecord(map[string]any{"type": "result", "data": value["data"]}, opts)
	}
	return a.outputLocal(value, cmd, opts)
}

func triggerResumeCommand(event, prefix string, opts Options) string {
	command := "flint trigger " + event + " --idempotency-key " + quoteRemediationArgument(prefix)
	for _, selection := range []struct{ flag, value string }{
		{"profile", opts.Profile}, {"context", opts.ContextID},
		{"merchant", opts.Merchant}, {"credential-store", opts.CredentialStore},
	} {
		if selection.value != "" {
			command += " --" + selection.flag + " " + quoteRemediationArgument(selection.value)
		}
	}
	if opts.For > 0 {
		command += " --for " + opts.For.String()
	}
	return command
}

func triggerOperation(id string) (*Command, bool) {
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, false
	}
	for path, methods := range doc.Paths {
		for method, operation := range methods {
			if operation.OperationID == id {
				return &Command{OperationID: id, Method: strings.ToUpper(method), APIPath: path, InputSchema: requestSchemaName(operation.RequestBody), OutputSchema: responseSchemaName(operation.Responses)}, true
			}
		}
	}
	return nil, false
}

// The plan interval and margin are run inputs; the source is a replayed keyed create.
type triggerClockAdvance struct {
	From          string `json:"from"`
	Months        int    `json:"months"`
	MarginSeconds int    `json:"margin_seconds"`
}

// Resolve only whole-value references, retaining objects such as outstanding_money.
func triggerResolve(value any, results map[string]any) (any, *CLIError) {
	switch v := value.(type) {
	case triggerClockAdvance:
		raw, e := triggerResolve(v.From, results)
		if e != nil {
			return nil, e
		}
		frozen, err := time.Parse(time.RFC3339, fmt.Sprint(raw))
		if err != nil {
			return nil, invalidResponseError("INVALID_RESPONSE", "Invalid test clock frozen_at.", err)
		}
		start := time.Date(frozen.Year(), frozen.Month()+time.Month(v.Months), 1, frozen.Hour(), frozen.Minute(), frozen.Second(), 0, frozen.Location())
		lastDay := start.AddDate(0, 1, -1).Day()
		return start.AddDate(0, 0, min(frozen.Day(), lastDay)-1).Add(time.Duration(v.MarginSeconds) * time.Second).UTC().Format(time.RFC3339), nil
	case string:
		if strings.HasPrefix(v, "{") && strings.HasSuffix(v, "}") {
			step, path, ok := strings.Cut(v[1:len(v)-1], ".")
			if ok {
				if found, exists := lookupResourcePath(results[step], path); exists {
					return found, nil
				}
				if found, exists := lookupResourcePath(results[step], "data.0."+path); exists {
					return found, nil
				}
			}
			return nil, invalidResponseError("INVALID_RESPONSE", "Trigger response is missing reference "+v, nil)
		}
	case map[string]any:
		out := map[string]any{}
		for k, item := range v {
			resolved, e := triggerResolve(item, results)
			if e != nil {
				return nil, e
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			resolved, e := triggerResolve(item, results)
			if e != nil {
				return nil, e
			}
			out[i] = resolved
		}
		return out, nil
	}
	return value, nil
}

func (a *App) runTrigger(ctx context.Context, recipe triggerRecipe, entry triggerEntry, opts Options, auth AuthContext, key, baseURL, prefix string) (map[string]any, *CLIError) {
	records := []map[string]any{}
	results := map[string]any{}
	ids := map[string]any{}
	plan := []map[string]any{}
	also := []string{}
	for _, event := range entry.Also {
		if event != entry.Event {
			also = append(also, event)
		}
	}
	data := map[string]any{"event_type": entry.Event, "recipe": recipe.Name, "resumable": recipe.Resumable, "sandbox_id": auth.SandboxID, "idempotency_key_prefix": prefix, "expected_events": []string{entry.Event}, "also_arrive": also, "ids": ids}
	fail := func(e *CLIError, step string) (map[string]any, *CLIError) {
		trigger := map[string]any{"event_type": entry.Event, "completed_steps": records, "failed_step": step, "resumable": recipe.Resumable}
		if recipe.Resumable {
			trigger["resume"] = triggerResumeCommand(entry.Event, prefix, opts)
		} else {
			trigger["recovery"] = "This recipe can't resume; start a new run."
		}
		addDetails := func(original any) map[string]any {
			details := map[string]any{}
			if existing, ok := original.(map[string]any); ok {
				details = copyMap(existing)
			} else if original != nil {
				details["problems"] = original
			}
			details["trigger"] = trigger
			return details
		}
		if raw, ok := e.Details.(map[string]any); ok && raw["code"] != nil {
			raw = copyMap(raw)
			raw["details"] = addDetails(raw["details"])
			e.Details = raw
		} else {
			e.Details = addDetails(e.Details)
		}
		return nil, e
	}
	waitFor := opts.For
	if waitFor == 0 {
		waitFor = 2 * time.Minute
		if recipe.Async == "temporal" {
			waitFor = 5 * time.Minute
		}
	}
	for _, step := range recipe.Steps {
		operation, _ := triggerOperation(step.OperationID)
		path := operation.APIPath
		for name, ref := range step.Path {
			value, e := triggerResolve(ref, results)
			if opts.DryRunClient() {
				value = ref
				e = nil
			}
			if e != nil {
				return fail(e, step.Name)
			}
			replacement := fmt.Sprint(value)
			if !opts.DryRunClient() {
				replacement = url.PathEscape(replacement)
			}
			path = strings.ReplaceAll(path, "{"+name+"}", replacement)
		}
		query := url.Values{}
		for name, ref := range step.Query {
			value, e := triggerResolve(ref, results)
			if opts.DryRunClient() {
				value = ref
				e = nil
			}
			if e != nil {
				return fail(e, step.Name)
			}
			query.Set(name, fmt.Sprint(value))
		}
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
		stepKey := ""
		if operation.Method != "GET" {
			stepKey = prefix + "-" + step.Name
		}
		record := map[string]any{"step": step.Name, "operation_id": step.OperationID, "method": operation.Method, "path": path, "idempotency_key": stepKey}
		if opts.DryRunClient() {
			record["body"] = step.Body
			record["until"] = step.Until
			plan = append(plan, record)
			continue
		}
		bodyValue, e := triggerResolve(step.Body, results)
		if e != nil {
			return fail(e, step.Name)
		}
		var body []byte
		if step.Body != nil {
			body, _ = json.Marshal(bodyValue)
		}
		stepCtx, stepCredential := ctx, key
		if step.Credential != "" {
			credential, e := triggerResolve(step.Credential, results)
			if e != nil {
				return fail(e, step.Name)
			}
			stepCredential, _ = credential.(string)
			if stepCredential == "" {
				return fail(invalidResponseError("INVALID_RESPONSE", "Missing checkout credential.", nil), step.Name)
			}
			stepCtx = withoutOAuthSession(ctx)
		}
		conditions, _ := parseWaitConditions(step.Until)
		waitCtx := stepCtx
		if len(conditions) > 0 {
			var cancel context.CancelFunc
			waitCtx, cancel = context.WithTimeout(stepCtx, waitFor)
			defer cancel()
		}
		waitError := func() *CLIError {
			e := cliError(ExitWait, errorTypeWaitConditionUnmet, "WAIT_CONDITION_UNMET", "Trigger wait did not reach its condition.")
			if recipe.Async == "stripe_webhook" {
				e.Message += " This waits for Stripe's webhook to reach Flint. A local server without Stripe webhook forwarding never receives it; use staging or --for longer."
			}
			return e
		}
		deadline := a.Now().Add(waitFor)
		backoff := time.Second
		var response *apiResponse
		for {
			if len(conditions) > 0 && (waitCtx.Err() != nil || !a.Now().Before(deadline)) {
				if ctx.Err() != nil {
					return fail(networkError("CLIENT_CANCELED", "Trigger was canceled.", ctx.Err()), step.Name)
				}
				return fail(waitError(), step.Name)
			}
			requestCtx := context.WithValue(waitCtx, responseContractKey{}, operation)
			requestTimeout := opts.Timeout
			if len(conditions) > 0 {
				requestTimeout = min(requestTimeout, max(time.Millisecond, deadline.Sub(a.Now())))
			}
			response, e = a.doRequestWithin(requestCtx, requestTimeout, baseURL, stepCredential, operation.Method, path, body, stepKey, opts.Debug)
			if e != nil {
				if len(conditions) > 0 && ctx.Err() == nil && (waitCtx.Err() != nil || !a.Now().Before(deadline)) {
					return fail(waitError(), step.Name)
				}
				return fail(e, step.Name)
			}
			if len(conditions) == 0 {
				break
			}
			matched, observed := conditionsMatch(response.Value, conditions)
			if len(conditions) == 1 && slices.Contains(step.Accepted, observed[conditions[0].Path]) {
				matched = true
			}
			if matched {
				break
			}
			progress := map[string]any{"type": "wait", "step": step.Name, "observed": observed}
			if opts.Output == "ndjson" {
				if code := a.outputTriggerRecord(progress, opts); code != ExitOK {
					return fail(networkError("OUTPUT_WRITE_FAILED", "Could not write trigger progress.", nil), step.Name)
				}
			} else if e := a.writeProgress(opts, progress, "Waiting for "+step.Name); e != nil {
				return fail(e, step.Name)
			}
			status, _ := lookupResourcePath(response.Value, "status")
			remaining := deadline.Sub(a.Now())
			if remaining <= 0 || (step.OperationID == "getTestClock" && status == "failed") {
				return fail(waitError(), step.Name)
			}
			if !sleepContext(waitCtx, min(backoff, remaining)) {
				if ctx.Err() == nil {
					return fail(waitError(), step.Name)
				}
				return fail(networkError("CLIENT_CANCELED", "Trigger was canceled.", ctx.Err()), step.Name)
			}
			backoff = min(backoff*2, 10*time.Second)
		}
		results[step.Name] = response.Value
		// Missing declared outputs fail closed; secrets remain only in the in-memory response.
		for name, path := range step.Outputs {
			value, ok := lookupResourcePath(response.Value, path)
			if !ok {
				return fail(invalidResponseError("INVALID_RESPONSE", "Trigger response is missing "+path, nil), step.Name)
			}
			ids[name] = value
		}
		for _, path := range step.Secrets {
			if value, ok := lookupResourcePath(response.Value, path); !ok || value == "" {
				return fail(invalidResponseError("INVALID_RESPONSE", "Trigger response is missing a checkout credential.", nil), step.Name)
			}
		}
		for _, name := range []string{"order_id", "customer_id", "refund_id", "checkout_session_id", "dispute_id", "payment_intent_id", "payment_method_id", "test_clock_id", "subscription_plan_id", "subscription_id", "invoice_id", "invoice_manual_payment_id"} {
			if path, ok := step.Outputs[name]; ok {
				value, _ := lookupResourcePath(response.Value, path)
				record["resource_type"] = strings.TrimSuffix(name, "_id")
				record["resource_id"] = value
				break
			}
		}
		record["status_code"] = response.Status
		record["request_id"], _ = lookupResourcePath(response.Value, "request_id")
		records = append(records, record)
		if opts.Output == "ndjson" {
			if code := a.outputTriggerRecord(map[string]any{"type": "step", "data": record}, opts); code != ExitOK {
				return fail(networkError("OUTPUT_WRITE_FAILED", "Could not write trigger step.", nil), step.Name)
			}
		}
	}
	data["steps"] = records
	if opts.DryRunClient() {
		data["plan"] = plan
	}
	return map[string]any{"data": data}, nil
}

func (a *App) outputTriggerRecord(record map[string]any, opts Options) int {
	if err := writeJSON(a.Stdout, record); err != nil {
		return a.fail(networkError("OUTPUT_WRITE_FAILED", "Could not write trigger output.", err), opts)
	}
	return ExitOK
}
