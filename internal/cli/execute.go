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
		return previewRequest(req, cmd, authContext, opts.Preview), nil
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
	if opts.Open && a.IsTTY() {
		if target, ok := firstStringAt(resp.Value, "data.hosted_checkout.url", "data.url", "data.checkout_session.url", "data.checkout_session.checkout_url"); ok {
			if err := openBrowser(target); err != nil && !opts.Quiet {
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

func permissionPreview(cmd *Command, authContext AuthContext) map[string]any {
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
