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

func (b *mcpCappedBuffer) Write(value []byte) (int, error) {
	if b.Len()+len(value) > b.limit {
		b.exceeded = true
		return 0, fmt.Errorf("MCP command output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(value)
}

// mcpPolicy holds the safety choices the human made when starting the server.
// Agents can narrow, but never widen, what these flags allow.
type mcpCallResult struct {
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
	method, _ := args["method"].(string)
	if cmd.Mutation || (cmd.CanonicalName == "api" && !strings.EqualFold(method, http.MethodGet)) {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) <= mcpMaxResultBytes {
		return nil
	}
	message := fmt.Sprintf("The result is %d bytes, over this server's %d byte limit for one tool result, so it was not returned. Narrow it and call again: keep only the fields you need with select, use page_size and page_token instead of all, or add filter arguments.", len(raw), mcpMaxResultBytes)
	return mcpToolError(message, map[string]any{"error": map[string]any{"type": errorTypeUsage, "code": "RESULT_TOO_LARGE", "message": message, "details": map[string]any{"bytes": len(raw), "limit": mcpMaxResultBytes}}})
}

// mcpWithWarnings adds CLI warnings to a successful result as their own text
// block, after the result and the environment line.
func mcpWithWarnings(result map[string]any, warnings string) map[string]any {
	if warnings = strings.TrimSpace(warnings); warnings != "" {
		content, _ := result["content"].([]map[string]any)
		result["content"] = append(content, map[string]any{"type": "text", "text": "Warnings:\n" + warnings})
	}
	return result
}

// runMCPTool runs a tool, applies the live and confirmation gates, and turns
// the outcome into an MCP tool result.
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

func mcpToolResult(value any, structured, transformed bool, environment string) map[string]any {
	structuredValue := value
	if transformed {
		structuredValue = map[string]any{"value": value}
	}
	raw, _ := json.Marshal(structuredValue)
	content := []map[string]any{{"type": "text", "text": string(raw)}}
	result := map[string]any{"content": content}
	if environment != "" {
		result["content"] = append(content, map[string]any{"type": "text", "text": "Flint environment: " + environment})
		result["_meta"] = map[string]any{mcpEnvironmentMetaKey: environment}
	}
	if structured {
		result["structuredContent"] = structuredValue
	}
	return result
}

// mcpOutputSchema is the public output contract of a tool's structured result.
// tools/list does not advertise it (every tool would embed its full component
// closure); agents fetch it on demand with the schema.output tool, and tests
// check results against it.
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

// mcpToolError leads with the human-readable message; details follow as JSON.
