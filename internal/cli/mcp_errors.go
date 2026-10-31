package cli

import (
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

// mcpCLIErrorEnvelope is also used by compatibility callers without command context.
// It deliberately does not use the terminal writer, whose diagnostics alter errors.
func mcpCLIErrorEnvelope(err *CLIError) map[string]any {
	return (&App{}).mcpErrorEnvelope(nil, nil, Options{}, mcpPolicy{}, err, "")
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

// mcpCommandError keeps the old internal helper available to callers that do not
// have an App. Execution paths use the method so actions retain server context.
func mcpCommandError(cmd *Command, outcome mcpCallResult, policy mcpPolicy, override string) map[string]any {
	return (&App{}).mcpCommandError(cmd, nil, Options{}, outcome, policy, override)
}

func (a *App) mcpCommandError(cmd *Command, args map[string]any, opts Options, outcome mcpCallResult, policy mcpPolicy, override string) map[string]any {
	e := outcome.cliError
	if e == nil {
		e = mcpErrorFromValue(outcome.value, outcome.exit)
	}
	if override != "" && e.apiError == nil {
		copy := *e
		copy.Message = override
		if copy.Type == errorTypeConfirmationRequired {
			copy.Code = "CONFIRMATION_NEEDS_USER"
		}
		e = &copy
	}
	return a.mcpLocalError(cmd, args, opts, policy, e, outcome.environment)
}

func (a *App) mcpLocalError(cmd *Command, args map[string]any, opts Options, policy mcpPolicy, e *CLIError, environment string) map[string]any {
	envelope := a.mcpErrorEnvelope(cmd, args, opts, policy, e, environment)
	raw, _ := mcpCompactJSON(envelope)
	result := map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(raw)}}, "isError": true,
	}
	if environment != "" {
		result["_meta"] = map[string]any{mcpEnvironmentMetaKey: environment}
	}
	return result
}

func (a *App) mcpErrorEnvelope(cmd *Command, args map[string]any, opts Options, policy mcpPolicy, e *CLIError, environment string) map[string]any {
	if e == nil {
		e = cliError(ExitSoftware, errorTypeInternal, "MCP_COMMAND_FAILED", "The Flint command failed without an error result.")
	}
	copy := *e
	e = &copy
	if e.apiError == nil {
		switch e.Code {
		case "PRODUCTION_CONFIRMATION_REQUIRED", "UNVERIFIED_ENVIRONMENT_CONFIRMATION_REQUIRED":
			e.Code = "CONFIRMATION_REQUIRED"
		case "INVALID_REQUEST":
			e.Type = errorTypeValidation
		case "CLIENT_TIMEOUT":
			e.Type = errorTypeTimeout
		}
	}
	origin := "cli"
	errorObject := mcpLocalErrorObject(e, cmd, policy)
	if e.apiError != nil {
		origin = "api"
		errorObject = cloneErrorJSON(e.apiError).(map[string]any)
	}
	meta := map[string]any{"origin": origin, "exit_code": e.ExitCode}
	if cmd != nil && cmd.CanonicalName != "" {
		meta["operation"] = cmd.CanonicalName
	}
	if environment != "" {
		meta["environment"] = environment
	}
	if mcpResultIsWrite(cmd, args) && mcpErrorUnknown(e) {
		meta["outcome"] = "unknown"
	}
	if key := mcpErrorKey(e); key != "" && e.requestSent && (cmd == nil || mcpResultIsWrite(cmd, args)) {
		meta["idempotency_key"] = key
	}
	actions := a.mcpNextActions(cmd, args, opts, policy, e, environment)
	if len(actions) == 0 {
		actions = []map[string]any{{"actor": "agent", "text": "Read the error guidance before acting again.", "call": map[string]any{"tool": "flint_help", "arguments": map[string]any{"topic": "errors"}}}}
	}
	meta["next_actions"] = actions
	return map[string]any{"error": errorObject, "cli": meta}
}

func mcpLocalErrorObject(e *CLIError, cmd *Command, policy mcpPolicy) map[string]any {
	message := mcpTranslateRemediation(e.Message, cmd, policy)
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("Flint command failed with exit code %d.", e.ExitCode)
	}
	object := map[string]any{
		"type": e.Type, "code": e.Code, "message": message, "error_source": "caller",
		"doc_url": "https://developers.withflintpay.com/docs/guides/cli#mcp-error-codes",
	}
	if e.Param != "" {
		object["param"] = e.Param
	}
	if e.Code == "INVALID_REQUEST" {
		object["doc_url"] = "https://developers.withflintpay.com/docs/errors#invalid_request"
	}
	var details []any
	switch raw := e.Details.(type) {
	case []any:
		details = cloneErrorJSON(raw).([]any)
	case []map[string]any:
		for _, detail := range raw {
			details = append(details, cloneErrorJSON(detail))
		}
	case map[string]any:
		detail := cloneErrorJSON(raw).(map[string]any)
		delete(detail, "remediation")
		details = []any{detail}
	}
	if len(details) == 0 {
		details = []any{map[string]any{"code": e.Code, "message": message}}
	}
	for _, raw := range details {
		if detail, ok := raw.(map[string]any); ok {
			delete(detail, "remediation")
			if _, exists := detail["code"]; !exists {
				detail["code"] = e.Code
			}
			if e.Code == "INVALID_REQUEST" {
				switch detail["code"] {
				case "REQUIRED", "INVALID_VALUE", "UNKNOWN_FIELD", "NOT_ALLOWED":
				default:
					text, _ := detail["message"].(string)
					if text == "" {
						text = message
						detail["message"] = text
					}
					text = strings.ToLower(text)
					detail["code"] = "INVALID_VALUE"
					if detail["reason"] == "missing_required" || strings.Contains(text, " is required") || strings.Contains(text, " are required") || strings.HasPrefix(text, "missing required ") {
						detail["code"] = "REQUIRED"
					}
				}
			}
			if e.Param != "" {
				if _, exists := detail["param"]; !exists {
					detail["param"] = e.Param
				}
			}
		}
	}
	object["details"] = mcpTranslateDetails(details, cmd, policy)
	next := "Follow cli.next_actions before calling again."
	if e.Code == "INVALID_REQUEST" {
		next = "Fix the arguments named by details[].param and call again."
	} else if diagnostics, ok := e.Details.(map[string]any); ok {
		if remediation, ok := diagnostics["remediation"].(map[string]any); ok {
			if text, ok := remediation["next_steps"].(string); ok && text != "" {
				next = mcpTranslateRemediation(text, cmd, policy)
			}
		}
		if diagnostics["outcome"] == "unknown" && e.requestSent {
			next = "Look up the result before acting again."
			if mcpResultIsWrite(cmd, nil) && commandIdempotencyClass(cmd) != "none" && mcpErrorKey(e) != "" {
				next += " If you call this write again, use the same idempotency_key."
			}
		}
	}
	object["remediation"] = map[string]any{"retry": "none", "next_steps": next}
	return object
}

// mcpErrorFromValue supports callers that have a value rather than the captured
// error. Child execution always retains CLIError and does not take this path.
func mcpErrorFromValue(value any, exit int) *CLIError {
	e := cliError(exit, errorTypeUsage, "MCP_COMMAND_FAILED", mcpErrorText(value))
	envelope, _ := value.(map[string]any)
	if raw, ok := envelope["error"].(map[string]any); ok {
		if typ, _ := raw["type"].(string); typ != "" {
			e.Type = typ
		}
		if code, _ := raw["code"].(string); code != "" {
			e.Code = code
		}
		e.Param, _ = raw["param"].(string)
		e.Details = raw["details"]
		meta, _ := envelope["cli"].(map[string]any)
		if meta["origin"] == "api" {
			e.apiError = cloneErrorJSON(raw).(map[string]any)
		}
		if meta["outcome"] == "unknown" {
			e.Details = map[string]any{"outcome": "unknown"}
		}
		if key, ok := meta["idempotency_key"].(string); ok {
			e.idempotencyKey, e.requestSent = key, true
		}
	}
	return e
}

// mcpToolError is for compatibility paths without command context. Every tool
// failure still has a single JSON text block with the same error contract.
func mcpToolError(message string, details any) map[string]any {
	e := mcpErrorFromValue(details, ExitUsage)
	if e.Message == "" {
		e.Message = message
	}
	return (&App{}).mcpLocalError(nil, nil, Options{}, mcpPolicy{}, e, "")
}
