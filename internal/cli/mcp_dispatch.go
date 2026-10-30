package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

var mcpReadControls = []string{"select", "all", "wait_for", "wait_seconds", "live"}
var mcpWriteControls = []string{"idempotency_key", "confirm", "dry_run", "select", "live"}

func (a *App) mcpDispatch(ctx context.Context, id any, tool string, input map[string]any, opts Options, policy mcpPolicy, structured bool) map[string]any {
	var errorCommand *Command
	errorArguments := input
	fail := func(e *CLIError) map[string]any {
		return rpcResult(id, a.mcpLocalError(errorCommand, errorArguments, opts, policy, e, a.mcpKnownEnvironment()))
	}
	if tool == "flint_search" || tool == "flint_describe" || tool == "flint_help" {
		value, e := a.mcpDiscovery(tool, input, opts, policy)
		if e != nil {
			return fail(e)
		}
		if refused := a.mcpResultTooLarge(nil, input, opts, policy, a.mcpKnownEnvironment(), map[string]any{"data": value}); refused != nil {
			return rpcResult(id, refused)
		}
		return rpcResult(id, mcpToolResult(map[string]any{"data": value}, structured, false, ""))
	}
	var name string
	arguments, controls := map[string]any{}, map[string]any{}
	discovery := policy.defaultToolSet() && (tool == "flint_read" || tool == "flint_write")
	if discovery {
		raw, exists := input["operation"]
		name, _ = raw.(string)
		if !exists {
			return fail(mcpInputError([]mcpInputProblem{mcpPlainProblem(mcpInputBodyLocation, "operation", "REQUIRED", "operation is required.")}))
		}
		if strings.TrimSpace(name) == "" {
			return fail(mcpInputError([]mcpInputProblem{mcpPlainProblem(mcpInputBodyLocation, "operation", "INVALID_VALUE", "operation must be a nonempty string.")}))
		}
		if raw, exists := input["arguments"]; exists {
			if encoded, ok := raw.(string); ok {
				if err := decodeJSONNumbers([]byte(encoded), &arguments); err != nil || arguments == nil {
					return fail(mcpInputError([]mcpInputProblem{mcpPlainProblem(mcpInputBodyLocation, "arguments", "INVALID_VALUE", "arguments must be a JSON object.")}))
				}
			} else if object, ok := raw.(map[string]any); ok {
				arguments = maps.Clone(object)
			} else {
				return fail(mcpInputError([]mcpInputProblem{mcpPlainProblem(mcpInputBodyLocation, "arguments", "INVALID_VALUE", "arguments must be an object.")}))
			}
		}
		for key, value := range input {
			if key != "operation" && key != "arguments" {
				controls[key] = value
			}
		}
	} else {
		if policy.defaultToolSet() {
			return a.mcpUnknownToolError(id, tool, policy)
		}
		index, err := a.mcpOperationIndex(policy)
		if err != nil {
			return fail(mcpDiscoverySnapshotError(err))
		}
		entry, ok := index.Lookup(tool)
		if !ok || tool != mcpToolName(entry.ID) {
			return a.mcpUnknownToolError(id, tool, policy)
		}
		name = entry.ID
		arguments = maps.Clone(input)
	}
	cmd, entry, e := a.mcpResolveOperation(name, policy)
	errorCommand = cmd
	if errorCommand == nil && entry.ID != "" {
		errorCommand = &Command{CanonicalName: entry.ID, Mutation: entry.Kind == "write"}
	}
	errorArguments = maps.Clone(arguments)
	errorArguments["_flint"] = controls
	if e != nil {
		return fail(e)
	}
	if discovery && ((tool == "flint_read") != (entry.Kind == "read")) {
		other := "flint_read"
		if entry.Kind == "write" {
			other = "flint_write"
		}
		e := usageError("OPERATION_NOT_AVAILABLE", "Use "+other+" for "+entry.ID+".", "operation")
		e.Details = map[string]any{"reason": "wrong_tool", "call": map[string]any{"tool": other, "arguments": input}}
		return fail(e)
	}
	names := mcpReadControls
	if entry.Kind == "write" {
		names = mcpWriteControls
	}
	if !discovery {
		for _, key := range names {
			if value, exists := arguments[key]; exists {
				controls[key] = value
				delete(arguments, key)
			}
		}
	}
	if cmd.CanonicalName == "signup" {
		if message := a.mcpSignupViolation(opts); message != "" {
			return fail(usageError("OPERATION_NOT_AVAILABLE", message, "operation"))
		}
	}
	arguments, e = mcpValidateInputs(cmd, entry, arguments, controls, policy)
	if e != nil {
		return fail(e)
	}
	internal := maps.Clone(arguments)
	for key, value := range controls {
		if key == "wait_seconds" {
			delete(controls, key)
			controls["for"] = fmt.Sprint(value) + "s"
		}
		if key == "dry_run" {
			if value == true {
				controls[key] = "client"
			} else {
				delete(controls, key)
			}
		}
	}
	// Trigger consumes a local run key to derive its recipe step keys.
	if entry.Idempotency == "none" && cmd.CanonicalName != "trigger" {
		delete(controls, "idempotency_key")
	}
	internal["_flint"] = controls
	if message := mcpGateViolation(internal, policy); message != "" && controls["dry_run"] != "client" {
		e := cliError(ExitConfirmation, errorTypeConfirmationRequired, "CONFIRMATION_REQUIRED", message)
		if strings.HasPrefix(message, "live ") {
			e = cliError(ExitAuth, errorTypeConfiguration, "LIVE_ACCESS_DISABLED", message)
		}
		return fail(e)
	}
	return rpcResult(id, a.runMCPTool(ctx, cmd, internal, opts, policy, structured))
}

func schemaNumber(value any) (float64, error) { return json.Number(fmt.Sprint(value)).Float64() }

func mcpLocalOperation(cmd *Command) bool {
	return cmd.Local || cmd.CanonicalName == "listen" || cmd.CanonicalName == "auth.status"
}
