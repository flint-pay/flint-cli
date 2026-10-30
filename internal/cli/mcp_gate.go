package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func mcpGateViolation(arguments map[string]any, policy mcpPolicy) string {
	controls, _ := arguments["_flint"].(map[string]any)
	if _, ok := controls["live"]; ok && !policy.allowLive {
		return "live is not accepted: this MCP server was started without --allow-live, so agents cannot act on live (production) data. Do not retry. If the user wants live access, they must restart the server with flint mcp serve --allow-live."
	}
	if _, ok := controls["confirm"]; ok && !policy.allowDestructive {
		return "confirm is not accepted: on this MCP server only the user can confirm destructive or live-sensitive operations. Call the tool without confirm; the server will ask the user when the client supports elicitation. Otherwise the user can restart the server with flint mcp serve --allow-destructive."
	}
	return ""
}

// mcpSelectionSources says where the human set each server selection.
func (a *App) mcpSelectionViolation(arguments map[string]any, parent Options) string {
	controls, _ := arguments["_flint"].(map[string]any)
	profile, hasProfile := controls["profile"].(string)
	merchant, hasMerchant := controls["merchant"].(string)
	if !hasProfile && !hasMerchant {
		return ""
	}
	base, _, err := a.resolveConfig(parent)
	if err != nil {
		return "_flint.profile and _flint.merchant are not accepted because this MCP server's own Flint configuration does not resolve (" + err.Error() + "). Do not retry; ask the user to fix the server's configuration and restart it."
	}
	candidate := parent
	if hasProfile {
		candidate.Profile = profile
	}
	if hasMerchant {
		candidate.Merchant = merchant
	}
	next, _, err := a.resolveConfig(candidate)
	if err != nil {
		return "" // The command reports the invalid selection itself.
	}
	pinned := func(control, setting, value, source string) string {
		return "_flint." + control + " is not accepted: this MCP server uses " + setting + " " + value + ", set by " + mcpSelectionSources[source] + ", and agents cannot replace it. Do not retry with a different value; omit _flint." + control + ". If the user wants another " + setting + ", they must restart the server with it."
	}
	switch source := base.Sources["profile"]; {
	case hasProfile && next.ProfileName != base.ProfileName && (source == "flag" || source == "environment" || source == "project"):
		return pinned("profile", "profile", base.ProfileName, source)
	case base.MerchantGuard != "" && next.MerchantGuard != base.MerchantGuard:
		control := "merchant"
		if !hasMerchant {
			control = "profile"
		}
		return pinned(control, "merchant guard", base.MerchantGuard, base.Sources["merchant_guard"])
	case base.ContextID != "" && next.ContextID != base.ContextID:
		return pinned("profile", "context", base.ContextID, base.Sources["context"])
	}
	return ""
}

// A scalar query value is shorthand for a one-element array.
func mcpConfirmationRequired(value any) bool {
	code := mcpErrorField(value, "code")
	return code == "CONFIRMATION_REQUIRED" || code == "PRODUCTION_CONFIRMATION_REQUIRED" || code == "UNVERIFIED_ENVIRONMENT_CONFIRMATION_REQUIRED"
}

func mcpConfirmationUnavailable(cmd *Command, cliMessage string) string {
	message := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cliMessage), "Re-run with --confirm."))
	if message == "" {
		message = "This operation requires confirmation."
	}
	return message + " Only the user can confirm it on this MCP server, and this MCP client does not support elicitation, so the server cannot ask them. Do not retry. Ask the user to run it themselves with the flint CLI (flint " + strings.Join(cmd.Path[1:], " ") + " ... --confirm) or to restart the server with flint mcp serve --allow-destructive. Use dry_run: true, where available, to show them the effect first."
}

func (a *App) mcpAskConfirmation(ctx context.Context, session *mcpSession, cmd *Command, args map[string]any, outcome mcpCallResult) (bool, string) {
	environment := outcome.environment
	if environment == "" {
		environment = "unknown"
	}
	cliMessage := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(mcpErrorText(outcome.value)), "Re-run with --confirm."))
	rawArgs, _ := json.Marshal(args)
	if len(rawArgs) > 2000 {
		rawArgs = append(rawArgs[:2000], []byte("...")...)
	}
	prompt := fmt.Sprintf("An AI agent wants to run the Flint tool %s (environment: %s). %s\n\nArguments: %s\n\nAllow this operation?", mcpToolName(cmd.CanonicalName), environment, cliMessage, rawArgs)
	requestCtx, cancel := context.WithTimeout(ctx, mcpConfirmationDeadline)
	defer cancel()
	response, err := session.request(requestCtx, "elicitation/create", map[string]any{
		"message": prompt,
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{"type": "boolean", "title": "Allow " + mcpToolName(cmd.CanonicalName), "description": "Approve this " + environment + " operation.", "default": false},
			},
			"required": []string{"confirm"},
		},
	})
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return false, "The user did not answer the confirmation request in time; the operation was not performed."
		}
		return false, "Could not ask the user to confirm this operation (" + err.Error() + "); it was not performed."
	}
	if rpcErr, ok := response["error"].(map[string]any); ok {
		message, _ := rpcErr["message"].(string)
		return false, "The MCP client could not ask the user to confirm this operation (" + message + "); it was not performed. The user can run it with the flint CLI or restart the server with --allow-destructive."
	}
	result, _ := response["result"].(map[string]any)
	action, _ := result["action"].(string)
	content, _ := result["content"].(map[string]any)
	if action == "accept" && content["confirm"] == true {
		return true, ""
	}
	return false, "The user declined to confirm " + mcpToolName(cmd.CanonicalName) + "; the operation was not performed. Do not retry unless the user asks."
}

// mcpEnvironmentObserver sees the child command's HTTP traffic. It records the
// credential environment from the auth-context check and, unless live access
// was allowed, refuses every later authenticated request on a live credential.
