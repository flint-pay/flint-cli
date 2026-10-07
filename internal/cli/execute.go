package cli

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type outputHandled struct{}

func (a *App) executeAPI(ctx context.Context, cmd *Command, opts Options, resolved ResolvedConfig, authContext AuthContext, key, baseURL string) (any, *CLIError) {
	ctx = context.WithValue(ctx, responseContractKey{}, cmd)
	req, e := a.prepareRequest(ctx, cmd, opts, resolved, key, baseURL)
	if e != nil {
		return nil, e
	}
	if opts.DryRun == "client" || opts.Preview {
		return previewRequest(req, permissionCommand(cmd, opts), authContext, opts.Preview), nil
	}
	if cmd.CanonicalName == "listen" {
		return a.executeListen(ctx, cmd, opts, req, key, baseURL)
	}
	if cmd.CanonicalName == "api" && opts.Paginate {
		return a.executeRawPagination(ctx, opts, req, key, baseURL)
	}
	if len(opts.WaitFor) > 0 {
		return a.executeWait(ctx, cmd, opts, req, key, baseURL)
	}
	if opts.All {
		return a.executeAllPages(ctx, cmd, opts, req, key, baseURL)
	}
	resp, e := a.doRequestWithin(ctx, opts.Timeout, baseURL, key, req.Method, req.Path, req.Body, req.IdempotencyKey, opts.Debug)
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
		return saveDownload(resp.Value, values[len(values)-1])
	}
	return resp.Value, nil
}

func previewRequest(req preparedRequest, cmd *Command, authContext AuthContext, preview bool) map[string]any {
	data := map[string]any{"method": req.Method, "path": req.Path, "headers": map[string]any{"Idempotency-Key": req.IdempotencyKey}, "body": req.BodyValue, "persistent_side_effects": false}
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

// The order shortcut sends a different operation from standalone creation.
// Resolve its permission contract without changing request-building metadata.
func permissionCommand(cmd *Command, opts Options) *Command {
	if cmd != nil && cmd.CanonicalName == "payment-intents.create" && len(opts.Raw["order"]) > 0 {
		if operation, ok := matchPublicOperation("POST", "/v1/orders/{order_id}/payment-intents"); ok {
			resolved := *cmd
			resolved.OperationID = operation.OperationID
			return &resolved
		}
	}
	return cmd
}

func permissionPreview(cmd *Command, authContext AuthContext) map[string]any {
	if authContext.AuthType == "session" {
		return map[string]any{"status": "unknown", "reason": "Session credential permissions are validated by the API and cannot be checked locally."}
	}
	if cmd == nil || cmd.OperationID == "" {
		return map[string]any{"status": "unknown", "reason": "The raw API command has no fixed operation contract."}
	}
	operation, ok := openAPIOperationByID(cmd.OperationID)
	if !ok {
		return map[string]any{"status": "unknown", "reason": "The operation is missing from the embedded public API contract."}
	}
	granted := make(map[string]bool, len(authContext.Scopes))
	for _, scope := range authContext.Scopes {
		granted[strings.TrimSpace(scope)] = true
	}
	mode := operation.FlintScopesMode
	if mode == "" {
		mode = "all"
	}
	satisfied := len(operation.FlintRequiredScopes) == 0
	missing := make([]string, 0)
	if mode == "any" {
		for _, scope := range operation.FlintRequiredScopes {
			if granted[scope] {
				satisfied = true
				break
			}
		}
		if !satisfied {
			missing = append(missing, operation.FlintRequiredScopes...)
		}
	} else {
		satisfied = true
		for _, scope := range operation.FlintRequiredScopes {
			if !granted[scope] {
				satisfied = false
				missing = append(missing, scope)
			}
		}
	}
	status := "pass"
	if !satisfied {
		status = "fail"
	}
	return map[string]any{
		"status":          status,
		"scope_mode":      mode,
		"required_scopes": append([]string(nil), operation.FlintRequiredScopes...),
		"missing_scopes":  missing,
	}
}

func commandPermissionError(cmd *Command, authContext AuthContext, profile string) *CLIError {
	if authContext.AuthType != "oauth" && authContext.AuthType != "api_key" {
		return nil
	}
	check := permissionPreview(cmd, authContext)
	if check["status"] != "fail" {
		return nil
	}
	missing, _ := check["missing_scopes"].([]string)
	mode, _ := check["scope_mode"].(string)
	requirement := "requires these scopes: "
	required, _ := check["required_scopes"].([]string)
	remediation := "Create an API key with all required scopes: " + strings.Join(required, ", ") + "."
	if mode == "any" {
		requirement = "requires at least one of these scopes: "
		remediation = "Create an API key with at least one of the listed scopes."
	}
	message := "The active credential " + requirement + strings.Join(missing, ", ") + "."
	actions := []any{map[string]any{"reason_message": remediation}}
	if authContext.AuthType == "oauth" {
		requestScopes := missing
		if mode == "any" && len(requestScopes) > 1 {
			requestScopes = requestScopes[:1]
		}
		command := "flint reauth"
		if authContext.OAuthSessionID == "" || authContext.ContextID == "" {
			command = "flint login --new-session"
			if authContext.Environment == "live" {
				command += " --live"
			}
		} else {
			command += " --context " + quoteRemediationArgument(authContext.ContextID)
		}
		if profile != "" {
			command += " --profile " + quoteRemediationArgument(profile)
		}
		for _, scope := range requestScopes {
			command += " --scope " + scope
		}
		message = "The current browser login " + requirement + strings.Join(missing, ", ") + ". Approve additional access in the browser."
		actions = []any{
			map[string]any{"reason_message": "Approve the additional scope access for this context."},
			map[string]any{"command": command},
		}
	} else {
		if credentialFromEnvironment() != "" {
			actions = append(actions, map[string]any{
				"reason_message": "Replace FLINT_API_KEY with the new key, or unset it before using an imported key. FLINT_API_KEY overrides saved credentials.",
			})
		}
		if profile != "" {
			actions = append(actions, map[string]any{
				"reason_message": fmt.Sprintf("When importing the key, set --profile to %q to update the profile used by this command.", profile),
			})
		}
		actions = append(actions, map[string]any{"command": "flint auth import", "url": dashboardAPIKeysURL})
	}
	err := cliError(ExitAuth, "authorization_error", "MISSING_REQUIRED_SCOPES", message)
	err.Details = map[string]any{
		"auth_type":       authContext.AuthType,
		"command":         cmd.CanonicalName,
		"operation_id":    cmd.OperationID,
		"scope_mode":      mode,
		"required_scopes": check["required_scopes"],
		"missing_scopes":  missing,
		"remediation":     map[string]any{"next_actions": actions},
	}
	return err
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
	var last, collection map[string]any
	collectionField := "data"
	if cmd.OperationID == "getResourceTimeline" {
		collectionField = "entries"
	}
	path := req.Path
	pages := 0
	seenPageTokens := map[string]bool{}
	for {
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
			combined = append(combined, data...)
		} else {
			return nil, invalidResponseError("INVALID_PAGINATION_RESPONSE", "List response collection is not an array.", nil)
		}
		last = m
		token, tokenErr := nextPageToken(m)
		if tokenErr != nil {
			return nil, tokenErr
		}
		if token == "" {
			break
		}
		if seenPageTokens[token] {
			return nil, invalidResponseError("PAGINATION_TOKEN_LOOP", "Flint returned a repeated pagination token.", nil)
		}
		path = setQuery(path, "page_token", token)
		if e := a.writeProgress(opts, map[string]any{"type": "pagination", "pages": pages, "resources": len(combined)}, fmt.Sprintf("Fetched page %d (%d resources)", pages, len(combined))); e != nil {
			return nil, e
		}
	}
	collection[collectionField] = combined
	last["next_page_token"] = ""
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
			err := cliError(ExitWait, "wait_condition_unmet", "WAIT_CONDITION_UNMET", "The wait bound passed before the requested condition was met.")
			err.Details = map[string]any{"conditions": opts.WaitFor, "last": last}
			return last, err
		}
		requestTimeout := min(opts.Timeout, remaining)
		resp, requestErr := a.doRequestWithin(ctx, requestTimeout, baseURL, key, req.Method, req.Path, nil, "", opts.Debug)
		if requestErr != nil {
			if !a.Now().Before(deadline) {
				err := cliError(ExitWait, "wait_condition_unmet", "WAIT_CONDITION_UNMET", "The wait bound passed before the requested condition was met.")
				err.Details = map[string]any{"conditions": opts.WaitFor, "last": last}
				return last, err
			}
			return last, requestErr
		}
		last = resp.Value
		matched, observed := conditionsMatch(last, conditions)
		if matched {
			return last, nil
		}
		if terminallyImpossible(cmd, conditions, observed) {
			err := cliError(ExitWait, "wait_condition_unmet", "WAIT_CONDITION_IMPOSSIBLE", "The resource reached a terminal state that cannot satisfy the requested condition.")
			err.Details = map[string]any{"conditions": opts.WaitFor, "observed": observed, "last": last}
			return last, err
		}
		if e := a.writeProgress(opts, map[string]any{"type": "wait", "conditions": opts.WaitFor, "observed": observed}, "Waiting for "+strings.Join(opts.WaitFor, ", ")); e != nil {
			return last, e
		}
		if !a.Now().Before(deadline) {
			err := cliError(ExitWait, "wait_condition_unmet", "WAIT_CONDITION_UNMET", "The wait bound passed before the requested condition was met.")
			err.Details = map[string]any{"conditions": opts.WaitFor, "observed": observed, "last": last}
			return last, err
		}
		remaining = deadline.Sub(a.Now())
		if delay > remaining {
			delay = remaining
		}
		if !sleepContext(ctx, delay) {
			return last, networkError("REQUEST_TIMEOUT", "The request timed out.", ctx.Err())
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
func conditionsMatch(value any, conditions []waitCondition) (bool, map[string]string) {
	observed := map[string]string{}
	for _, c := range conditions {
		v, ok := lookupResourcePath(value, c.Path)
		if !ok {
			return false, observed
		}
		text := fmt.Sprint(v)
		observed[c.Path] = text
		if text != c.Expected {
			return false, observed
		}
	}
	return true, observed
}
func lookupResourcePath(value any, path string) (any, bool) {
	if v, ok := lookupPath(value, path); ok {
		return v, true
	}
	if v, ok := lookupPath(value, "data."+path); ok {
		return v, true
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	data, ok := m["data"].(map[string]any)
	if !ok {
		return nil, false
	}
	for _, v := range data {
		if nested, ok := v.(map[string]any); ok {
			if found, ok := lookupPath(nested, path); ok {
				return found, true
			}
		}
	}
	return nil, false
}

var terminalStates = map[string]map[string]bool{
	"payment-intents.get":   {"succeeded": true, "canceled": true, "expired": true},
	"refunds.get":           {"succeeded": true, "failed": true, "canceled": true, "partially_succeeded": true},
	"checkout-sessions.get": {"paid": true, "expired": true, "closed": true, "invalidated": true},
	"orders.get":            {"closed": true},
}

func terminallyImpossible(cmd *Command, conditions []waitCondition, observed map[string]string) bool {
	states := terminalStates[cmd.CanonicalName]
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
