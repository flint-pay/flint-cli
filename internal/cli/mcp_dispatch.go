package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/flint-pay/flint-cli/internal/opindex"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

var mcpReadControls = []string{"select", "all", "wait_for", "wait_seconds", "live"}
var mcpWriteControls = []string{"idempotency_key", "confirm", "dry_run", "select", "live"}

func (a *App) mcpDispatch(ctx context.Context, id any, tool string, input map[string]any, opts Options, policy mcpPolicy, structured bool) map[string]any {
	fail := func(e *CLIError) map[string]any {
		return rpcResult(id, mcpToolError(e.Message, mcpCLIErrorEnvelope(e)))
	}
	if tool == "flint_search" || tool == "flint_describe" || tool == "flint_help" {
		value, e := a.mcpDiscovery(tool, input, opts, policy)
		if e != nil {
			return fail(e)
		}
		return rpcResult(id, mcpToolResult(map[string]any{"data": value}, structured, false, ""))
	}
	var name string
	arguments, controls := map[string]any{}, map[string]any{}
	discovery := policy.defaultToolSet() && (tool == "flint_read" || tool == "flint_write")
	if discovery {
		name, _ = input["operation"].(string)
		if name == "" {
			return fail(usageError("INVALID_REQUEST", "operation is required.", "operation"))
		}
		if raw, exists := input["arguments"]; exists {
			if encoded, ok := raw.(string); ok {
				if err := decodeJSONNumbers([]byte(encoded), &arguments); err != nil || arguments == nil {
					return fail(usageError("INVALID_REQUEST", "arguments must be a JSON object.", "arguments"))
				}
			} else if object, ok := raw.(map[string]any); ok {
				arguments = maps.Clone(object)
			} else {
				return fail(usageError("INVALID_REQUEST", "arguments must be an object.", "arguments"))
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
	if e := validateMCPControls(entry, controls, policy); e != nil {
		return fail(e)
	}
	arguments, e = validateMCPOperation(cmd, entry, arguments)
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
	if message := mcpGateViolation(internal, policy); message != "" {
		return rpcResult(id, mcpToolError(message, nil))
	}
	return rpcResult(id, a.runMCPTool(ctx, cmd, internal, opts, policy, structured))
}

func validateMCPControls(entry opindex.Entry, controls map[string]any, policy mcpPolicy) *CLIError {
	names := mcpReadControls
	if entry.Kind == "write" {
		names = mcpWriteControls
	}
	for key, value := range controls {
		if !containsString(names, key) || key == "live" && !policy.allowLive {
			return usageError("INVALID_REQUEST", key+" is not an available control.", key)
		}
		valid := true
		switch key {
		case "live", "confirm", "dry_run", "all":
			_, valid = value.(bool)
		case "idempotency_key":
			text, ok := value.(string)
			valid = ok && len(text) > 0 && len(text) <= 255
		case "select", "wait_for":
			raw, err := json.Marshal(value)
			var values []string
			valid = err == nil && json.Unmarshal(raw, &values) == nil && len(values) > 0
			if key == "select" && len(values) > 50 {
				valid = false
			}
			for _, text := range values {
				if strings.TrimSpace(text) == "" {
					valid = false
				}
			}
		case "wait_seconds":
			number, err := schemaNumber(value)
			valid = err == nil && number >= 1 && number <= 120 && number == float64(int(number)) && validateMCPSchema(map[string]any{"type": "integer"}, value) == nil
		}
		if !valid {
			return usageError("INVALID_REQUEST", key+" has an invalid value.", key)
		}
		if key == "all" && !entry.Paginated || (key == "wait_for" || key == "wait_seconds") && !entry.Get {
			return usageError("INVALID_REQUEST", key+" is not allowed for "+entry.ID+".", key)
		}
	}
	if entry.Kind == "write" && entry.Idempotency == "required" && controls["idempotency_key"] == nil {
		return usageError("INVALID_REQUEST", "idempotency_key is required for "+entry.ID+". Choose a unique key and reuse it if you retry this request.", "idempotency_key")
	}
	return nil
}

func schemaNumber(value any) (float64, error) { return json.Number(fmt.Sprint(value)).Float64() }

// Validate public arguments before credentials, guards, or transport are used.
func validateMCPOperation(cmd *Command, entry opindex.Entry, args map[string]any) (map[string]any, *CLIError) {
	args = maps.Clone(args)
	if cmd.Stream {
		for name, bound := range map[string]int{"max_seconds": 120, "max_events": 100} {
			if raw, ok := args[name]; ok {
				n, err := schemaNumber(raw)
				if err != nil || n < 1 || n > float64(bound) || n != float64(int(n)) {
					return nil, usageError("INVALID_REQUEST", fmt.Sprintf("%s must be from 1 to %d.", name, bound), name)
				}
			}
		}
	}
	if mcpLocalOperation(cmd) {
		if entry.ID == "trigger" {
			if raw, ok := args["max_seconds"]; ok {
				n, err := schemaNumber(raw)
				if err != nil || n < 1 || n > 60 || n != float64(int(n)) {
					return nil, usageError("INVALID_REQUEST", "max_seconds must be from 1 to 60.", "max_seconds")
				}
			}
		}
		allowed := map[string]Arg{}
		for _, arg := range cmd.Arguments {
			allowed[arg.Name] = arg
		}
		if cmd.Stream {
			allowed["max_events"] = Arg{Type: "integer"}
			allowed["max_seconds"] = Arg{Type: "integer"}
		}
		if entry.ID == "trigger" {
			allowed["max_seconds"] = Arg{Type: "integer"}
		}
		for key, value := range args {
			arg, ok := allowed[key]
			if !ok {
				return nil, usageError("INVALID_REQUEST", key+" is not an argument of "+entry.ID+".", key)
			}
			schema := map[string]any{"type": arg.Type}
			if arg.Type == "" || arg.Type == "time" {
				schema["type"] = "string"
			}
			if arg.Repeat {
				schema = map[string]any{"type": "array", "items": schema}
			}
			if len(arg.Enum) > 0 {
				schema["enum"] = arg.Enum
			}
			if err := validateMCPSchema(schema, value); err != nil {
				return nil, usageError("INVALID_REQUEST", key+" has an invalid value.", key)
			}
		}
		for _, arg := range cmd.Arguments {
			if arg.Required && args[arg.Name] == nil {
				return nil, usageError("INVALID_REQUEST", arg.Name+" is required.", arg.Name)
			}
		}
		return args, nil
	}
	operation, ok := openAPIOperationByID(cmd.OperationID)
	if !ok {
		return nil, usageError("UNKNOWN_OPERATION", "The operation is not in the embedded API contract.", "operation")
	}
	body := maps.Clone(args)
	for _, parameter := range operation.Parameters {
		location, _ := parameter["in"].(string)
		if location != "path" && location != "query" {
			continue
		}
		name, _ := parameter["name"].(string)
		value, exists := args[name]
		delete(body, name)
		if !exists {
			if parameter["required"] == true {
				return nil, usageError("INVALID_REQUEST", name+" is required.", name)
			}
			continue
		}
		schema, _ := parameter["schema"].(map[string]any)
		schema = maps.Clone(schema)
		if location == "query" {
			schema = queryDiscoverySchema(schema)
			if schema["type"] == "array" {
				if text, ok := value.(string); ok {
					value = []any{text}
					args[name] = value
				}
			}
		}
		if err := validateMCPSchema(schema, value); err != nil {
			return nil, usageError("INVALID_REQUEST", name+" does not match the operation's input schema: "+err.Error(), name)
		}
	}
	if _, e := validateRequestBody(cmd, entry.ID, body); e != nil {
		return nil, e
	}
	return args, nil
}

func validateMCPSchema(schema map[string]any, value any) error {
	raw, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	var resource any
	if err = json.Unmarshal(raw, &resource); err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	if err = compiler.AddResource("urn:flint:mcp:input", resource); err != nil {
		return err
	}
	compiled, err := compiler.Compile("urn:flint:mcp:input")
	if err != nil {
		return err
	}
	raw, err = json.Marshal(value)
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return compiled.Validate(instance)
}

func mcpLocalOperation(cmd *Command) bool {
	return cmd.Local || cmd.CanonicalName == "listen" || cmd.CanonicalName == "auth.status"
}
