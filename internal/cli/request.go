package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"time"
)

type preparedRequest struct {
	Method         string
	Path           string
	Body           []byte
	BodyValue      map[string]any
	IdempotencyKey string
	Warnings       []map[string]any
}

// commandIdempotencyClass derives write safety from the vendored API contract.
func commandIdempotencyClass(cmd *Command) string {
	if cmd == nil || cmd.Local || cmd.OperationID == "" {
		return "none"
	}
	operation, _ := openAPIOperationByID(cmd.OperationID)
	for _, parameter := range operation.Parameters {
		if parameter["in"] == "header" && parameter["name"] == "Idempotency-Key" {
			if parameter["required"] == true {
				return "required"
			}
			return "optional"
		}
	}
	return "none"
}

func requestIdempotencyKey(cmd *Command, opts Options) (string, *CLIError) {
	// Match HTTP header whitespace normalization before deciding replay safety.
	key := strings.Trim(opts.IdempotencyKey, " \t")
	if unknownRawOperation(cmd) {
		return key, nil
	}
	switch commandIdempotencyClass(cmd) {
	case "required":
		if strings.TrimSpace(key) == "" {
			return "", usageError("IDEMPOTENCY_KEY_REQUIRED", "--idempotency-key is required for this operation. Save a key with the action before sending and reuse the same key on retry; a key generated for one command cannot protect a later run from repeating the action.", "--idempotency-key")
		}
		return key, nil
	case "optional":
		if key != "" {
			return key, nil
		}
		key, err := newIdempotencyKey()
		if err != nil {
			return "", networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate an idempotency key.", err)
		}
		return key, nil
	default:
		return "", nil
	}
}

func (a *App) prepareRequest(ctx context.Context, cmd *Command, opts Options, resolved ResolvedConfig, key, baseURL string) (req preparedRequest, requestErr *CLIError) {
	req = preparedRequest{Method: cmd.Method, Path: cmd.APIPath}
	req.IdempotencyKey, requestErr = requestIdempotencyKey(cmd, opts)
	if requestErr != nil {
		return req, requestErr
	}
	if e := validateExpansions(cmd.OperationID, opts.Expand); e != nil {
		return req, e
	}
	if cmd.CanonicalName == "api" {
		if e := validateRawPublicRoute(cmd.Method, cmd.APIPath); e != nil {
			return req, e
		}
	}
	if unknownRawOperation(cmd) {
		warning := fmt.Sprintf("%s %s is absent from CLI %s schema snapshot %s. Body and scope validation are unavailable; unknown writes are sent once, without automatic retries.", cmd.Method, cmd.APIPath, a.Info.Version, embeddedSnapshotHash())
		req.Warnings = append(req.Warnings, map[string]any{"message": warning})
		if !opts.Quiet {
			fmt.Fprintln(a.Stderr, "warning: "+warning)
		}
	}
	body, inputErr := a.readInput(opts.Input)
	if inputErr != nil {
		return req, inputErr
	}
	for _, arg := range cmd.Arguments {
		// Query arrays and their history references are normalized together below.
		if arg.Query != "" {
			continue
		}
		var values []string
		if arg.Positional > 0 && len(opts.Positionals) >= arg.Positional {
			values = []string{opts.Positionals[arg.Positional-1]}
		}
		if arg.Flag != "" {
			values = argumentValues(arg, opts)
		}
		if len(values) == 0 {
			continue
		}
		for i, value := range values {
			if arg.IDPrefix != "" && !(arg.Nullable && value == "null") {
				value = strings.TrimSpace(value)
				values[i] = value
				if e := validateIDPrefix(value, arg); e != nil {
					return req, e
				}
			}
			if arg.AcceptsHistoryRef && strings.HasPrefix(value, "@last") {
				resolvedID, e := a.resolveHistoryRef(value, arg.IDPrefix, historyScopeFromResolved(resolved))
				if e != nil {
					return req, e
				}
				if opts.Debug {
					fmt.Fprintf(a.Stderr, "debug: resolved %s to %s\n", value, resolvedID)
				}
				value = resolvedID
				values[i] = value
			}
			if arg.Positional > 0 {
				req.Path = strings.Replace(req.Path, "{"+arg.Name+"}", url.PathEscape(value), 1)
			}
			if arg.BodyPath != "" && arg.Name != "input" {
				if strings.HasSuffix(arg.BodyPath, ".currency") {
					value = strings.ToUpper(value)
				}
				var converted any
				var e error
				if arg.Nullable && value == "null" {
					converted = nil
				} else if arg.Type == "time" {
					location := time.UTC
					if calendarTime(value) {
						var note string
						location, note = a.reportingTimezone(ctx, opts, resolved, key, baseURL)
						if note != "" && !opts.Quiet {
							fmt.Fprintln(a.Stderr, "Note: "+note)
						}
					}
					var timeErr *CLIError
					converted, timeErr = resolveBodyTime(value, a.Now(), location)
					if timeErr != nil {
						timeErr.Param = arg.Name
						return req, timeErr
					}
				} else {
					converted, e = convertValue(value, arg.Type)
				}
				if e != nil {
					return req, usageError("INVALID_ARGUMENT", fmt.Sprintf("Invalid value for %s: %v", arg.Flag, e), arg.Name)
				}
				if arg.Repeat && !(arg.Nullable && value == "null") {
					existing, _ := lookupPath(body, arg.BodyPath)
					list, _ := existing.([]any)
					list = append(list, converted)
					if e := setBodyPath(body, arg.BodyPath, list); e != nil {
						return req, usageError("INVALID_ARGUMENT", e.Error(), arg.Name)
					}
				} else if e := setBodyPath(body, arg.BodyPath, converted); e != nil {
					return req, usageError("INVALID_ARGUMENT", e.Error(), arg.Name)
				}
			}
		}
	}

	if e := selectRequestUnionTags(cmd, opts, body); e != nil {
		return req, e
	}
	query := url.Values{}
	var reportingLocation *time.Location
	var timezoneNote string
	for _, arg := range cmd.Arguments {
		if arg.Query == "" {
			continue
		}
		values := argumentValues(arg, opts)
		for _, value := range values {
			if arg.IDPrefix != "" {
				value = strings.TrimSpace(value)
				if e := validateIDPrefix(value, arg); e != nil {
					return req, e
				}
			}
			if arg.AcceptsHistoryRef && strings.HasPrefix(value, "@last") {
				resolvedID, e := a.resolveHistoryRef(value, arg.IDPrefix, historyScopeFromResolved(resolved))
				if e != nil {
					return req, e
				}
				value = resolvedID
			}
			if arg.Type == "time" {
				location := time.UTC
				if calendarTime(value) {
					if reportingLocation == nil {
						reportingLocation, timezoneNote = a.reportingTimezone(ctx, opts, resolved, key, baseURL)
					}
					location = reportingLocation
				}
				instant, e := resolveTime(value, a.Now(), location)
				if e != nil {
					return req, usageError("INVALID_TIME", e.Error(), arg.Name)
				}
				if !opts.Quiet {
					fmt.Fprintf(a.Stderr, "Note: %s %q is %s in %s.\n", arg.Query, value, instant, location)
					if calendarTime(value) && timezoneNote != "" {
						fmt.Fprintln(a.Stderr, "Note: "+timezoneNote)
					}
				}
				value = instant
			}
			query.Add(arg.Query, value)
		}
		if arg.Repeat && arg.Style == "form" && arg.Explode != nil && !*arg.Explode && len(query[arg.Query]) > 0 {
			query.Set(arg.Query, strings.Join(query[arg.Query], ","))
		}
	}
	if opts.Limit > 0 {
		query.Set("page_size", strconv.Itoa(min(opts.Limit, 100)))
	}
	if opts.PageSize > 0 && opts.Limit == 0 {
		query.Set("page_size", strconv.Itoa(opts.PageSize))
	}
	if opts.PageToken != "" {
		query.Set("page_token", opts.PageToken)
	}
	for _, expand := range opts.Expand {
		query.Add("expand", expand)
	}
	if cmd.CanonicalName != "api" && cmd.OperationID == "createPaymentIntent" && opts.Input == "" {
		if len(opts.Raw["amount"]) == 0 || len(opts.Raw["currency"]) == 0 {
			return req, usageError("MISSING_REQUIRED_ARGUMENT", "Standalone payment intents require --amount and --currency.", "amount")
		}
		if len(opts.Raw["payment-options"]) == 0 {
			return req, usageError("MISSING_REQUIRED_ARGUMENT", "Standalone payment intents require at least one --payment-options.", "payment_option")
		}
	}

	if cmd.CanonicalName != "api" && cmd.OperationID == "payOrder" {
		if e := a.prepareOrderPayment(ctx, body, cmd, opts, resolved, key, baseURL, req.Path); e != nil {
			return req, e
		}
	}
	if len(query) > 0 {
		var e *CLIError
		if req.Path, e = withRequestQuery(req.Path, query); e != nil {
			return req, e
		}
	}
	// Public API handlers with request schemas decode JSON even when every field
	// is optional. Send an empty object so omission means no fields, not invalid JSON.
	operation, _ := openAPIOperationByID(cmd.OperationID)
	if len(body) > 0 || opts.Input != "" || cmd.InputSchema != "" || operation.RequestBody != nil {
		mediaType, _ := requestContentSchema(operation.RequestBody)
		if mediaType == "application/x-www-form-urlencoded" {
			form := url.Values{}
			for name, value := range body {
				text, ok := value.(string)
				if !ok {
					return req, usageError("INVALID_INPUT", "Form request fields must be strings.", name)
				}
				form.Set(name, text)
			}
			req.Body = []byte(form.Encode())
		} else {
			if mediaType == "application/json" || operation.RequestBody == nil {
				if e := a.validateBody(cmd, opts, body, &req); e != nil {
					return req, e
				}
			}
			raw, e := json.Marshal(body)
			if e != nil {
				return req, usageError("INVALID_INPUT", e.Error(), "input")
			}
			req.Body = raw
		}
		req.BodyValue = body
	}
	return req, nil
}

// selectRequestUnionTags fills the discriminator that a convenience flag
// implies. The public contract requires an explicit tag on every union
// request body; a tag already present in --input is kept, so JSON input must
// still satisfy the contract on its own.
func selectRequestUnionTags(cmd *Command, opts Options, body map[string]any) *CLIError {
	// Raw requests resolve operation IDs for contract metadata and safety checks.
	// Convenience defaults belong only to named commands, never the raw body.
	if cmd.CanonicalName == "api" {
		return nil
	}

	flagged := func(names ...string) bool {
		for _, name := range names {
			if len(opts.Raw[name]) > 0 {
				return true
			}
		}
		return false
	}
	switch publicOperationOverlays[cmd.OperationID].Union {
	case "quick_pay":
		if _, hasType := body["type"]; !hasType && flagged("quick-pay-name", "amount", "currency") {
			body["type"] = "quick_pay"
		}
	case "line_items":
		if !flagged("item-name", "amount", "currency") {
			return nil
		}
		items, _ := body["line_items"].([]any)
		if len(items) == 0 {
			return nil
		}
		if item, ok := items[0].(map[string]any); ok {
			if _, hasType := item["type"]; !hasType {
				item["type"] = "custom_fixed"
			}
		}
	case "credential":
		credentials := map[string]string{
			"confirmation-token":   "stripe_confirmation_token",
			"payment-source-token": "token",
			"payment-method-id":    "payment_method",
		}
		var selected []string
		for _, name := range []string{"confirmation-token", "payment-source-token", "payment-method-id"} {
			if flagged(name) {
				selected = append(selected, name)
			}
		}
		if len(selected) == 0 {
			// Without a credential, confirm continues with the attached payment
			// source, such as after the buyer completes current_payment_action.
			if _, present := body["credential_type"]; !present && opts.Input == "" {
				body["credential_type"] = "attached"
			}
			return nil
		}
		if len(selected) > 1 {
			return usageError("CONFLICTING_ARGUMENTS", "Use only one of --confirmation-token, --payment-source-token, or --payment-method.", strings.ReplaceAll(selected[1], "-", "_"))
		}
		credentialType := credentials[selected[0]]
		if existing, present := body["credential_type"]; present && existing != credentialType {
			return usageError("CONFLICTING_ARGUMENTS", fmt.Sprintf("--%s requires credential_type %s.", selected[0], credentialType), "credential_type")
		}
		body["credential_type"] = credentialType
	case "api_key":
		return inferAPIKeyAccess(cmd, opts, body)
	case "invoice":
		if !flagged("order-id") {
			return nil
		}
		if existing, present := body["type"]; present && existing != "order" {
			return usageError("CONFLICTING_ARGUMENTS", "--order-id requires type order.", "type")
		}
		body["type"] = "order"
	}
	return nil
}

func withRequestQuery(path string, query url.Values) (string, *CLIError) {
	parsed, err := url.Parse(path)
	if err != nil {
		return path, usageError("INVALID_REQUEST_URL", err.Error(), "path")
	}
	merged := parsed.Query()
	for name, values := range query {
		merged[name] = values
	}
	parsed.RawQuery = merged.Encode()
	return parsed.String(), nil
}

// validateBody checks the merged JSON body locally. The raw api command is the
// unvalidated escape hatch.
func (a *App) validateBody(cmd *Command, opts Options, body map[string]any, req *preparedRequest) *CLIError {
	if cmd.CanonicalName == "api" || cmd.OperationID == "" {
		return nil
	}
	target, label := cmd, cmd.CanonicalName

	warnings, e := validateRequestBody(target, label, body)
	if e != nil {
		if details, ok := e.Details.(map[string]any); ok {
			if problems, ok := details["problems"].([]map[string]any); ok {
				for _, problem := range problems {
					path, _ := problem["path"].(string)
					for _, arg := range cmd.Arguments {
						if arg.BodyPath != "" && path == jsonFieldPath(strings.Split(arg.BodyPath, ".")) && problem["message"] == "required field is missing" {
							message := fmt.Sprintf("Missing required field %s (%s or --input).", arg.BodyPath, arg.Flag)
							e.Message = strings.ReplaceAll(e.Message, path+": required field is missing", path+": "+message)
							problem["message"] = message
						}
						flag := strings.TrimPrefix(arg.Flag, "--")
						if arg.BodyPath != "" && len(opts.Raw[flag]) > 0 && path == jsonFieldPath(strings.Split(arg.BodyPath, ".")) {
							annotated := path + " (from " + arg.Flag + ")"
							problem["path"] = annotated
							e.Message = strings.ReplaceAll(e.Message, path+":", annotated+":")
							e.Param = flag
						}
					}
				}
			}
		}
		return e
	}
	req.Warnings = warnings
	if !opts.Quiet {
		for _, warning := range warnings {
			line := fmt.Sprintf("warning: request body field %s is not a documented %s request field and will be sent as-is.", warning["path"], label)
			if suggestion, ok := warning["suggestion"].(string); ok {
				line += " Did you mean " + suggestion + "?"
			}
			fmt.Fprintln(a.Stderr, line)
		}
	}
	return nil
}

func (a *App) readInput(path string) (map[string]any, *CLIError) {
	if path == "" {
		return map[string]any{}, nil
	}
	var reader io.Reader
	if strings.HasPrefix(strings.TrimSpace(path), "{") {
		reader = strings.NewReader(path)
	} else if path == "-" {
		reader = a.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, usageError("INPUT_READ_FAILED", err.Error(), "input")
		}
		defer f.Close()
		reader = f
	}
	input := &contextInputReader{ctx: a.commandContext(), reader: reader}
	raw, err := io.ReadAll(io.LimitReader(input, (8<<20)+1))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, networkError("CLIENT_CANCELED", "JSON input reading was canceled.", err)
		}
		return nil, usageError("INPUT_READ_FAILED", err.Error(), "input")
	}
	if len(raw) > 8<<20 {
		return nil, usageError("INPUT_TOO_LARGE", "Input JSON exceeds 8 MiB.", "input")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, usageError("EMPTY_INPUT", "Input JSON is empty.", "input")
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	valueStart := int64(len(raw) - len(bytes.TrimLeft(raw, " \t\r\n")))
	if err := dec.Decode(&body); err != nil {
		return nil, invalidInputJSON(raw, err, valueStart)
	}
	if body == nil {
		return nil, invalidInputJSON(raw, errors.New("input must be a JSON object, not null"), valueStart)
	}
	firstEnd := dec.InputOffset()
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			rest := raw[firstEnd:]
			err = errors.New("input must contain exactly one JSON object")
			return nil, invalidInputJSON(raw, err, firstEnd+int64(len(rest)-len(bytes.TrimLeft(rest, " \t\r\n"))))
		}
		return nil, invalidInputJSON(raw, err, valueStart)
	}
	return body, nil
}

// invalidInputJSON reports the 1-based line and column of the offending byte;
// position is used when the decoder error carries no offset.
func invalidInputJSON(raw []byte, err error, position int64) *CLIError {
	message := strings.TrimPrefix(err.Error(), "json: ")
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxErr):
		position = syntaxErr.Offset - 1
	case errors.As(err, &typeErr):
		message = "input must be a JSON object, not " + typeErr.Value
	case errors.Is(err, io.ErrUnexpectedEOF):
		message = "unexpected end of input"
		position = int64(len(bytes.TrimRight(raw, " \t\r\n")))
	}
	line, column := 1, 1
	for _, c := range raw[:min(max(position, 0), int64(len(raw)))] {
		if c == '\n' {
			line, column = line+1, 1
		} else if c&0xC0 != 0x80 {
			column++
		}
	}
	e := usageError("INVALID_INPUT_JSON", fmt.Sprintf("Input JSON is invalid at line %d, column %d: %s.", line, column, message), "input")
	e.Details = map[string]any{"line": line, "column": column}
	return e
}

func convertValue(value, typ string) (any, error) {
	switch typ {
	case "integer":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			if _, floatErr := strconv.ParseFloat(value, 64); floatErr == nil {
				return nil, fmt.Errorf("%q is not a whole number; money amounts use the smallest currency unit, so 10.00 USD is 1000", value)
			}
			return nil, fmt.Errorf("%q is not a whole number", value)
		}
		return n, nil
	case "number":
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", value)
		}
		return n, nil
	case "date":
		if _, err := time.Parse(time.DateOnly, value); err != nil {
			return nil, fmt.Errorf("date must be YYYY-MM-DD")
		}
		return value, nil
	case "boolean":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("%q is not true or false", value)
		}
		return b, nil
	default:
		return value, nil
	}
}

func setBodyPath(root map[string]any, path string, value any) error {
	parts := strings.Split(path, ".")
	var current any = root
	for i, part := range parts {
		last := i == len(parts)-1
		switch container := current.(type) {
		case map[string]any:
			if last {
				container[part] = value
				return nil
			}
			nextPart := parts[i+1]
			next, ok := container[part]
			if !ok {
				if _, err := strconv.Atoi(nextPart); err == nil {
					next = []any{}
				} else {
					next = map[string]any{}
				}
				container[part] = next
			}
			current = next
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 {
				return fmt.Errorf("invalid array path %s", path)
			}
			for len(container) <= index {
				container = append(container, nil)
			}
			// The only registry array paths currently address index zero on a root-owned slice.
			if i == 0 {
				return fmt.Errorf("array paths cannot start with an index")
			}
			parentPath := strings.Join(parts[:i], ".")
			if err := replaceSlice(root, parentPath, container); err != nil {
				return err
			}
			if last {
				container[index] = value
				_ = replaceSlice(root, parentPath, container)
				return nil
			}
			if container[index] == nil {
				container[index] = map[string]any{}
			}
			current = container[index]
		default:
			return fmt.Errorf("input path %s conflicts with an existing scalar", path)
		}
	}
	return nil
}

func replaceSlice(root map[string]any, path string, value []any) error {
	parts := strings.Split(path, ".")
	cur := root
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			return fmt.Errorf("input path %s conflicts with an existing value", path)
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = value
	return nil
}

func validateIDPrefix(value string, arg Arg) *CLIError {
	if strings.HasPrefix(value, arg.IDPrefix) || strings.HasPrefix(value, "@last") {
		return nil
	}
	for _, prefix := range publicResourceIDPrefixes {
		if prefix != arg.IDPrefix && strings.HasPrefix(value, prefix) {
			return usageError("INVALID_RESOURCE_ID", fmt.Sprintf("%s expects an ID starting with %s; got %s.", arg.Name, arg.IDPrefix, value), arg.Name)
		}
	}
	return nil
}

func (a *App) prepareOrderPayment(ctx context.Context, body map[string]any, cmd *Command, opts Options, resolved ResolvedConfig, key, baseURL, path string) *CLIError {
	values := opts.Raw["payment-source-token"]
	if len(values) == 0 {
		return nil
	}
	if action, present := body["action"]; present && action != "confirm_payment_intents" {
		return usageError("CONFLICTING_ARGUMENTS", "--payment-source-token requires action confirm_payment_intents.", "action")
	}
	for _, field := range []string{"payment_intents", "payment_source", "setup_payment_source", "payment_attempt_id"} {
		if _, present := body[field]; present {
			return usageError("CONFLICTING_ARGUMENTS", "--payment-source-token cannot be combined with "+field+".", field)
		}
	}
	body["action"] = "confirm_payment_intents"
	delete(body, "payment_source_tokens")
	selections := make([]any, 0, len(values))
	for _, entry := range values {
		intentID, token, found := strings.Cut(entry, "=")
		if !found {
			if opts.DryRun == "client" {
				return usageError("PAYMENT_INTENT_MAPPING_REQUIRED", "Client dry-run requires payment-intent-id=token syntax because it performs no API reads.", "payment_source_token")
			}
			resolvedID, e := a.resolveSingleOrderPaymentIntent(ctx, path, key, baseURL, opts.Timeout, opts.Debug)
			if e != nil {
				return e
			}
			intentID = resolvedID
			token = entry
		} else if strings.HasPrefix(intentID, "@last") {
			v, e := a.resolveHistoryRef(intentID, "pi_", historyScopeFromResolved(resolved))
			if e != nil {
				return e
			}
			intentID = v
		}
		if !strings.HasPrefix(intentID, "pi_") {
			return usageError("INVALID_PAYMENT_INTENT_ID", "--payment-source-token map keys must be payment intent IDs.", "payment_source_token")
		}
		selections = append(selections, map[string]any{"credential_type": "token", "payment_intent_id": intentID, "token": token})
	}
	body["payment_intents"] = selections
	return nil
}

func (a *App) resolveSingleOrderPaymentIntent(ctx context.Context, payPath, key, baseURL string, timeout time.Duration, debug bool) (string, *CLIError) {
	orderPath := setQuery(strings.TrimSuffix(payPath, "/pay"), "expand", "payment_intents")
	resp, e := a.doRequestWithin(ctx, timeout, baseURL, key, http.MethodGet, orderPath, nil, "", debug)
	if e != nil {
		return "", e
	}
	var candidates []string
	if raw, ok := lookupPath(resp.Value, "data.payment_intents"); ok {
		if intents, ok := raw.([]any); ok {
			for _, item := range intents {
				intent, _ := item.(map[string]any)
				status, _ := intent["status"].(string)
				id, _ := intent["payment_intent_id"].(string)
				if id != "" && (status == "requires_payment_method" || status == "requires_confirmation") {
					candidates = append(candidates, id)
				}
			}
		}
	}
	sort.Strings(candidates)
	if len(candidates) != 1 {
		return "", usageError("AMBIGUOUS_PAYMENT_INTENT", fmt.Sprintf("A bare payment source token requires exactly one order payment intent awaiting confirmation; found %d (%s).", len(candidates), strings.Join(candidates, ", ")), "payment_source_token")
	}
	return candidates[0], nil
}

func validateRawPublicRoute(method, path string) *CLIError {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return usageError("INVALID_HTTP_METHOD", "flint api accepts get, post, put, patch, or delete.", "method")
	}
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.Opaque != "" || strings.Contains(path, "#") || !strings.HasPrefix(u.Path, "/v1/") {
		return usageError("NON_PUBLIC_API_PATH", "flint api only accepts relative /v1/ public API paths without fragments.", "path")
	}
	// Reject traversal, separator aliases, double encoding, and control characters
	// before classification so upstream routing cannot reinterpret the path.
	if pathpkg.Clean(u.Path) != u.Path || strings.ContainsAny(u.Path, "\\%") {
		return usageError("UNDOCUMENTED_API_OPERATION", "Use a canonical /v1/ path without traversal or repeated separators.", "path")
	}
	for _, ch := range u.Path {
		if ch < 0x20 || ch == 0x7f {
			return usageError("NON_PUBLIC_API_PATH", "API paths must not contain control characters.", "path")
		}
	}
	// Escaped slashes change segment boundaries and can bypass known-route safety.
	if strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") || strings.Contains(strings.ToLower(u.EscapedPath()), "%5c") {
		return usageError("NON_PUBLIC_API_PATH", "API path separators must not be escaped.", "path")
	}
	if _, ok := matchPublicOperation(method, path); ok {
		return nil
	}
	if u.RawPath != "" {
		return usageError("UNDOCUMENTED_API_OPERATION", "Unknown API routes require a canonical escaped /v1/ path.", "path")
	}
	return nil
}

// inferAPIKeyAccess fills the access discriminator for API key commands.
// --scope without --access selects restricted, as the Dashboard does, and
// --access, --scope, or --test-key-name on sandboxes create imply
// issue_test_key. JSON input keeps its explicit fields and must satisfy the
// public contract. Creating a key with no access choice names the flags
// instead of reporting a schema branch.
func inferAPIKeyAccess(cmd *Command, opts Options, body map[string]any) *CLIError {
	if _, hasAccess := body["access"]; !hasAccess && len(opts.Raw["scopes"]) > 0 {
		body["access"] = "restricted"
	}
	creates := cmd.OperationID != "updateAPIKey"
	if cmd.OperationID == "createSandbox" {
		_, hasIssue := body["issue_test_key"]
		if !hasIssue && (len(opts.Raw["access"]) > 0 || len(opts.Raw["scopes"]) > 0 || len(opts.Raw["test-key-name"]) > 0) {
			body["issue_test_key"] = true
		}
		issue, _ := body["issue_test_key"].(bool)
		creates = issue
	}
	if _, hasAccess := body["access"]; creates && !hasAccess {
		return usageError("MISSING_REQUIRED_ARGUMENT", "Choose the key's access with --access full, --access read_only, or --scopes SCOPE for restricted access.", "access")
	}
	return nil
}
