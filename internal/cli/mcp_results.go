package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type mcpCappedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

// A successful MCP write must retain the server's complete response.
type mcpFullWriteResponseKey struct{}

func (b *mcpCappedBuffer) Write(value []byte) (int, error) {
	if b.limit > 0 && b.Len()+len(value) > b.limit {
		b.exceeded = true
		return 0, fmt.Errorf("MCP command output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(value)
}

// mcpCallResult retains command data separately from its presentation notes.
type mcpCallResult struct {
	cliError    *CLIError
	value       any
	transformed bool
	exit        int
	environment string
	liveBlocked bool
	warnings    string // stderr of a successful command, minus progress and debug lines
}

// mcpResultTooLarge refuses an oversized read result with advice to narrow
// it. Writes are never refused here: their result reports a completed change.
func mcpResultTooLarge(cmd *Command, args map[string]any, value any) map[string]any {
	return (&App{}).mcpResultTooLarge(cmd, args, Options{}, mcpPolicy{}, "", value)
}

func (a *App) mcpResultTooLarge(cmd *Command, args map[string]any, opts Options, policy mcpPolicy, environment string, value any) map[string]any {
	if mcpResultIsWrite(cmd, args) {
		return nil
	}
	raw, err := mcpCompactJSON(value)
	if err != nil || len(raw) <= mcpMaxResultBytes {
		return nil
	}
	e := mcpResultSizeError(len(raw), 0)
	return a.mcpLocalError(cmd, args, opts, policy, e, environment)
}

func mcpResultIsWrite(cmd *Command, args map[string]any) bool {
	if cmd == nil {
		return false
	}
	method, _ := args["method"].(string)
	return cmd.Mutation || cmd.Destructive || cmd.Stream || cmd.CanonicalName == "signup" || cmd.CanonicalName == "trigger" || (cmd.CanonicalName == "api" && !strings.EqualFold(method, http.MethodGet))
}

func mcpResultSizeError(size, pages int) *CLIError {
	message := fmt.Sprintf("The result is %d bytes, over this server's %d byte limit for one tool result, so it was not returned. Narrow it with select, a smaller page_size, or filters; use page_token instead of all.", size, mcpMaxResultBytes)
	e := usageError("RESULT_TOO_LARGE", message, "")
	details := map[string]any{"bytes": size, "limit": mcpMaxResultBytes, "remediation": map[string]any{"retry": "none", "next_actions": []any{
		map[string]any{"reason_message": "Use select to return only the fields you need."},
		map[string]any{"reason_message": "Use a smaller page_size and page_token instead of all, or add filters."},
	}}}
	if pages > 0 {
		details["pages_fetched"] = pages
	}
	e.Details = details
	return e
}

// mcpWithWarnings folds CLI notes and warnings into the single notes block.
func mcpWithWarnings(result map[string]any, warnings string) map[string]any {
	content, _ := result["content"].([]map[string]any)
	var lines []string
	if len(content) > 1 {
		text, _ := content[1]["text"].(string)
		lines = append(lines, strings.TrimSpace(text))
	}
	for _, line := range strings.Split(warnings, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(strings.ToLower(line), "note:"):
			line = "Note: " + strings.TrimSpace(line[len("note:"):])
		case strings.HasPrefix(strings.ToLower(line), "warning:"):
			line = "Warning: " + strings.TrimSpace(line[len("warning:"):])
		default:
			line = "Warning: " + line
		}
		lines = append(lines, line)
	}
	if len(lines) > 0 {
		result["content"] = append(content[:1], map[string]any{"type": "text", "text": strings.Join(lines, "\n")})
	}
	return result
}

func mcpWithEnvironment(result map[string]any, environment string) map[string]any {
	if environment != "" {
		content, _ := result["content"].([]map[string]any)
		result["content"] = append(content[:1], map[string]any{"type": "text", "text": "Flint environment: " + environment})
		result["_meta"] = map[string]any{mcpEnvironmentMetaKey: environment}
	}
	return result
}

func mcpCompactJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func (w *mcpStderr) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		index := bytes.IndexByte(w.partial, '\n')
		if index < 0 {
			break
		}
		w.line(w.partial[:index])
		w.partial = w.partial[index+1:]
	}
	return len(p), nil
}

func (w *mcpLineNotifier) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	for _, b := range p[:n] {
		if b == '\n' {
			w.records++
			w.progress.notify(fmt.Sprintf("Received %d stream records", w.records))
		}
	}
	return n, err
}

func mcpToolResult(value any, _, _ bool, environment string) map[string]any {
	raw, _ := mcpCompactJSON(value)
	content := []map[string]any{{"type": "text", "text": string(raw)}}
	result := map[string]any{"content": content}
	return mcpWithEnvironment(result, environment)
}

// mcpOutputSchema describes legacy command envelopes for contract fixtures.
// Tools do not advertise an output schema or return structuredContent.
func mcpOutputSchema(cmd *Command) (map[string]any, *CLIError) {
	outputSchema, outputErr := schemaForCommand(cmd, false)
	if outputErr != nil {
		return nil, outputErr
	}
	if cmd.Stream {
		stream := mcpStreamOutputSchema()
		if cmd.CanonicalName == "listen" {
			return map[string]any{"type": "object", "anyOf": []any{stream, map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"data"},
				"properties": map[string]any{"data": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"signing_secret", "source"},
					"properties": map[string]any{"signing_secret": map[string]any{"type": "string"}, "source": map[string]any{"type": "string"}},
				}},
			}}}, nil
		}
		return stream, nil
	}
	variants := []any{outputSchema}
	if cmd.Supports.DryRunClient || cmd.Destructive {
		variants = append(variants, mcpPreviewOutputSchema())
	}
	if cmd.Supports.Field && cmd.Supports.Select && cmd.Supports.JQ {
		variants = append(variants, map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{"value": map[string]any{}},
			"required":             []string{"value"},
		})
	}
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"anyOf":   variants,
	}, nil
}

// Client dry runs and destructive previews return the prepared request instead
// of the API resource. Keep that envelope explicit in the tool's output union.
func mcpPreviewOutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"data"},
		"properties": map[string]any{
			"data": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"method", "path", "headers", "body", "persistent_side_effects"},
				"properties": map[string]any{
					"method": map[string]any{"type": "string"},
					"path":   map[string]any{"type": "string"},
					"headers": map[string]any{
						"type":       "object",
						"properties": map[string]any{"Idempotency-Key": map[string]any{"type": "string"}},
					},
					"body":                    map[string]any{},
					"persistent_side_effects": map[string]any{"const": false},
					"warnings":                map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
					"preview": map[string]any{
						"type":     "object",
						"required": []string{"permission_check", "reversibility", "affected_resources"},
						"properties": map[string]any{
							"permission_check":   map[string]any{"type": "object"},
							"reversibility":      map[string]any{"type": "object"},
							"affected_resources": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
						},
					},
				},
			},
		},
	}
}

func mcpStreamOutputSchema() map[string]any {
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"records": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": true,
					"required":             []string{"type"},
					"properties": map[string]any{
						"type": map[string]any{"type": "string", "enum": []string{"version", "event", "listener", "ready", "gap", "withheld", "forward", "reconnect", "disconnect", "checkpoint"}},
					},
				},
			},
		},
		"required": []string{"records"},
	}
}
