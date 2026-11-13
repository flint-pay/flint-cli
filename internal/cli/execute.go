package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

type outputHandled struct{}

// Embedded callers may bound a projected collection before another page is read.
// Terminal pagination has no check and keeps its existing behavior.
type paginationResultCheckKey struct{}
type paginationResultCheck func(any, int) *CLIError

func (a *App) executeAPI(ctx context.Context, cmd *Command, opts Options, resolved ResolvedConfig, authContext AuthContext, key, baseURL string) (any, *CLIError) {
	ctx = context.WithValue(ctx, responseContractKey{}, cmd)
	req, e := a.prepareRequest(ctx, cmd, opts, resolved, key, baseURL)
	if e != nil {
		return nil, e
	}
	if a.mcpGate != nil {
		if e := a.mcpGate.prepared(ctx, a, cmd, req, resolved); e != nil {
			return nil, e
		}
		// The MCP gate authorized this exact prepared request. Legacy terminal
		// confirmation must not add a second approval or change the request.
		opts.Confirm = true
	}
	if opts.DryRun == "client" || opts.Preview {
		if authContext.AuthType == "none" {
			req.Warnings = append(req.Warnings, map[string]any{
				"message":        unauthenticatedDryRunWarning,
				"skipped_checks": []string{"scopes", "merchant_guard", "sandbox_guard"},
			})
		}
		return previewRequest(req, cmd, authContext, opts.Preview), nil
	}
	if cmd.CanonicalName == "listen" {
		return a.executeListen(ctx, cmd, opts, req, key, baseURL)
	}
	if cmd.Follow && lastRawOption(opts, "follow") == "true" {
		return a.executeFollow(ctx, opts, req, key, baseURL)
	}
	if cmd.CanonicalName == "api" && opts.Paginate {
		return a.executeRawPagination(ctx, opts, req, key, baseURL)
	}
	if len(opts.WaitFor) > 0 {
		return a.executeWait(ctx, cmd, opts, req, key, baseURL)
	}
	if opts.All || opts.Limit > 0 || opts.Output == "ndjson" {
		return a.executeAllPages(ctx, cmd, opts, req, key, baseURL)
	}
	if req.ArrayEditNoop {
		return req.ArrayEditValue, nil
	}
	resp, e := a.doRequestWithin(ctx, opts.Timeout, baseURL, key, req.Method, req.Path, req.Body, req.IdempotencyKey, opts.Debug)
	if e != nil && e.Code == "VERSION_CONFLICT" && hasArrayEdits(cmd, opts) {
		retryOpts := opts
		if opts.Input != "" {
			input, _ := json.Marshal(req.OriginalInput)
			retryOpts.Input = string(input)
		}
		retryOpts.IdempotencyKey = "" // The new version changes the body, so it needs a fresh replay key.
		req, e = a.prepareRequest(ctx, cmd, retryOpts, resolved, key, baseURL)
		if e != nil {
			return nil, e
		}
		if req.ArrayEditNoop {
			return req.ArrayEditValue, nil
		}
		resp, e = a.doRequestWithin(ctx, opts.Timeout, baseURL, key, req.Method, req.Path, req.Body, req.IdempotencyKey, opts.Debug)
	}

	if e != nil {
		return nil, e
	}
	if opts.Open && opts.Output == "human" && !opts.NoInput && a.IsTTY() {
		if target, ok := firstStringAt(resp.Value, "data.url", "data.checkout_session.url", "data.checkout_session.checkout_url"); ok {
			opener := a.OpenBrowser
			if opener == nil {
				opener = openBrowser
			}
			if err := opener(target); err != nil && !opts.Quiet {
				fmt.Fprintln(a.Stderr, "warning: could not open browser: "+err.Error())
			}
		}
	}
	if values := opts.Raw["save-to"]; len(values) > 0 {
		return a.saveDownload(resp.Value, values[len(values)-1])
	}
	return resp.Value, nil
}

// routeNotFound reports a 404 for the route itself rather than for a resource
// the route names, such as a sandbox that does not exist.
func previewRequest(req preparedRequest, cmd *Command, authContext AuthContext, preview bool) map[string]any {
	headers := map[string]any{}
	if req.IdempotencyKey != "" {
		headers["Idempotency-Key"] = req.IdempotencyKey
	}
	data := map[string]any{"method": req.Method, "path": req.Path, "headers": headers, "body": req.BodyValue, "persistent_side_effects": false}
	if authContext.AuthType == "none" {
		data["environment"] = authContext.Environment
	}
	if len(req.OverriddenFields) > 0 {
		data["overridden_fields"] = req.OverriddenFields
	}
	if len(req.Warnings) > 0 {
		data["warnings"] = req.Warnings
	}
	if preview {
		data["preview"] = map[string]any{
			"permission_check": permissionPreview(cmd, authContext),
			"reversibility": map[string]any{
				"reversible": false,
				"reason":     "Flint has no automatic rollback operation for this command.",
			},
			"affected_resources": []map[string]any{{"method": req.Method, "path": req.Path}},
		}
	}
	return map[string]any{"data": data}
}

func permissionPreview(cmd *Command, authContext AuthContext) map[string]any {
	if !credentialScopesKnown(authContext) {
		return map[string]any{"status": "unknown", "reason": "This credential's scopes cannot be checked locally."}
	}
	if cmd == nil || cmd.OperationID == "" {
		return map[string]any{"status": "unknown", "reason": "The raw API command has no fixed operation contract."}
	}
	if _, ok := openAPIOperationByID(cmd.OperationID); !ok {
		return map[string]any{"status": "unknown", "reason": "The operation is missing from the embedded public API contract."}
	}
	requirement := commandScopeRequirement(cmd)
	if requirement == nil {
		return map[string]any{"status": "pass", "missing_scopes": []string{}}
	}
	index, err := scopeRequirementIndex()
	if err != nil {
		return map[string]any{"status": "unknown", "reason": "The embedded scope metadata could not be read."}
	}
	missing := index.MissingScopes(opindex.Entry{ScopeRequirement: requirement}, authContext.Scopes, true)
	status := "pass"
	if len(missing) > 0 {
		status = "fail"
	}
	return map[string]any{"status": status, "scope_requirement": requirement, "missing_scopes": missing}
}

func commandPermissionError(cmd *Command, authContext AuthContext, profile string) *CLIError {
	check := permissionPreview(cmd, authContext)
	if check["status"] != "fail" {
		return nil
	}
	required := check["scope_requirement"].(*opindex.ScopeRequirement)
	missing := check["missing_scopes"].([]string)
	eligible, liveOnly := missingScopeGuidance(missing, authContext.Environment)
	liveRequired := len(liveOnly) > 0 && (required.Mode != "any" || len(eligible) == 0)
	requirementText := "requires these scopes: "
	if required.Mode == "any" {
		requirementText = "requires at least one of these scopes: "
	}
	message := "The active credential " + requirementText + strings.Join(missing, ", ") + "."
	if liveRequired {
		message += " The missing scopes require live mode: " + strings.Join(liveOnly, ", ") + "."
	}
	actions := scopeRemediation(authContext, profile, required, eligible, liveOnly, liveRequired)
	detail := map[string]any{
		"code": "INSUFFICIENT_SCOPE", "message": message, "reason": "scope_not_granted",
		"scope_requirement": required, "missing_scopes": missing,
	}
	if liveRequired {
		detail["allowed_key_modes"] = []string{"live"}
	}
	err := cliError(ExitAuth, errorTypeAuthorization, "INSUFFICIENT_SCOPE", message)
	err.Details = map[string]any{
		"type": err.Type, "code": err.Code, "message": message,
		"details": []any{detail}, "remediation": map[string]any{"next_actions": actions},
	}
	return err
}

func scopeRemediation(auth AuthContext, profile string, required *opindex.ScopeRequirement, eligible, liveOnly []string, liveRequired bool) []any {
	if auth.AuthType == "workload_identity" || auth.WorkloadIdentityID != "" {
		message := "Grant this workload identity the required scopes: " + strings.Join(required.Scopes, ", ") + "."
		if required.Mode == "any" {
			message = "Grant this workload identity at least one required scope: " + strings.Join(eligible, ", ") + "."
		}
		if liveRequired {
			message = "Use a live workload identity grant with the required scopes: " + strings.Join(required.Scopes, ", ") + "."
		}
		return []any{map[string]any{"reason_message": message}, map[string]any{"reason_message": "If FLINT_OIDC_SCOPE narrows this token, include the required scopes or unset it to use the grant's scopes, then obtain a new workload token. For an explicit FLINT_ACCESS_TOKEN, replace it with the new token."}}
	}
	var actions []any
	if override := activeScopeCredentialOverride(); override != "" {
		actions = append(actions, map[string]any{"reason_message": "Replace " + override + " with a credential that has the required scopes, or unset it before using a saved login or imported key. " + override + " overrides saved credentials."})
	}
	importCommand := scopeProfileCommand("flint auth import", profile)
	if liveRequired || normalizeEnvironment(auth.Environment) == "live" {
		importCommand += " --live"
	}
	if liveRequired {
		return append(actions, map[string]any{"reason_message": "Use a live key with the required scopes. These scopes cannot be added to a sandbox key or browser context."}, map[string]any{"command": importCommand, "url": scopeDashboardURL(liveOnly)})
	}
	if auth.isCLILogin() && auth.OAuthSessionID != "" && auth.ContextID != "" {
		command := scopeProfileCommand("flint reauth --context "+quoteRemediationArgument(auth.ContextID), profile)
		return append(actions, map[string]any{"reason_message": "Approve the additional scope access for this context."}, map[string]any{"command": appendScopeArguments(command, required.Mode, eligible)})
	}
	// A new session cannot inherit the old credential's scopes. Request the
	// complete all-mode requirement, including scopes the old credential held.
	loginScopes, _ := missingScopeGuidance(required.Scopes, auth.Environment)
	command := "flint login --new-session"
	if normalizeEnvironment(auth.Environment) == "live" {
		command += " --live"
	}
	command = appendScopeArguments(scopeProfileCommand(command, profile), required.Mode, loginScopes)
	if !auth.isCLILogin() {
		message := "Create an API key with all required scopes: " + strings.Join(required.Scopes, ", ") + "."
		if required.Mode == "any" {
			message = "Create an API key with at least one of these scopes: " + strings.Join(loginScopes, ", ") + "."
		}
		actions = append(actions, map[string]any{"reason_message": message}, map[string]any{"command": importCommand, "url": scopeDashboardURL(loginScopes)})
	}
	return append(actions, map[string]any{"command": command})
}

func scopeProfileCommand(command, profile string) string {
	if profile != "" {
		command += " --profile " + quoteRemediationArgument(profile)
	}
	return command
}

func appendScopeArguments(command, mode string, scopes []string) string {
	if mode == "any" && len(scopes) > 1 {
		scopes = scopes[:1]
	}
	for _, scope := range scopes {
		command += " --scope " + quoteRemediationArgument(scope)
	}
	return command
}

func scopeDashboardURL(scopes []string) string {
	if len(scopes) > 0 {
		return dashboardAPIKeysURL + "?create=true&scope=" + url.QueryEscape(scopes[0])
	}
	return dashboardAPIKeysURL
}

func activeScopeCredentialOverride() string {
	if strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN")) != "" {
		return "FLINT_ACCESS_TOKEN"
	}
	if credentialFromEnvironment() != "" {
		return "FLINT_API_KEY"
	}
	return ""
}

func quoteRemediationArgument(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./-", r))
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (a *App) executeAllPages(ctx context.Context, cmd *Command, opts Options, req preparedRequest, key, baseURL string) (any, *CLIError) {
	combined := make([]any, 0)
	count := 0
	stream := opts.Output == "ndjson"
	var last, collection map[string]any
	collectionField := "data"
	if cmd.OperationID == "getResourceTimeline" {
		collectionField = "entries"
	}
	path := req.Path
	pages := 0
	seenPageTokens := map[string]bool{}
	for {
		if opts.Limit > 0 {
			path = setQuery(path, "page_size", fmt.Sprint(min(opts.Limit-count, 100)))
		}
		if token := pageToken(path); token != "" {
			if seenPageTokens[token] {
				return nil, invalidResponseError("PAGINATION_TOKEN_LOOP", "Flint returned a repeated pagination token.", nil)
			}
			seenPageTokens[token] = true
		}
		resp, e := a.doRequestWithin(ctx, opts.Timeout, baseURL, key, req.Method, path, req.Body, req.IdempotencyKey, opts.Debug)
		if e != nil {
			return nil, e
		}
		m, ok := resp.Value.(map[string]any)
		if !ok {
			return nil, invalidResponseError("INVALID_PAGINATION_RESPONSE", "List response is not an object.", nil)
		}
		pages++
		collection = m
		if collectionField == "entries" {
			collection, _ = m["data"].(map[string]any)
		}
		if data, ok := collection[collectionField].([]any); ok {
			remaining := len(data)
			if opts.Limit > 0 {
				remaining = min(remaining, opts.Limit-count)
			}
			if stream {
				// Project each page independently, including timeline entries.
				value, e := applyCommandOutputTransforms(ctx, map[string]any{"data": data[:remaining]}, cmd, opts)
				if e != nil {
					return nil, e
				}
				for _, resource := range value.(map[string]any)["data"].([]any) {
					if err := writeJSON(a.Stdout, resource); err != nil {
						return nil, networkError("OUTPUT_WRITE_FAILED", "Could not write command output.", err)
					}
				}
			} else {
				combined = append(combined, data[:remaining]...)
			}
			count += remaining
			if remaining < len(data) {
				delete(m, "next_page_token")
			}
		} else {
			return nil, invalidResponseError("INVALID_PAGINATION_RESPONSE", "List response collection is not an array.", nil)
		}
		last = m
		token, tokenErr := nextPageToken(m)
		if tokenErr != nil {
			return nil, tokenErr
		}
		if check, ok := ctx.Value(paginationResultCheckKey{}).(paginationResultCheck); ok && !stream {
			candidate := copyMap(m)
			if collectionField == "data" {
				candidate[collectionField] = combined
			} else {
				candidateCollection := copyMap(collection)
				candidateCollection[collectionField] = combined
				candidate["data"] = candidateCollection
			}
			if opts.Limit == 0 {
				candidate["next_page_token"] = ""
			}
			if e := check(candidate, pages); e != nil {
				return nil, e
			}
		}
		if token == "" || (opts.Limit > 0 && count >= opts.Limit) || (stream && !opts.All && opts.Limit == 0) {
			break
		}
		if seenPageTokens[token] {
			return nil, invalidResponseError("PAGINATION_TOKEN_LOOP", "Flint returned a repeated pagination token.", nil)
		}
		path = setQuery(path, "page_token", token)
		if e := a.writeProgress(opts, map[string]any{"type": "pagination", "pages": pages, "resources": count}, fmt.Sprintf("Fetched page %d (%d resources)", pages, count)); e != nil {
			return nil, e
		}
	}
	if stream {
		return outputHandled{}, nil
	}
	collection[collectionField] = combined
	if opts.Limit == 0 {
		last["next_page_token"] = ""
	}
	return last, nil
}

func (a *App) executeWait(ctx context.Context, cmd *Command, opts Options, req preparedRequest, key, baseURL string) (any, *CLIError) {
	conditions, e := parseWaitConditions(opts.WaitFor)
	if e != nil {
		return nil, e
	}
	bound := 2 * time.Minute
	if opts.For > 0 {
		bound = opts.For
	}
	deadline := a.Now().Add(bound)
	delay := time.Second
	var last any
	for {
		remaining := deadline.Sub(a.Now())
		if remaining <= 0 {
			err := cliError(ExitWait, errorTypeWaitConditionUnmet, "WAIT_CONDITION_UNMET", "The wait bound passed before the requested condition was met.")
			err.Details = map[string]any{"conditions": opts.WaitFor, "observed": observedConditions(last, conditions), "last": last}
			return last, err
		}
		requestTimeout := min(opts.Timeout, remaining)
		resp, requestErr := a.doRequestWithin(ctx, requestTimeout, baseURL, key, req.Method, req.Path, nil, "", opts.Debug)
		if requestErr != nil {
			if !a.Now().Before(deadline) {
				err := cliError(ExitWait, errorTypeWaitConditionUnmet, "WAIT_CONDITION_UNMET", "The wait bound passed before the requested condition was met.")
				err.Details = map[string]any{"conditions": opts.WaitFor, "observed": observedConditions(last, conditions), "last": last}
				return last, err
			}
			return last, requestErr
		}
		last, requestErr = prepareOptionalFields(resp.Value, cmd, opts)
		if requestErr != nil {
			return last, requestErr
		}
		matched, observed := conditionsMatch(last, conditions)
		if matched {
			return last, nil
		}
		if terminallyImpossible(cmd, conditions, observed) {
			err := cliError(ExitWait, errorTypeWaitConditionUnmet, "WAIT_CONDITION_IMPOSSIBLE", "The resource reached a terminal state that cannot satisfy the requested condition.")
			err.Details = map[string]any{"conditions": opts.WaitFor, "observed": observed, "last": last}
			return last, err
		}
		if e := a.writeProgress(opts, map[string]any{"type": "wait", "conditions": opts.WaitFor, "observed": observed}, "Waiting for "+strings.Join(opts.WaitFor, ", ")); e != nil {
			return last, e
		}
		if !a.Now().Before(deadline) {
			err := cliError(ExitWait, errorTypeWaitConditionUnmet, "WAIT_CONDITION_UNMET", "The wait bound passed before the requested condition was met.")
			err.Details = map[string]any{"conditions": opts.WaitFor, "observed": observed, "last": last}
			return last, err
		}
		remaining = deadline.Sub(a.Now())
		if delay > remaining {
			delay = remaining
		}
		if !sleepContext(ctx, delay) {
			return last, networkError("CLIENT_TIMEOUT", "The request timed out.", ctx.Err())
		}
		delay = min(delay*2, 10*time.Second)
	}
}

func (a *App) writeProgress(opts Options, record map[string]any, plain string) *CLIError {
	mode := opts.Progress
	if opts.Quiet || mode == "quiet" || (mode == "auto" && opts.Output != "human") {
		return nil
	}
	if mode == "json" {
		if err := writeJSON(a.Stderr, record); err != nil {
			return networkError("OUTPUT_WRITE_FAILED", "Could not write progress output.", err)
		}
		return nil
	}
	fmt.Fprintln(a.Stderr, plain)
	return nil
}

type waitCondition struct{ Path, Expected string }

func parseWaitConditions(values []string) ([]waitCondition, *CLIError) {
	out := make([]waitCondition, 0, len(values))
	for _, v := range values {
		path, expected, ok := strings.Cut(v, "=")
		if !ok || strings.TrimSpace(path) == "" {
			return nil, usageError("INVALID_WAIT_CONDITION", "--wait-for must use field=value.", "wait-for")
		}
		out = append(out, waitCondition{strings.TrimSpace(path), expected})
	}
	return out, nil
}
func observedConditions(value any, conditions []waitCondition) map[string]string {
	observed := map[string]string{}
	for _, c := range conditions {
		if v, ok := lookupResourcePath(value, c.Path); ok {
			observed[c.Path] = fmt.Sprint(v)
			if v == nil {
				observed[c.Path] = "null"
			}
		}
	}
	return observed
}

func conditionsMatch(value any, conditions []waitCondition) (bool, map[string]string) {
	return matchConditions(value, conditions, lookupResourcePath)
}
func matchConditions(value any, conditions []waitCondition, lookup func(any, string) (any, bool)) (bool, map[string]string) {
	observed := map[string]string{}
	for _, c := range conditions {
		v, ok := lookup(value, c.Path)
		if !ok {
			return false, observed
		}
		text := fmt.Sprint(v)
		if v == nil {
			text = "null"
		}
		observed[c.Path] = text
		if text != c.Expected {
			return false, observed
		}
	}
	return true, observed
}
func lookupResourcePath(value any, path string) (any, bool) {
	return lookupPath(resourceRoot(value), path)
}

func terminallyImpossible(cmd *Command, conditions []waitCondition, observed map[string]string) bool {
	states := publicOperationOverlays[cmd.OperationID].TerminalStates
	if len(states) == 0 {
		return false
	}
	for _, c := range conditions {
		if c.Path == "status" {
			actual := observed[c.Path]
			return states[actual] && actual != c.Expected
		}
	}
	return false
}

func pageToken(path string) string {
	u, err := url.Parse(path)
	if err != nil {
		return ""
	}
	return u.Query().Get("page_token")
}

func nextPageToken(envelope map[string]any) (string, *CLIError) {
	raw, exists := envelope["next_page_token"]
	if !exists || raw == nil {
		return "", nil
	}
	token, ok := raw.(string)
	if !ok {
		return "", invalidResponseError("INVALID_PAGINATION_RESPONSE", "next_page_token is not a string.", nil)
	}
	return token, nil
}

func (a *App) executeRawPagination(ctx context.Context, opts Options, req preparedRequest, key, baseURL string) (any, *CLIError) {
	if req.Method != "GET" {
		return nil, usageError("PAGINATION_REQUIRES_GET", "--paginate requires flint api get.", "paginate")
	}
	path := req.Path
	seenPageTokens := map[string]bool{}
	pages := 0
	for {
		if token := pageToken(path); token != "" {
			if seenPageTokens[token] {
				return nil, invalidResponseError("PAGINATION_TOKEN_LOOP", "Flint returned a repeated pagination token.", nil)
			}
			seenPageTokens[token] = true
		}
		resp, e := a.doRequestWithin(ctx, opts.Timeout, baseURL, key, req.Method, path, nil, "", opts.Debug)
		if e != nil {
			return nil, e
		}
		if err := writeJSON(a.Stdout, resp.Value); err != nil {
			return nil, networkError("OUTPUT_WRITE_FAILED", "Could not write paginated output.", err)
		}
		pages++
		if progressErr := a.writeProgress(opts, map[string]any{"type": "pagination", "pages": pages}, fmt.Sprintf("Fetched page %d", pages)); progressErr != nil {
			return nil, progressErr
		}
		m, ok := resp.Value.(map[string]any)
		if !ok {
			return nil, invalidResponseError("INVALID_PAGINATION_RESPONSE", "Page response is not an object.", nil)
		}
		token, tokenErr := nextPageToken(m)
		if tokenErr != nil {
			return nil, tokenErr
		}
		if token == "" {
			return outputHandled{}, nil
		}
		if seenPageTokens[token] {
			return nil, invalidResponseError("PAGINATION_TOKEN_LOOP", "Flint returned a repeated pagination token.", nil)
		}
		path = setQuery(req.Path, "page_token", token)
	}
}
func setQuery(path, key, value string) string {
	u, _ := url.Parse(path)
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

func openBrowser(target string) error {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return fmt.Errorf("browser target must be an absolute HTTP or HTTPS URL")
	}
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
		args = []string{target}
	case "windows":
		command = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", target}
	default:
		command = "xdg-open"
		args = []string{target}
	}
	return exec.Command(command, args...).Start()
}
