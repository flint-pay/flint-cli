package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const latestMCPProtocolVersion = "2025-11-25"

const (
	mcpMaxStreamEvents   = 100
	mcpMaxStreamDuration = 2 * time.Minute
	mcpMaxOutputBytes    = 32 << 20
	mcpMaxStderrBytes    = 1 << 20
)

var supportedMCPProtocolVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
}

type jsonRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
	Result  any            `json:"result,omitempty"`
	Error   map[string]any `json:"error,omitempty"`
}

func mcpRequestKey(id any) string {
	raw, err := json.Marshal(id)
	if err != nil {
		return fmt.Sprintf("%T:%v", id, id)
	}
	return string(raw)
}

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

func (a *App) serveMCP(opts Options) int {
	scanner := bufio.NewScanner(a.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	encoder := json.NewEncoder(a.Stdout)
	encoder.SetEscapeHTML(false)
	var encoderMu sync.Mutex
	writeResponse := func(response map[string]any) error {
		encoderMu.Lock()
		defer encoderMu.Unlock()
		return encoder.Encode(response)
	}
	var inFlightMu sync.Mutex
	inFlight := map[string]context.CancelFunc{}
	var workers sync.WaitGroup
	type pendingFeedbackConsent struct {
		toolCall      jsonRPCRequest
		elicitationID string
		cancelled     bool
	}
	pendingConsent := map[string]pendingFeedbackConsent{}
	consentCancelled := false
	clientSupportsElicitation := false
	startToolCall := func(req jsonRPCRequest, negotiatedVersion string) {
		key := mcpRequestKey(req.ID)
		inFlightMu.Lock()
		_, duplicate := inFlight[key]
		if !duplicate {
			baseCtx := a.Context
			if baseCtx == nil {
				baseCtx = context.Background()
			}
			toolCtx, cancel := context.WithCancel(baseCtx)
			inFlight[key] = cancel
			workers.Add(1)
			go func(req jsonRPCRequest, protocolVersion, key string) {
				defer workers.Done()
				response := a.handleMCPVersionContext(toolCtx, req, opts, protocolVersion)
				if toolCtx.Err() != nil {
					response = rpcError(req.ID, -32800, "Request canceled")
				}
				inFlightMu.Lock()
				delete(inFlight, key)
				inFlightMu.Unlock()
				cancel()
				if err := writeResponse(response); err != nil {
					fmt.Fprintln(a.Stderr, "MCP output failed: "+err.Error())
				}
			}(req, negotiatedVersion, key)
		}
		inFlightMu.Unlock()
		if duplicate {
			_ = writeResponse(rpcError(req.ID, -32600, "A request with this id is already running"))
		}
	}
	cancelInFlight := func() {
		inFlightMu.Lock()
		defer inFlightMu.Unlock()
		for _, cancel := range inFlight {
			cancel()
		}
	}
	defer func() {
		cancelInFlight()
		workers.Wait()
	}()
	initializeSeen := false
	initialized := false
	protocolVersion := latestMCPProtocolVersion
	for scanner.Scan() {
		var req jsonRPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			if err := writeResponse(rpcError(nil, -32700, "Parse error")); err != nil {
				fmt.Fprintln(a.Stderr, "MCP output failed: "+err.Error())
				return ExitNetwork
			}
			continue
		}
		if req.JSONRPC == "2.0" && req.Method == "" && req.ID != nil {
			key := mcpRequestKey(req.ID)
			pending, ok := pendingConsent[key]
			if !ok {
				_ = writeResponse(rpcError(req.ID, -32600, "Unexpected JSON-RPC response"))
				continue
			}
			delete(pendingConsent, key)
			if pending.cancelled {
				continue
			}
			action, content := feedbackElicitationResult(req.Result)
			switch action {
			case "accept":
				if enabled, _ := content["enable_agent_feedback"].(bool); enabled {
					if err := a.setAgentFeedbackSubmission(opts, "enabled"); err != nil {
						_ = writeResponse(rpcResult(pending.toolCall.ID, mcpToolError("Could not save feedback consent.", map[string]any{"code": "CONFIG_WRITE_FAILED"})))
						continue
					}
					startToolCall(pending.toolCall, protocolVersion)
					continue
				}
				_ = a.setAgentFeedbackSubmission(opts, "disabled")
				_ = writeResponse(rpcResult(pending.toolCall.ID, mcpToolError("Agent feedback submission is disabled.", map[string]any{"code": "AGENT_FEEDBACK_DISABLED"})))
			case "decline":
				_ = a.setAgentFeedbackSubmission(opts, "disabled")
				_ = writeResponse(rpcResult(pending.toolCall.ID, mcpToolError("Agent feedback submission is disabled.", map[string]any{"code": "AGENT_FEEDBACK_DISABLED"})))
			default:
				consentCancelled = true
				_ = writeResponse(rpcResult(pending.toolCall.ID, mcpToolError("Agent feedback submission was not enabled.", map[string]any{"code": "AGENT_FEEDBACK_DISABLED"})))
			}
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			if err := writeResponse(rpcError(req.ID, -32600, "Invalid Request")); err != nil {
				fmt.Fprintln(a.Stderr, "MCP output failed: "+err.Error())
				return ExitNetwork
			}
			continue
		}
		if strings.HasPrefix(req.Method, "notifications/") {
			if req.Method == "notifications/initialized" && initializeSeen {
				initialized = true
			}
			if req.Method == "notifications/cancelled" {
				if requestID, ok := req.Params["requestId"]; ok {
					key := mcpRequestKey(requestID)
					for consentID, pending := range pendingConsent {
						if mcpRequestKey(pending.toolCall.ID) != key || pending.cancelled {
							continue
						}
						pending.cancelled = true
						pendingConsent[consentID] = pending
						_ = writeResponse(rpcError(pending.toolCall.ID, -32800, "Request canceled"))
						_ = writeResponse(map[string]any{
							"jsonrpc": "2.0",
							"method":  "notifications/cancelled",
							"params": map[string]any{
								"requestId": pending.elicitationID,
								"reason":    "Parent request canceled",
							},
						})
					}
					inFlightMu.Lock()
					cancel := inFlight[key]
					inFlightMu.Unlock()
					if cancel != nil {
						cancel()
					}
				}
			}
			continue
		}
		// MCP operations other than notifications are JSON-RPC requests. Refuse
		// notification-shaped tool calls so a client cannot trigger a mutation
		// without receiving its confirmation or error result.
		if req.ID == nil {
			if err := writeResponse(rpcError(nil, -32600, "MCP requests require an id")); err != nil {
				fmt.Fprintln(a.Stderr, "MCP output failed: "+err.Error())
				return ExitNetwork
			}
			continue
		}
		if req.Method != "initialize" && req.Method != "ping" && !initialized {
			if req.ID != nil {
				if err := writeResponse(rpcError(req.ID, -32002, "Server is not initialized")); err != nil {
					fmt.Fprintln(a.Stderr, "MCP output failed: "+err.Error())
					return ExitNetwork
				}
			}
			continue
		}
		if req.Method == "initialize" && initializeSeen {
			if req.ID != nil {
				if err := writeResponse(rpcError(req.ID, -32600, "Initialize may only be called once")); err != nil {
					fmt.Fprintln(a.Stderr, "MCP output failed: "+err.Error())
					return ExitNetwork
				}
			}
			continue
		}
		if req.Method == "tools/call" {
			if mcpToolCallTargetsFeedbackCreate(req) {
				state := a.agentFeedbackSubmission(opts)
				if state == "disabled" || (state == "" && consentCancelled) {
					_ = writeResponse(rpcResult(req.ID, mcpToolError("Agent feedback submission is disabled.", map[string]any{"code": "AGENT_FEEDBACK_DISABLED"})))
					continue
				}
				if state == "" && clientSupportsElicitation {
					serverID := "flint-feedback-consent-" + mcpRequestKey(req.ID)
					pendingConsent[mcpRequestKey(serverID)] = pendingFeedbackConsent{toolCall: req, elicitationID: serverID}
					if err := writeResponse(feedbackConsentElicitation(serverID)); err != nil {
						return ExitNetwork
					}
					continue
				}
				if state == "" && !clientSupportsElicitation {
					message := strings.ReplaceAll(agentFeedbackConsentText, " [y/N]", "") + "\n\nRun flint feedback configure enabled in a terminal to enable it."
					_ = writeResponse(rpcResult(req.ID, mcpToolError(message, map[string]any{"code": "AGENT_FEEDBACK_DISABLED"})))
					continue
				}
			}
			startToolCall(req, protocolVersion)
			continue
		}
		response := a.handleMCPVersion(req, opts, protocolVersion)
		if req.Method == "initialize" {
			if _, failed := response["error"]; !failed {
				initializeSeen = true
				initialized = false
				if negotiated, ok := lookupPath(response, "result.protocolVersion"); ok {
					protocolVersion, _ = negotiated.(string)
				}
				clientSupportsElicitation = mcpClientSupportsElicitation(req.Params)
			}
		}
		if req.ID != nil {
			if err := writeResponse(response); err != nil {
				fmt.Fprintln(a.Stderr, "MCP output failed: "+err.Error())
				return ExitNetwork
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(a.Stderr, "MCP input failed: "+err.Error())
		return ExitNetwork
	}
	return ExitOK
}

func mcpClientSupportsElicitation(params map[string]any) bool {
	capabilities, _ := params["capabilities"].(map[string]any)
	elicitation, ok := capabilities["elicitation"].(map[string]any)
	if !ok {
		return false
	}
	_, form := elicitation["form"]
	return form || len(elicitation) == 0
}

func feedbackConsentElicitation(id any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "elicitation/create", "params": map[string]any{
		"mode":    "form",
		"message": strings.ReplaceAll(agentFeedbackConsentText, " [y/N]", ""),
		"requestedSchema": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"enable_agent_feedback": map[string]any{"type": "boolean", "title": "Enable agent feedback", "default": false}},
			"required":   []string{"enable_agent_feedback"},
		},
	}}
}

func feedbackElicitationResult(value any) (string, map[string]any) {
	result, _ := value.(map[string]any)
	action, _ := result["action"].(string)
	content, _ := result["content"].(map[string]any)
	return action, content
}

func (a *App) agentFeedbackSubmission(opts Options) string {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return "disabled"
	}
	return resolved.AgentFeedbackSubmission
}

func (a *App) setAgentFeedbackSubmission(opts Options, state string) error {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return err
	}
	return a.updateConfig(func(cfg *Config) error {
		if cfg.Profiles == nil {
			cfg.Profiles = map[string]Profile{}
		}
		profile := cfg.Profiles[resolved.ProfileName]
		profile.AgentFeedbackSubmission = state
		cfg.Profiles[resolved.ProfileName] = profile
		return nil
	})
}

func (a *App) handleMCP(req jsonRPCRequest, opts Options) map[string]any {
	return a.handleMCPVersion(req, opts, latestMCPProtocolVersion)
}

func (a *App) handleMCPVersion(req jsonRPCRequest, opts Options, protocolVersion string) map[string]any {
	ctx := a.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return a.handleMCPVersionContext(ctx, req, opts, protocolVersion)
}

func (a *App) handleMCPVersionContext(ctx context.Context, req jsonRPCRequest, opts Options, protocolVersion string) map[string]any {
	switch req.Method {
	case "initialize":
		requested, _ := req.Params["protocolVersion"].(string)
		if requested == "" {
			return rpcError(req.ID, -32602, "initialize requires params.protocolVersion")
		}
		if _, ok := req.Params["capabilities"].(map[string]any); !ok {
			return rpcError(req.ID, -32602, "initialize requires params.capabilities")
		}
		clientInfo, ok := req.Params["clientInfo"].(map[string]any)
		if !ok {
			return rpcError(req.ID, -32602, "initialize requires params.clientInfo")
		}
		clientName, _ := clientInfo["name"].(string)
		clientVersion, _ := clientInfo["version"].(string)
		if clientName == "" || clientVersion == "" {
			return rpcError(req.ID, -32602, "initialize clientInfo requires name and version")
		}
		protocolVersion := latestMCPProtocolVersion
		if supportedMCPProtocolVersions[requested] {
			protocolVersion = requested
		}
		return rpcResult(req.ID, map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "flint", "version": a.Info.Version}})
	case "ping":
		return rpcResult(req.ID, map[string]any{})
	case "tools/list":
		tools := []map[string]any{}
		feedbackScopes := a.mcpFeedbackScopes(ctx, opts)
		feedbackState := a.agentFeedbackSubmission(opts)
		for _, cmd := range a.Registry.Commands {
			if !mcpCommandExposed(cmd) {
				continue
			}
			switch cmd.CanonicalName {
			case "feedback-reports.create":
				if feedbackState == "disabled" || (feedbackState == "enabled" && !feedbackScopes["developer.feedback_reports.write"]) {
					continue
				}
			case "feedback-reports.get", "feedback-reports.list":
				if !feedbackScopes["developer.feedback_reports.read"] {
					continue
				}
			}
			schema, e := schemaForMCPCommand(cmd)
			if e != nil {
				return rpcError(req.ID, -32603, "Could not generate tool input schema for "+cmd.CanonicalName)
			}
			outputSchema, outputErr := mcpOutputSchema(cmd)
			if outputErr != nil {
				return rpcError(req.ID, -32603, "Could not generate tool output schema for "+cmd.CanonicalName)
			}
			annotations := mcpToolAnnotations(cmd)
			tool := map[string]any{"name": cmd.CanonicalName, "description": cmd.Description, "inputSchema": schema, "annotations": annotations}
			if protocolVersion >= "2025-06-18" {
				tool["outputSchema"] = outputSchema
			}
			tools = append(tools, tool)
		}
		return rpcResult(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		name, nameOK := req.Params["name"].(string)
		if !nameOK || name == "" {
			return rpcError(req.ID, -32602, "tools/call requires params.name")
		}
		cmd, ok := a.Registry.ByName(name)
		if !ok || !mcpCommandExposed(cmd) {
			return rpcError(req.ID, -32602, "Unknown tool: "+name)
		}
		arguments := map[string]any{}
		if rawArguments, exists := req.Params["arguments"]; exists {
			var argumentsOK bool
			arguments, argumentsOK = rawArguments.(map[string]any)
			if !argumentsOK {
				return rpcError(req.ID, -32602, "tools/call params.arguments must be an object")
			}
		}
		if err := validateMCPArguments(cmd, arguments); err != nil {
			return rpcResult(req.ID, mcpToolError("Tool arguments do not match the advertised input schema: "+err.Error(), nil))
		}
		if mcpCommandTargetsFeedbackCreate(cmd, arguments) {
			state := a.agentFeedbackSubmission(opts)
			if state == "disabled" {
				return rpcResult(req.ID, mcpToolError("Agent feedback submission is disabled.", map[string]any{"code": "AGENT_FEEDBACK_DISABLED"}))
			}
			if state == "enabled" && !a.mcpFeedbackScopes(ctx, opts)["developer.feedback_reports.write"] {
				return rpcResult(req.ID, mcpToolError("The active credential does not have developer.feedback_reports.write. Create or import a credential with that scope, then restart the MCP server.", map[string]any{"code": "INSUFFICIENT_SCOPE"}))
			}
		}
		if (cmd.CanonicalName == "feedback-reports.get" || cmd.CanonicalName == "feedback-reports.list") && !a.mcpFeedbackScopes(ctx, opts)["developer.feedback_reports.read"] {
			return rpcResult(req.ID, mcpToolError("The active credential does not have developer.feedback_reports.read.", map[string]any{"code": "INSUFFICIENT_SCOPE"}))
		}
		result, transformed, exit := a.callMCPCommand(ctx, cmd, arguments, opts)
		if exit != 0 {
			return rpcResult(req.ID, mcpToolError(fmt.Sprintf("Flint command exited %d", exit), result))
		}
		return rpcResult(req.ID, mcpToolResult(result, protocolVersion >= "2025-06-18", transformed))
	default:
		return rpcError(req.ID, -32601, "Method not found")
	}
}

func mcpToolCallTargetsFeedbackCreate(req jsonRPCRequest) bool {
	name, _ := req.Params["name"].(string)
	if name == "feedback-reports.create" {
		return true
	}
	if name != "api" {
		return false
	}
	arguments, _ := req.Params["arguments"].(map[string]any)
	return mcpCommandTargetsFeedbackCreate(&Command{CanonicalName: "api"}, arguments)
}

func mcpCommandTargetsFeedbackCreate(cmd *Command, arguments map[string]any) bool {
	if cmd == nil {
		return false
	}
	if cmd.CanonicalName == "feedback-reports.create" {
		return true
	}
	if cmd.CanonicalName != "api" {
		return false
	}
	method, _ := arguments["method"].(string)
	path, _ := arguments["path"].(string)
	return isFeedbackCreateRequest(method, path)
}

func (a *App) mcpFeedbackScopes(ctx context.Context, opts Options) map[string]bool {
	result := map[string]bool{}
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return result
	}
	key, _, err := a.resolveCredential(resolved.ProfileName)
	if err != nil || key == "" {
		return result
	}
	baseURL, err := a.baseURLForCredential(key)
	if err != nil {
		return result
	}
	timeout := opts.Timeout
	if timeout <= 0 || timeout > 10*time.Second {
		timeout = 5 * time.Second
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	auth, lookupErr := a.fetchAuthContext(lookupCtx, baseURL, key, opts.Debug)
	if lookupErr != nil {
		return result
	}
	for _, scope := range auth.Scopes {
		result[strings.TrimSpace(scope)] = true
	}
	return result
}

func mcpCommandExposed(cmd *Command) bool {
	return cmd != nil && cmd.CanonicalName == cmd.Name && cmd.CanonicalName != "mcp.serve" && !cmd.MCPHidden
}

func validateMCPArguments(cmd *Command, arguments map[string]any) error {
	schema, schemaErr := schemaForMCPCommand(cmd)
	if schemaErr != nil {
		return fmt.Errorf("generate input schema: %s", schemaErr.Message)
	}
	rawSchema, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("encode input schema: %w", err)
	}
	var normalizedSchema any
	if err := json.Unmarshal(rawSchema, &normalizedSchema); err != nil {
		return fmt.Errorf("normalize input schema: %w", err)
	}
	const resourceURL = "https://cli.withflintpay.com/mcp/tool-input.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resourceURL, normalizedSchema); err != nil {
		return fmt.Errorf("load input schema: %w", err)
	}
	compiled, err := compiler.Compile(resourceURL)
	if err != nil {
		return fmt.Errorf("compile input schema: %w", err)
	}
	if err := compiled.Validate(arguments); err != nil {
		return err
	}
	if err := validateMCPStreamBounds(cmd, arguments); err != nil {
		return err
	}
	return nil
}

func schemaForMCPCommand(cmd *Command) (map[string]any, *CLIError) {
	schema, schemaErr := schemaForCommand(cmd, true)
	if schemaErr != nil {
		return schema, schemaErr
	}
	// MCP requires an object root even when the public schema is a union.
	schema["type"] = "object"
	if cmd == nil || !cmd.Stream {
		return schema, nil
	}
	properties, _ := schema["properties"].(map[string]any)
	controlsProperty, _ := properties["_flint"].(map[string]any)
	controls, _ := controlsProperty["properties"].(map[string]any)
	controls["max_events"] = map[string]any{
		"type":        "integer",
		"minimum":     1,
		"maximum":     mcpMaxStreamEvents,
		"description": fmt.Sprintf("Stop after at most %d stream records.", mcpMaxStreamEvents),
	}
	controls["for"] = map[string]any{
		"type":        "string",
		"description": "Positive Go duration no longer than 2m.",
	}
	controlsProperty["description"] = "CLI execution controls. Streaming tools require max_events or for. Flint also applies a 100-record and 2-minute safety bound."
	schema["allOf"] = appendSchemaConstraint(schema["allOf"], map[string]any{
		"required": []string{"_flint"},
		"properties": map[string]any{
			"_flint": map[string]any{
				"anyOf": []any{
					map[string]any{"required": []string{"max_events"}},
					map[string]any{"required": []string{"for"}},
				},
			},
		},
	})
	return schema, nil
}

func appendSchemaConstraint(existing any, constraint map[string]any) []any {
	switch values := existing.(type) {
	case []any:
		return append(values, constraint)
	case nil:
		return []any{constraint}
	default:
		return []any{values, constraint}
	}
}

func validateMCPStreamBounds(cmd *Command, arguments map[string]any) error {
	if cmd == nil || !cmd.Stream {
		return nil
	}
	controls, _ := arguments["_flint"].(map[string]any)
	if raw, ok := controls["for"]; ok {
		duration, err := time.ParseDuration(fmt.Sprint(raw))
		if err != nil || duration <= 0 {
			return fmt.Errorf("_flint.for must be a positive Go duration")
		}
		if duration > mcpMaxStreamDuration {
			return fmt.Errorf("_flint.for must not exceed %s", mcpMaxStreamDuration)
		}
	}
	return nil
}

func mcpToolAnnotations(cmd *Command) map[string]any {
	readOnly := !cmd.Mutation && !cmd.Destructive
	destructive := cmd.Destructive
	openWorld := !cmd.Local
	switch cmd.CanonicalName {
	case "api":
		// The raw tool's method is selected at call time and can be DELETE.
		readOnly = false
		destructive = true
	case "auth.logout", "history":
		readOnly = false
		destructive = true
	case "config.set", "doctor":
		readOnly = false
	case "signup":
		readOnly = false
		openWorld = true
	case "config.validate", "init":
		openWorld = true
	}
	return map[string]any{
		"readOnlyHint":    readOnly,
		"destructiveHint": destructive,
		"idempotentHint":  readOnly,
		"openWorldHint":   openWorld,
	}
}

func (a *App) callMCPCommand(ctx context.Context, cmd *Command, args map[string]any, parent Options) (any, bool, int) {
	if cmd.Stream {
		bound := mcpMaxStreamDuration
		if controls, ok := args["_flint"].(map[string]any); ok {
			if raw, exists := controls["for"]; exists {
				if requested, err := time.ParseDuration(fmt.Sprint(raw)); err == nil && requested > 0 {
					bound = requested
				}
			}
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bound)
		defer cancel()
	}
	argv := append([]string(nil), cmd.Path[1:]...)
	argv = appendInheritedMCPOptions(argv, parent)
	transformOptions, transformed, transformErr := mcpOutputTransformOptions(cmd, args)
	if transformErr != nil {
		return mcpCLIErrorEnvelope(transformErr), false, transformErr.ExitCode
	}
	body := map[string]any{}
	known := map[string]bool{}
	maxPositional := 0
	for _, arg := range cmd.Arguments {
		maxPositional = max(maxPositional, arg.Positional)
	}
	positionals := make([]string, maxPositional)
	for _, arg := range cmd.Arguments {
		known[arg.Name] = true
		value, ok := args[arg.Name]
		if !ok {
			continue
		}
		if arg.Positional > 0 {
			positionals[arg.Positional-1] = fmt.Sprint(value)
			continue
		}
		if arg.Flag != "" {
			argv = appendMCPFlag(argv, arg.Flag, value)
		}
	}
	for _, value := range positionals {
		if value != "" {
			argv = append(argv, value)
		}
	}
	for key, value := range args {
		if !known[key] && !strings.HasPrefix(key, "_") {
			body[key] = value
		}
	}
	if globals, ok := args["_flint"].(map[string]any); ok {
		for key, value := range globals {
			if key == "field" || key == "select" || key == "jq" {
				continue
			}
			flag := "--" + strings.ReplaceAll(key, "_", "-")
			argv = appendMCPFlag(argv, flag, value)
		}
	}
	if cmd.Stream {
		globals, _ := args["_flint"].(map[string]any)
		if _, exists := globals["max_events"]; !exists {
			argv = append(argv, "--max-events", fmt.Sprint(mcpMaxStreamEvents))
		}
		if _, exists := globals["for"]; !exists {
			argv = append(argv, "--for", mcpMaxStreamDuration.String())
		}
	}
	var stdin io.Reader = strings.NewReader("")
	if len(body) > 0 {
		raw, err := json.Marshal(body)
		if err != nil {
			return map[string]any{"error": "Could not encode MCP tool input: " + err.Error()}, false, ExitUsage
		}
		stdin = bytes.NewReader(raw)
		argv = append(argv, "--input", "-")
	}
	argv = append(argv, "--output", "json", "--no-input")
	stdout := &mcpCappedBuffer{limit: mcpMaxOutputBytes}
	stderr := &mcpCappedBuffer{limit: mcpMaxStderrBytes}
	child := *a
	child.Stdout = stdout
	child.Stderr = stderr
	child.Stdin = stdin
	child.IsTTY = func() bool { return false }
	child.Context = ctx
	exit := child.Run(argv)
	if stdout.exceeded {
		return map[string]any{"error": fmt.Sprintf("Flint command output exceeded the %d byte MCP safety limit.", mcpMaxOutputBytes)}, false, ExitNetwork
	}
	trimmed := bytes.TrimSpace(stdout.Bytes())
	if len(trimmed) == 0 {
		return map[string]any{"stderr": strings.TrimSpace(stderr.String())}, false, exit
	}
	var result any
	if err := json.Unmarshal(trimmed, &result); err == nil {
		if exit == ExitOK && transformed {
			result, transformErr = applyOutputTransforms(result, transformOptions)
			if transformErr != nil {
				return mcpCLIErrorEnvelope(transformErr), false, transformErr.ExitCode
			}
		}
		return result, transformed, exit
	}
	var lines []any
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 64<<10), 20<<20)
	for scanner.Scan() {
		var line any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return map[string]any{"error": "Could not decode Flint NDJSON output: " + err.Error(), "stderr": strings.TrimSpace(stderr.String())}, false, ExitNetwork
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return map[string]any{"error": "Could not read Flint NDJSON output: " + err.Error(), "stderr": strings.TrimSpace(stderr.String())}, false, ExitNetwork
	}
	if len(lines) > 0 {
		return map[string]any{"records": lines}, false, exit
	}
	return map[string]any{"stdout": string(trimmed), "stderr": strings.TrimSpace(stderr.String())}, false, exit
}

func mcpOutputTransformOptions(cmd *Command, args map[string]any) (Options, bool, *CLIError) {
	opts := defaultOptions()
	controls, _ := args["_flint"].(map[string]any)
	transformed := false
	if raw, exists := controls["field"]; exists {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.field must be a non-empty string.", "field")
		}
		opts.Field = value
		transformed = true
	}
	if raw, exists := controls["jq"]; exists {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.jq must be a non-empty string.", "jq")
		}
		opts.JQ = value
		transformed = true
	}
	if raw, exists := controls["select"]; exists {
		values, ok := raw.([]any)
		if !ok || len(values) == 0 {
			return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.select must be a non-empty string array.", "select")
		}
		for _, rawValue := range values {
			value, ok := rawValue.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.select must be a non-empty string array.", "select")
			}
			opts.Select = append(opts.Select, value)
		}
		transformed = true
	}
	if !transformed {
		return opts, false, nil
	}
	if !cmd.Supports.Field || !cmd.Supports.Select || !cmd.Supports.JQ {
		return opts, false, usageError("UNSUPPORTED_FLAG", "Output transforms do not apply to this command.", "field")
	}
	if opts.Field != "" && opts.JQ != "" {
		return opts, false, usageError("CONFLICTING_FLAGS", "_flint.jq and _flint.field cannot be used together.", "field")
	}
	if paginate, _ := controls["paginate"].(bool); paginate {
		return opts, false, usageError("UNSUPPORTED_FLAG", "Output transforms do not apply to paginated NDJSON output.", "paginate")
	}
	return opts, true, nil
}

func mcpCLIErrorEnvelope(err *CLIError) map[string]any {
	payload := map[string]any{"type": err.Type, "code": err.Code, "message": err.Message}
	if err.Param != "" {
		payload["param"] = err.Param
	}
	if err.Details != nil {
		payload["details"] = err.Details
	}
	return map[string]any{"error": payload}
}

func mcpOutputSchema(cmd *Command) (map[string]any, *CLIError) {
	outputSchema, outputErr := schemaForCommand(cmd, false)
	if outputErr != nil {
		return nil, outputErr
	}
	if cmd.Stream {
		return mcpStreamOutputSchema(), nil
	}
	if !cmd.Supports.Field || !cmd.Supports.Select || !cmd.Supports.JQ {
		return outputSchema, nil
	}
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"anyOf": []any{
			outputSchema,
			map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties":           map[string]any{"value": map[string]any{}},
				"required":             []string{"value"},
			},
		},
	}, nil
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
						"type": map[string]any{"type": "string", "enum": []string{"version", "event", "listener", "ready", "gap", "withheld", "forward", "disconnect", "checkpoint"}},
					},
				},
			},
		},
		"required": []string{"records"},
	}
}

func appendMCPFlag(argv []string, flag string, value any) []string {
	switch values := value.(type) {
	case []any:
		for _, item := range values {
			argv = appendMCPFlag(argv, flag, item)
		}
	case bool:
		argv = append(argv, flag+"="+fmt.Sprint(values))
	default:
		argv = append(argv, flag, fmt.Sprint(value))
	}
	return argv
}

func appendInheritedMCPOptions(argv []string, opts Options) []string {
	for _, item := range []struct {
		name  string
		value string
	}{
		{"profile", opts.Profile},
		{"merchant", opts.Merchant},
		{"color", opts.Color},
	} {
		if len(opts.Raw[item.name]) > 0 && item.value != "" {
			argv = append(argv, "--"+item.name, item.value)
		}
	}
	for _, item := range []struct {
		name  string
		value bool
	}{
		{"live", opts.Live},
		{"debug", opts.Debug},
		{"quiet", opts.Quiet},
	} {
		if len(opts.Raw[item.name]) > 0 {
			argv = append(argv, "--"+item.name+"="+fmt.Sprint(item.value))
		}
	}
	if len(opts.Raw["timeout"]) > 0 {
		argv = append(argv, "--timeout", opts.Timeout.String())
	}
	return argv
}

func mcpToolResult(value any, structured, transformed bool) map[string]any {
	structuredValue := value
	if transformed {
		structuredValue = map[string]any{"value": value}
	}
	raw, _ := json.Marshal(structuredValue)
	result := map[string]any{"content": []map[string]any{{"type": "text", "text": string(raw)}}}
	if structured {
		result["structuredContent"] = structuredValue
	}
	return result
}
func mcpToolError(message string, value any) map[string]any {
	payload := map[string]any{"message": message}
	if value != nil {
		payload["result"] = value
	}
	raw, _ := json.Marshal(payload)
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(raw)}}, "isError": true}
}
func rpcResult(id, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}
func rpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}
