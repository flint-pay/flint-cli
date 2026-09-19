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
		_ = writeJSON(a.Stdout, body)
		return
	}
	fmt.Fprintln(a.Stderr, err.Message)
	if err.RequestID != "" {
		fmt.Fprintf(a.Stderr, "Request ID: %s\n", err.RequestID)
	}
	if details, ok := err.Details.(map[string]any); ok {
		if remediation, ok := details["remediation"].(map[string]any); ok {
			renderNextActions(a.Stderr, remediation["next_actions"])
		}
	}
	if err.Details != nil && strings.HasPrefix(err.Code, "UNKNOWN_") {
		if list, ok := err.Details.([]map[string]any); ok && len(list) > 0 {
			if s, ok := list[0]["suggestions"].([]string); ok && len(s) > 0 {
				fmt.Fprintln(a.Stderr, "Did you mean: "+strings.Join(s, ", "))
			}
		}
	}
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
		outputErr := cliError(ExitSoftware, "internal_error", "OUTPUT_ENCODING_FAILED", "Could not encode command output.")
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
				return nil, networkError("REQUEST_CANCELED", "Output transformation was canceled.", err)
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
			value = values
		}
	}
	return value, nil
}

func (a *App) writeResult(value any, cmd *Command, opts Options) *CLIError {
	value, err := applyOutputTransforms(a.commandContext(), value, opts)
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
		for _, path := range []string{"data.recommended_action", "data.retryable", "data.error_category"} {
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
	if url, ok := firstStringAt(value, "data.hosted_checkout.url", "data.url", "data.checkout_session.url", "data.checkout_session.checkout_url"); ok {
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
	if hosted, ok := remaining["hosted_checkout"].(map[string]any); ok {
		hosted = maps.Clone(hosted)
		delete(hosted, "url")
		remaining["hosted_checkout"] = hosted
	}
	if checkoutSession, ok := remaining["checkout_session"].(map[string]any); ok {
		checkoutSession = maps.Clone(checkoutSession)
		delete(checkoutSession, "url")
		delete(checkoutSession, "checkout_url")
		remaining["checkout_session"] = checkoutSession
	}
	renderHumanValue(w, remaining, "", now, 0)
}

func renderHumanValue(w io.Writer, value any, prefix string, now time.Time, depth int) {
	if depth > 3 {
		return
	}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			x := v[k]
			switch x.(type) {
			case map[string]any, []any:
				fmt.Fprintln(w, humanLabel(k))
				renderHumanValue(w, x, k, now, depth+1)
			default:
				fmt.Fprintf(w, "%-20s %s\n", humanLabel(k), formatHumanScalar(x, now))
			}
		}
	case []any:
		for i, x := range v {
			if i > 0 {
				fmt.Fprintln(w)
			}
			renderHumanValue(w, x, prefix, now, depth+1)
		}
	default:
		fmt.Fprintln(w, formatHumanScalar(v, now))
	}
}
func humanLabel(path string) string {
	parts := strings.Split(path, ".")
	s := strings.ReplaceAll(parts[len(parts)-1], "_", " ")
	if s == "" {
		if path != "" {
			return path
		}
		return "(empty key)"
	}
	first, size := utf8.DecodeRuneInString(s)
	label := string(unicode.ToUpper(first)) + s[size:]
	if label == "Id" {
		return "ID"
	}
	if strings.HasSuffix(label, " id") {
		return strings.TrimSuffix(label, " id") + " ID"
	}
	return label
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
func relativeAge(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
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
		projected := make([]any, 0, len(resources))
		for _, resource := range resources {
			resourceMap, ok := resource.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("--select requires object resources")
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
