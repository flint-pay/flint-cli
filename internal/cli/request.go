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
}

func (a *App) prepareRequest(ctx context.Context, cmd *Command, opts Options, resolved ResolvedConfig, key, baseURL string) (preparedRequest, *CLIError) {
	req := preparedRequest{Method: cmd.Method, Path: cmd.APIPath}
	if e := validateExpansions(cmd.OperationID, opts.Expand); e != nil {
		return req, e
	}
	if cmd.CanonicalName == "api" {
		if e := validateRawPublicRoute(cmd.Method, cmd.APIPath); e != nil {
			return req, e
		}
	}
	body, inputErr := a.readInput(opts.Input)
	if inputErr != nil {
		return req, inputErr
	}
	for _, arg := range cmd.Arguments {
		var values []string
		if arg.Positional > 0 && len(opts.Positionals) >= arg.Positional {
			values = []string{opts.Positionals[arg.Positional-1]}
		}
		if arg.Flag != "" {
			values = opts.Raw[strings.TrimPrefix(arg.Flag, "--")]
		}
		if len(values) == 0 {
			continue
		}
		for i, value := range values {
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
			if arg.Query != "" {
				continue
			}
			if arg.BodyPath != "" && arg.Name != "input" {
				converted, e := convertValue(value, arg.Type)
				if e != nil {
					return req, usageError("INVALID_ARGUMENT", fmt.Sprintf("Invalid value for %s: %v", arg.Flag, e), arg.Name)
				}
				if arg.Repeat {
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
	query := url.Values{}
	for _, arg := range cmd.Arguments {
		if arg.Query == "" {
			continue
		}
		values := opts.Raw[strings.TrimPrefix(arg.Flag, "--")]
		for _, value := range values {
			if arg.AcceptsHistoryRef && strings.HasPrefix(value, "@last") {
				resolvedID, e := a.resolveHistoryRef(value, arg.IDPrefix, historyScopeFromResolved(resolved))
				if e != nil {
					return req, e
				}
				value = resolvedID
			}
			if arg.Type == "time" {
				instant, e := resolveTime(value, a.Now())
				if e != nil {
					return req, usageError("INVALID_TIME", e.Error(), arg.Name)
				}
				if opts.Debug {
					fmt.Fprintf(a.Stderr, "debug: resolved %s to %s\n", value, instant)
				}
				value = instant
			}
			query.Add(arg.Query, value)
		}
	}
	if opts.PageSize > 0 {
		query.Set("page_size", strconv.Itoa(opts.PageSize))
	}
	if opts.PageToken != "" {
		query.Set("page_token", opts.PageToken)
	}
	for _, expand := range opts.Expand {
		query.Add("expand", expand)
	}
	if cmd.CanonicalName == "payment-intents.create" {
		orderValues := opts.Raw["order"]
		if len(orderValues) > 0 {
			if len(opts.Raw["amount"]) > 0 || len(opts.Raw["currency"]) > 0 || len(opts.Raw["payment-option"]) > 0 || len(opts.Raw["transaction-purpose"]) > 0 {
				return req, usageError("CONFLICTING_ARGUMENTS", "--order cannot be combined with --amount, --currency, --payment-option, or --transaction-purpose because the order-scoped request has a separate contract.", "order")
			}
			if _, hasAmount := body["amount_money"]; hasAmount {
				return req, usageError("CONFLICTING_ARGUMENTS", "--order cannot be combined with amount_money because the amount derives from the order balance.", "order")
			}
			orderID := orderValues[len(orderValues)-1]
			if strings.HasPrefix(orderID, "@last") {
				v, e := a.resolveHistoryRef(orderID, "ord_", historyScopeFromResolved(resolved))
				if e != nil {
					return req, e
				}
				orderID = v
			}
			req.Path = "/v1/orders/" + url.PathEscape(orderID) + "/payment-intents"
			delete(body, "order_id")
		} else if opts.Input == "" {
			if len(opts.Raw["amount"]) == 0 || len(opts.Raw["currency"]) == 0 {
				return req, usageError("MISSING_REQUIRED_ARGUMENT", "Standalone payment intents require --amount and --currency.", "amount")
			}
			if len(opts.Raw["payment-option"]) == 0 {
				return req, usageError("MISSING_REQUIRED_ARGUMENT", "Standalone payment intents require at least one --payment-option.", "payment_option")
			}
		}
	}
	if cmd.CanonicalName == "orders.pay" {
		if e := a.prepareOrderPayment(ctx, body, cmd, opts, resolved, key, baseURL, req.Path); e != nil {
			return req, e
		}
	}
	if len(query) > 0 {
		parsed, err := url.Parse(req.Path)
		if err != nil {
			return req, usageError("INVALID_REQUEST_URL", err.Error(), "path")
		}
		merged := parsed.Query()
		for name, values := range query {
			merged[name] = values
		}
		parsed.RawQuery = merged.Encode()
		req.Path = parsed.String()
	}
	if cmd.Mutation {
		req.IdempotencyKey = opts.IdempotencyKey
		if req.IdempotencyKey == "" {
			var keyErr error
			req.IdempotencyKey, keyErr = newIdempotencyKey()
			if keyErr != nil {
				return req, networkError("IDEMPOTENCY_KEY_GENERATION_FAILED", "Could not generate an idempotency key.", keyErr)
			}
		}
	}
	// Public API handlers with request schemas decode JSON even when every field
	// is optional. Send an empty object so omission means no fields, not invalid JSON.
	operation, _ := openAPIOperationByID(cmd.OperationID)
	if len(body) > 0 || opts.Input != "" || cmd.InputSchema != "" || operation.RequestBody != nil {
		raw, e := json.Marshal(body)
		if e != nil {
			return req, usageError("INVALID_INPUT", e.Error(), "input")
		}
		req.Body = raw
		req.BodyValue = body
	}
	return req, nil
}

func (a *App) readInput(path string) (map[string]any, *CLIError) {
	if path == "" {
		return map[string]any{}, nil
	}
	var reader io.Reader
	if path == "-" {
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
			return nil, networkError("REQUEST_CANCELED", "JSON input reading was canceled.", err)
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
	if err := dec.Decode(&body); err != nil {
		return nil, usageError("INVALID_INPUT_JSON", err.Error(), "input")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, usageError("INVALID_INPUT_JSON", "Input must contain exactly one JSON object.", "input")
		}
		return nil, usageError("INVALID_INPUT_JSON", err.Error(), "input")
	}
	if body == nil {
		return nil, usageError("INVALID_INPUT_JSON", "Input must be a JSON object.", "input")
	}
	return body, nil
}

func convertValue(value, typ string) (any, error) {
	switch typ {
	case "integer":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, err
		}
		return n, nil
	case "boolean":
		b, err := strconv.ParseBool(value)
		return b, err
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

func resolveTime(value string, now time.Time) (string, error) {
	if value == "yesterday" {
		return now.Add(-24 * time.Hour).UTC().Format(time.RFC3339), nil
	}
	if d, err := time.ParseDuration(value); err == nil && d > 0 {
		return now.Add(-d).UTC().Format(time.RFC3339), nil
	}
	instant, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", fmt.Errorf("time must be RFC3339, a relative duration such as 2h, or yesterday")
	}
	return instant.UTC().Format(time.RFC3339), nil
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
		selections = append(selections, map[string]any{"payment_intent_id": intentID, "token": token})
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
	_, err := loadOpenAPI()
	if err != nil {
		return cliError(ExitUsage, "internal_error", "SCHEMA_SNAPSHOT_INVALID", err.Error())
	}
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/v1/") {
		return usageError("NON_PUBLIC_API_PATH", "flint api only accepts documented /v1 public API paths.", "path")
	}
	// Validation and safety classification must recognize exactly the same
	// routes, including escaped paths and trailing slashes.
	if _, ok := matchPublicOperation(method, path); ok {
		return nil
	}
	return usageError("UNDOCUMENTED_API_OPERATION", fmt.Sprintf("%s %s is not present in Flint's public OpenAPI contract.", method, u.Path), "path")
}
