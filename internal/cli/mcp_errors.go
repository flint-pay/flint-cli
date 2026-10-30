package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

func mcpErrorField(value any, field string) string {
	envelope, _ := value.(map[string]any)
	switch e := envelope["error"].(type) {
	case map[string]any:
		text, _ := e[field].(string)
		return text
	case string:
		if field == "message" {
			return e
		}
	}
	if field == "message" {
		if stderr, _ := envelope["stderr"].(string); stderr != "" {
			return stderr
		}
	}
	return ""
}

func mcpErrorText(value any) string { return mcpErrorField(value, "message") }

func mcpCLIErrorEnvelope(err *CLIError) map[string]any {
	var stdout bytes.Buffer
	(&App{Stdout: &stdout}).writeError(err, Options{Output: "json"})
	var envelope map[string]any
	_ = json.Unmarshal(stdout.Bytes(), &envelope)
	return envelope
}

var mcpControlFlags = map[string]string{
	"live": "live", "confirm": "confirm", "idempotency-key": "idempotency_key", "dry-run": "dry_run",
	"preview": "preview", "page-size": "page_size", "page-token": "page_token", "all": "all",
	"expand": "expand", "field": "field", "select": "select", "jq": "jq", "for": "for",
	"max-events": "max_events", "wait-for": "wait_for",
	"timeout": "timeout", "merchant": "merchant", "profile": "profile", "paginate": "paginate",
}

var mcpFlagPattern = regexp.MustCompile(`--([a-z][a-z0-9-]*)(=client)?`)

// mcpTranslateRemediation rewrites CLI flag advice into the tool's own terms:
// _flint controls, tool arguments, or guidance for the human running the server.
func mcpTranslateRemediation(message string, cmd *Command, policy mcpPolicy) string {
	if strings.Contains(message, "Re-run with --confirm.") {
		replacement := "Re-run with confirm set to true, after checking the effect with dry_run: true where available."
		if !policy.allowDestructive {
			replacement = "Only the user can confirm it on this MCP server."
		}
		message = strings.ReplaceAll(message, "Re-run with --confirm.", replacement)
	}
	arguments := map[string]string{}
	if cmd != nil {
		for _, arg := range cmd.Arguments {
			if arg.Flag != "" {
				arguments[strings.TrimPrefix(arg.Flag, "--")] = arg.Name
			}
		}
	}
	return mcpFlagPattern.ReplaceAllStringFunc(message, func(match string) string {
		parts := mcpFlagPattern.FindStringSubmatch(match)
		name := parts[1]
		if argument, ok := arguments[name]; ok {
			return "the " + argument + " argument"
		}
		control, ok := mcpControlFlags[name]
		if !ok || (name == "live" && !policy.allowLive) || (name == "confirm" && !policy.allowDestructive) {
			return match
		}
		if parts[2] != "" {
			return control + ": true"
		}
		return control
	})
}

// mcpCredentialMessages replace CLI sign-in advice, which an MCP client
// configuration cannot follow, for errors about a missing credential.
func mcpTranslateDetails(value any, cmd *Command, policy mcpPolicy) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if key == "command" || key == "url" || key == "idempotency_key" {
				out[key] = item
				continue
			}
			out[key] = mcpTranslateDetails(item, cmd, policy)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = mcpTranslateDetails(item, cmd, policy)
		}
		return out
	case []map[string]any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = mcpTranslateDetails(item, cmd, policy)
		}
		return out
	case string:
		return mcpTranslateRemediation(typed, cmd, policy)
	}
	return value
}

func mcpCommandError(cmd *Command, outcome mcpCallResult, policy mcpPolicy, override string) map[string]any {
	value := outcome.value
	message := override
	if message == "" {
		message = mcpTranslateRemediation(mcpErrorText(value), cmd, policy)
	}
	code := mcpErrorField(value, "code")
	if replacement, ok := mcpCredentialMessages[code]; ok && override == "" {
		message = replacement + mcpCredentialRemedy
	}
	if envelope, ok := value.(map[string]any); ok {
		if e, ok := envelope["error"].(map[string]any); ok {
			copyEnvelope := copyMap(envelope)
			copyError := copyMap(e)
			if remediation, has := copyError["remediation"]; has {
				copyError["remediation"] = mcpTranslateDetails(remediation, cmd, policy)
			}
			if _, credential := mcpCredentialMessages[code]; credential {
				copyError["remediation"] = map[string]any{"next_actions": []any{map[string]any{"reason_message": strings.TrimSpace(mcpCredentialRemedy)}}}
			}
			if details, has := copyError["details"]; has {
				translated := mcpTranslateDetails(details, cmd, policy)
				if _, credential := mcpCredentialMessages[code]; credential {
					if detailMap, ok := translated.(map[string]any); ok {
						detailMap["remediation"] = map[string]any{"next_actions": []any{map[string]any{"reason_message": strings.TrimSpace(mcpCredentialRemedy)}}}
					}
				}
				copyError["details"] = translated
				if detailMap, ok := translated.(map[string]any); ok && commandIdempotencyClass(cmd) != "none" && !strings.Contains(message, "idempotency_key") {
					if key, ok := detailMap["idempotency_key"].(string); ok && key != "" {
						advice := idempotencyRetryAdvice(remediationRetry(copyError["remediation"]), key)
						message = strings.TrimSpace(message + " " + mcpTranslateRemediation(advice, cmd, policy))
					}
				}
			}
			if _, has := copyError["message"]; has {
				copyError["message"] = message
			}
			copyEnvelope["error"] = copyError
			value = copyEnvelope
		}
	}
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("Flint command failed with exit code %d.", outcome.exit)
	}
	details := map[string]any{"exit_code": outcome.exit}
	if outcome.environment != "" {
		details["environment"] = outcome.environment
	}
	if value != nil {
		details["result"] = value
	}
	return mcpToolError(message, details)
}

// mcpEnvironmentMetaKey names the credential environment in result _meta.
func mcpToolError(message string, details any) map[string]any {
	text := message
	if details != nil {
		raw, _ := json.Marshal(details)
		text += "\n\n" + string(raw)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true}
}
