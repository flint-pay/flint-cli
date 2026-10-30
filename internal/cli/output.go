package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/itchyny/gojq"
)

func (a *App) writeError(err *CLIError, opts Options) {
	if opts.Output == "json" || opts.Output == "ndjson" {
		var body map[string]any
		if details, ok := err.Details.(map[string]any); ok {
			_, hasType := details["type"]
			_, hasCode := details["code"]
			_, hasMessage := details["message"]
			if hasType && hasCode && hasMessage {
				copy := copyMap(details)
				if err.RequestID != "" {
					if _, exists := copy["request_id"]; !exists {
						copy["request_id"] = err.RequestID
					}
				}
				body = map[string]any{"error": copy}
			}
		}
		if body == nil {
			e := map[string]any{"type": err.Type, "code": err.Code, "message": err.Message}
			if err.Param != "" {
				e["param"] = err.Param
			}
			if err.Details != nil {
				e["details"] = err.Details
			}
			if err.RequestID != "" {
				e["request_id"] = err.RequestID
			}
			body = map[string]any{"error": e}
		}
		e := body["error"].(map[string]any)
		if err.HTTPStatus != 0 {
			e["http_status"] = err.HTTPStatus
		}
		e["retryable"] = errorRetryable(err)
		if err.RetryAfterMs != nil {
			e["retry_after_ms"] = *err.RetryAfterMs
		}
		if details, ok := e["details"].(map[string]any); ok {
			if remediation, exists := details["remediation"]; exists {
				if server, ok := e["remediation"].(map[string]any); ok {
					merged := copyMap(server)
					if added, ok := remediation.(map[string]any); ok {
						for key, value := range added {
							if key == "next_actions" {
								existing, _ := merged[key].([]any)
								actions, _ := value.([]any)
								merged[key] = append(append([]any(nil), existing...), actions...)
							} else {
								merged[key] = value
							}
						}
					}
					e["remediation"] = merged
				} else {
					e["remediation"] = remediation
				}
			}
		} else if e["details"] != nil {
			e["details"] = map[string]any{"problems": e["details"]}
		} else {
			e["details"] = map[string]any{}
		}
		writer := a.Stdout
		if !opts.streamCommand && opts.Output == "ndjson" {
			writer = a.Stderr
		}
		_ = writeJSON(writer, body)
		return
	}
	fmt.Fprintln(a.Stderr, err.Message)
	if err.RequestID != "" {
		fmt.Fprintf(a.Stderr, "Request ID: %s\n", err.RequestID)
	}
	if details, ok := err.Details.(map[string]any); ok {
		if err.idempotencyKey != "" && details["outcome"] != "unknown" {
			fmt.Fprintln(a.Stderr, "Idempotency key: "+terminalSafe(err.idempotencyKey))
		}
		if remediation, ok := details["remediation"].(map[string]any); ok {
			renderRemediation(a.Stderr, remediation)
		}
		if nested, ok := details["details"].(map[string]any); ok {
			if remediation, ok := nested["remediation"].(map[string]any); ok {
				renderRemediation(a.Stderr, remediation)
			}
		}
	}
	if details, ok := err.Details.(map[string]any); ok && strings.HasPrefix(err.Code, "UNKNOWN_") {
		if suggestions, ok := details["suggestions"].([]string); ok && len(suggestions) > 0 {
			fmt.Fprintln(a.Stderr, "Did you mean: "+strings.Join(suggestions, ", "))
		}
	}
}

func renderRemediation(w io.Writer, remediation map[string]any) {
	if step, _ := remediation["next_steps"].(string); strings.TrimSpace(step) != "" {
		fmt.Fprintln(w, "Next: "+step)
	}
	renderNextActions(w, remediation["next_actions"])
}

func renderNextActions(w io.Writer, value any) {
	items, ok := value.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := m["command"].(string); ok && cmd != "" {
			if target, ok := m["url"].(string); ok && target != "" {
				fmt.Fprintln(w, "Next: "+cmd+" ("+target+")")
			} else {
				fmt.Fprintln(w, "Next: "+cmd)
			}
			continue
		}
		if message, ok := m["reason_message"].(string); ok && message != "" {
			fmt.Fprintln(w, "Next: "+message)
		}
	}
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}

// normalizeOutput gives typed local values the same shape and field visibility
// as JSON responses, while preserving exact integer values.
func normalizeOutput(value any) (any, *CLIError) {
	raw, err := json.Marshal(value)
	if err == nil {
		err = decodeJSONNumbers(raw, &value)
	}
	if err != nil {
		outputErr := cliError(ExitSoftware, errorTypeInternal, "OUTPUT_ENCODING_FAILED", "Could not encode command output.")
		outputErr.Cause = err
		return nil, outputErr
	}
	return value, nil
}

func applyOutputTransforms(ctx context.Context, value any, opts Options) (any, *CLIError) {
	if len(opts.Select) > 0 || opts.Field != "" || opts.JQ != "" {
		// Commands may build envelopes with typed Go values. Normalize through
		// JSON first so every transform sees the same shape users receive.
		var err *CLIError
		value, err = normalizeOutput(value)
		if err != nil {
			return nil, err
		}
	}
	if len(opts.Select) > 0 {
		projected, err := selectFields(value, opts.Select)
		if err != nil {
			return nil, usageError("INVALID_SELECT", err.Error(), "select")
		}
		value = projected
	}
	if opts.Field != "" {
		v, ok := lookupPath(value, opts.Field)
		if !ok {
			return nil, usageError("FIELD_NOT_FOUND", "Field path does not exist: "+opts.Field, "field")
		}
		switch v.(type) {
		case string, float32, float64, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, bool, json.Number, nil:
			value = v
		default:
			return nil, usageError("FIELD_NOT_SCALAR", "--field requires a scalar value.", "field")
		}
	}
	if opts.JQ != "" {
		query, err := gojq.Parse(opts.JQ)
		if err != nil {
			return nil, usageError("INVALID_JQ", err.Error(), "jq")
		}
		iter := query.RunWithContext(ctx, value)
		var values []any
		outputBytes := 2 // JSON array delimiters.
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if err := ctx.Err(); err != nil {
				return nil, networkError("CLIENT_CANCELED", "Output transformation was canceled.", err)
			}
			if err, ok := v.(error); ok {
				return nil, usageError("JQ_EVALUATION_FAILED", err.Error(), "jq")
			}
			if opts.outputLimit > 0 {
				raw, err := json.Marshal(v)
				if err != nil {
					return nil, usageError("JQ_EVALUATION_FAILED", err.Error(), "jq")
				}
				outputBytes += len(raw) + 1
				if outputBytes > opts.outputLimit {
					return nil, networkError("OUTPUT_LIMIT_EXCEEDED", "Output transformation exceeded the MCP output limit.", nil)
				}
			}
			values = append(values, v)
		}
		if len(values) == 1 {
			value = values[0]
		} else {
			value = jqResults(values)
		}
	}
	return value, nil
}

// jqResults holds a --jq expression that produced zero or several values. The
// CLI writes one value per line like jq; embedded callers still see an array.
type jqResults []any

func (a *App) writeJQResults(values jqResults, opts Options) *CLIError {
	for _, v := range values {
		var err error
		if s, ok := v.(string); ok && opts.Output != "json" && opts.Output != "ndjson" {
			_, err = fmt.Fprintln(a.Stdout, s)
		} else {
			err = writeJSON(a.Stdout, v)
		}
		if err != nil {
			return networkError("OUTPUT_WRITE_FAILED", "Could not write command output.", err)
		}
	}
	return nil
}

func applyCommandOutputTransforms(ctx context.Context, value any, cmd *Command, opts Options) (any, *CLIError) {
	if len(opts.Select) > 0 || opts.Field != "" {
		var e *CLIError
		value, e = prepareOptionalFields(value, cmd, opts)
		if e != nil {
			return nil, e
		}
	}
	return applyOutputTransforms(ctx, value, opts)
}

func (a *App) writeResult(value any, cmd *Command, opts Options) *CLIError {
	value, err := applyCommandOutputTransforms(a.commandContext(), value, cmd, opts)
	if err != nil {
		return err
	}
	if opts.Field != "" {
		if value == nil {
			value = "null"
		}
		if _, err := fmt.Fprintln(a.Stdout, value); err != nil {
			return networkError("OUTPUT_WRITE_FAILED", "Could not write command output.", err)
		}
		return nil
	}
	if values, ok := value.(jqResults); ok {
		return a.writeJQResults(values, opts)
	}
	if opts.Output == "json" || opts.Output == "ndjson" {
		if err := writeJSON(a.Stdout, value); err != nil {
			return networkError("OUTPUT_WRITE_FAILED", "Could not write command output.", err)
		}
		return nil
	}
	// A projection can replace the response envelope with a scalar or a new
	// object. Specialized renderers only understand the original envelope.
	if opts.JQ != "" || len(opts.Select) > 0 {
		cmd = nil
	}
	output := &outputErrorWriter{writer: a.Stdout}
	if err := renderHuman(output, value, cmd, a.Now()); err != nil {
		return err
	}
	if output.err != nil {
		return networkError("OUTPUT_WRITE_FAILED", "Could not write command output.", output.err)
	}
	if cmd != nil && cmd.Supports.Pagination && !opts.Quiet {
		if envelope, ok := value.(map[string]any); ok && envelope != nil {
			if token, _ := envelope["next_page_token"].(string); token != "" {
				fmt.Fprintf(a.Stderr, "More results are available. Run the same command with --page-token %s for the next page, or --all for every page.\n", terminalSafe(token))
			}
		}
	}
	return nil
}

// Human renderers make several writes. Preserve the first failure and stop
// writing so a later successful write cannot hide a truncated result.
type outputErrorWriter struct {
	writer io.Writer
	err    error
}

func (w *outputErrorWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

func renderHuman(w io.Writer, value any, cmd *Command, now time.Time) *CLIError {
	if cmd != nil && cmd.CanonicalName == "auth.login" {
		envelope, _ := value.(map[string]any)
		data, _ := envelope["data"].(map[string]any)
		if auth, ok := data["active_context"].(AuthContext); ok {
			fmt.Fprintln(w, "Already authenticated.")
			if auth.Name != "" {
				fmt.Fprintln(w, "Context:", terminalSafe(auth.Name))
			}
			fmt.Fprintln(w, "Environment:", strings.ToUpper(terminalSafe(auth.Environment)))
			fmt.Fprintln(w, "Merchant:", terminalSafe(auth.MerchantID))
			if auth.SandboxID != "" {
				fmt.Fprintln(w, "Sandbox:", terminalSafe(auth.SandboxID))
			}
			if auth.ContextID != "" {
				fmt.Fprintln(w, "Context ID:", terminalSafe(auth.ContextID))
			}
			if next, ok := data["next"].(string); ok && next != "" {
				fmt.Fprintln(w, "Next:", terminalSafe(next))
			}
			return nil
		}
	}
	if cmd != nil && cmd.CanonicalName == "context.list" {
		envelope, _ := value.(map[string]any)
		list, ok := envelope["data"].(contextList)
		if ok {
			if len(list.Contexts) == 0 {
				fmt.Fprintln(w, "No authorized contexts. Run flint reauth.")
				return nil
			}
			for _, item := range list.Contexts {
				marker := " "
				if item.ID == list.Active {
					marker = "*"
				}
				fmt.Fprintf(w, "%s %s  %s / %s [%s]\n", marker, terminalSafe(item.ID), terminalSafe(item.Name), terminalSafe(item.MerchantID), strings.ToUpper(item.Environment))
			}
			fmt.Fprintln(w, "Run flint context switch to select a context.")
			return nil
		}
	}
	if cmd != nil && cmd.CanonicalName == "context.switch" {
		envelope, _ := value.(map[string]any)
		data, _ := envelope["data"].(map[string]any)
		item, ok := data["active_context"].(*authorizedContext)
		if ok {
			fmt.Fprintf(w, "Active context: %s / %s [%s] (%s)\n", terminalSafe(item.Name), terminalSafe(item.MerchantID), strings.ToUpper(item.Environment), terminalSafe(item.ID))
			return nil
		}
	}

	if cmd != nil && cmd.CanonicalName == "doctor" {
		renderDoctorHuman(w, value)
		return nil
	}
	if cmd != nil && cmd.Render == "checkout" {
		renderCheckoutHuman(w, value, now)
		return nil
	}
	if cmd != nil && cmd.Render == "help_search" {
		renderHelpSearchHuman(w, value, now)
		return nil
	}
	if cmd != nil && cmd.Render == "support_open" {
		renderSupportOpenHuman(w, value)
		return nil
	}
	if cmd != nil && cmd.Render == "request_log" {
		for _, path := range []string{"data.recommended_action", "data.retry", "data.error_category"} {
			if v, ok := lookupPath(value, path); ok {
				fmt.Fprintf(w, "%-20s %v\n", humanLabel(path), v)
			}
		}
	}
	// Specialized renderers above consume their original typed payloads.
	// Normalize only the generic fallback so structs and typed collections
	// render as named fields instead of Go debug representations.
	var err *CLIError
	value, err = normalizeOutput(value)
	if err != nil {
		return err
	}
	m, ok := value.(map[string]any)
	if !ok {
		fmt.Fprintln(w, humanScalar(value))
		return nil
	}
	data := m["data"]
	if data == nil {
		data = value
	}
	renderHumanValue(w, data, "", now, 0)
	return nil
}

func renderDoctorHuman(w io.Writer, value any) {
	envelope, _ := value.(map[string]any)
	checks, _ := envelope["data"].([]map[string]any)
	for _, check := range checks {
		marker := "✓"
		if check["status"] == "fail" {
			marker = "✗"
		} else if check["status"] == "info" {
			marker = "i"
		} else if check["status"] == "warning" {
			marker = "!"
		}
		label := humanLabel(fmt.Sprint(check["name"]))
		if check["name"] == "config" && check["status"] == "pass" {
			label = fmt.Sprintf("Config loaded (profile: %v)", check["profile"])
		} else if check["name"] == "credential" && check["status"] == "pass" {
			label = fmt.Sprintf("Credential found (source: %v); API validation follows", check["source"])
		}
		fmt.Fprintf(w, "%s %s\n", marker, label)
		keys := make([]string, 0, len(check))
		for key := range check {
			if key != "fix" && key != "name" && key != "status" && key != "profile" && key != "source" && !(key == "note" && check["name"] == "credential" && check["status"] == "pass") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		if _, ok := check["fix"]; ok {
			keys = append(keys, "fix")
		}
		for _, key := range keys {
			label := humanLabel(key)
			if key == "fix" {
				label = "Next"
			}
			fmt.Fprintf(w, "  %s: %v\n", label, check[key])
		}
	}
}

func renderCheckoutHuman(w io.Writer, value any, now time.Time) {
	if url, ok := firstStringAt(value, "data.url", "data.checkout_session.url", "data.checkout_session.checkout_url"); ok {
		fmt.Fprintln(w, url)
	}
	m, ok := value.(map[string]any)
	if !ok {
		return
	}
	data, ok := m["data"].(map[string]any)
	if !ok {
		return
	}
	remaining := maps.Clone(data)
	delete(remaining, "url")
	delete(remaining, "checkout_url")
	if checkoutSession, ok := remaining["checkout_session"].(map[string]any); ok {
		checkoutSession = maps.Clone(checkoutSession)
		delete(checkoutSession, "url")
		delete(checkoutSession, "checkout_url")
		remaining["checkout_session"] = checkoutSession
	}
	renderHumanValue(w, remaining, "", now, 0)
}

func renderHumanValue(w io.Writer, value any, prefix string, now time.Time, depth int) {
	indent := strings.Repeat("  ", depth)
	if depth > 12 {
		fmt.Fprintln(w, indent+"…")
		return
	}
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			fmt.Fprintln(w, indent+"(none)")
			return
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			x := v[key]
			switch x.(type) {
			case map[string]any, []any:
				fmt.Fprintln(w, indent+humanLabel(key))
				renderHumanValue(w, x, key, now, depth+1)
			default:
				fmt.Fprintf(w, "%s%-20s %s\n", indent, humanLabel(key), formatHumanScalar(x, now))
			}
		}
	case []any:
		if len(v) == 0 {
			fmt.Fprintln(w, indent+"(none)")
			return
		}
		parts := make([]string, 0, len(v))
		for _, x := range v {
			switch x.(type) {
			case map[string]any, []any:
				parts = nil
			default:
				if parts != nil {
					parts = append(parts, formatHumanScalar(x, now))
				}
			}
		}
		if parts != nil {
			fmt.Fprintln(w, indent+strings.Join(parts, ", "))
			return
		}
		for i, x := range v {
			if i > 0 {
				fmt.Fprintln(w)
			}
			renderHumanValue(w, x, prefix, now, depth+1)
		}
	default:
		fmt.Fprintln(w, indent+formatHumanScalar(v, now))
	}
}
func humanLabel(path string) string {
	parts := strings.Split(path, ".")
	words := strings.Fields(strings.ReplaceAll(parts[len(parts)-1], "_", " "))
	if len(words) == 0 {
		if path != "" {
			return path
		}
		return "(empty key)"
	}
	for i, word := range words {
		switch strings.ToLower(word) {
		case "id", "api", "cli", "url":
			words[i] = strings.ToUpper(word)
		default:
			if i == 0 {
				first, size := utf8.DecodeRuneInString(word)
				words[i] = string(unicode.ToUpper(first)) + word[size:]
			}
		}
	}
	return strings.Join(words, " ")
}
func humanScalar(v any) string { return fmt.Sprint(v) }
func formatHumanScalar(v any, now time.Time) string {
	if s, ok := v.(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Local().Format("2006-01-02 15:04:05 MST") + " (" + relativeAge(now.Sub(t)) + ")"
		}
		return s
	}
	if v == nil {
		return "-"
	}
	return fmt.Sprint(v)
}

// relativeAge describes how far t is from now: "5m ago" for the past and
// "in 8d" for the future, such as an evidence deadline.
func relativeAge(d time.Duration) string {
	future := d < 0
	if future {
		d = -d
	}
	var span string
	switch {
	case d < time.Minute:
		span = fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		span = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		span = fmt.Sprintf("%dh", int(d.Hours()))
	default:
		span = fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if future {
		return "in " + span
	}
	return span + " ago"
}
func firstStringAt(v any, paths ...string) (string, bool) {
	for _, p := range paths {
		if x, ok := lookupPath(v, p); ok {
			if s, ok := x.(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

func lookupPath(value any, path string) (any, bool) {
	cur := value
	for _, part := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			var ok bool
			cur, ok = v[part]
			if !ok {
				return nil, false
			}
		case []any:
			var index int
			if _, err := fmt.Sscanf(part, "%d", &index); err != nil || index < 0 || index >= len(v) {
				return nil, false
			}
			cur = v[index]
		default:
			return nil, false
		}
	}
	return cur, true
}
func selectFields(value any, fields []string) (any, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("--select requires an object envelope")
	}
	data, hasData := m["data"]
	if resources, ok := data.([]any); ok {
		for _, field := range fields {
			found := false
			for _, resource := range resources {
				if _, ok := lookupPath(resource, field); ok {
					found = true
					break
				}
			}
			if !found && len(resources) > 0 {
				return nil, fmt.Errorf("selected field does not exist: %s", field)
			}
		}
		projected := make([]any, 0, len(resources))
		for _, resource := range resources {
			resourceMap, ok := resource.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("--select requires object resources")
			}
			resourceMap = copyMap(resourceMap)
			for _, field := range fields {
				if _, ok := lookupPath(resourceMap, field); !ok {
					setNested(resourceMap, strings.Split(field, "."), nil)
				}
			}
			item, err := projectObject(resourceMap, fields)
			if err != nil {
				return nil, err
			}
			projected = append(projected, item)
		}
		out := copyMap(m)
		out["data"] = projected
		return out, nil
	}
	if dataMap, ok := data.(map[string]any); ok {
		if projected, err := projectObject(dataMap, fields); err == nil {
			out := copyMap(m)
			out["data"] = projected
			return out, nil
		}
		for key, nested := range dataMap {
			if nestedMap, ok := nested.(map[string]any); ok {
				projected, err := projectObject(nestedMap, fields)
				if err == nil {
					out := copyMap(m)
					newData := copyMap(dataMap)
					newData[key] = projected
					out["data"] = newData
					return out, nil
				}
			}
		}
	}
	if !hasData {
		return projectObject(m, fields)
	}
	return nil, fmt.Errorf("selected fields do not exist on returned resources")
}

func projectObject(m map[string]any, fields []string) (map[string]any, error) {
	out := map[string]any{}
	for _, field := range fields {
		v, ok := lookupPath(m, field)
		if !ok {
			return nil, fmt.Errorf("selected field does not exist: %s", field)
		}
		setNested(out, strings.Split(field, "."), v)
	}
	return out, nil
}

func copyMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}
func setNested(root map[string]any, parts []string, value any) {
	cur := root
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = value
}
