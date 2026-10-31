package cli

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/flint-pay/flint-cli/internal/schemaview"
)

func (a *App) mcpKnownEnvironment() string {
	if a.mcpAuth != nil {
		if auth, known := a.mcpAuth.snapshot(); known {
			return normalizeEnvironment(auth.Environment)
		}
	}
	return ""
}

func mcpAgentAction(text string, call map[string]any) map[string]any {
	return map[string]any{"actor": "agent", "text": text, "call": call}
}
func mcpUserAction(text string) map[string]any {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "Ask the user to") {
		text = "Ask the user to " + text
	}
	return map[string]any{"actor": "user", "text": text}
}

// Calls always use the current server's tool mode and public control names.
func (a *App) mcpOperationCall(cmd *Command, args map[string]any, policy mcpPolicy) map[string]any {
	arguments := maps.Clone(args)
	if arguments == nil {
		arguments = map[string]any{}
	}
	controls, _ := arguments["_flint"].(map[string]any)
	delete(arguments, "_flint")
	public := map[string]any{}
	for name, value := range controls {
		if name == "for" {
			if d, err := time.ParseDuration(fmt.Sprint(value)); err == nil {
				public["wait_seconds"] = d.Seconds()
			}
			continue
		}
		if !containsString(mcpReadControls, name) && !containsString(mcpWriteControls, name) {
			continue
		}
		if name == "dry_run" && value == "client" {
			value = true
		}
		if name == "idempotency_key" && commandIdempotencyClass(cmd) == "none" {
			continue
		}
		public[name] = value
	}
	if !policy.defaultToolSet() {
		for key, value := range public {
			arguments[key] = value
		}
		return map[string]any{"tool": mcpToolName(cmd.CanonicalName), "arguments": arguments}
	}
	tool := "flint_read"
	if mcpResultIsWrite(cmd, args) {
		tool = "flint_write"
	}
	public["operation"], public["arguments"] = cmd.CanonicalName, arguments
	return map[string]any{"tool": tool, "arguments": public}
}

func mcpErrorFact(value any, name string) any {
	switch node := value.(type) {
	case map[string]any:
		if v, ok := node[name]; ok {
			return v
		}
		// Facts can be at the root of older API errors or in details[].
		for _, key := range []string{"details", "problems", "last_error"} {
			if found := mcpErrorFact(node[key], name); found != nil {
				return found
			}
		}
	case []any:
		for _, item := range node {
			if found := mcpErrorFact(item, name); found != nil {
				return found
			}
		}
	case []map[string]any:
		for _, item := range node {
			if found := mcpErrorFact(item, name); found != nil {
				return found
			}
		}
	}
	return nil
}
func mcpErrorObject(e *CLIError) map[string]any {
	if e.apiError != nil {
		return e.apiError
	}
	object, _ := e.Details.(map[string]any)
	return object
}
func mcpErrorKey(e *CLIError) string {
	if e.idempotencyKey != "" {
		return e.idempotencyKey
	}
	key, _ := mcpErrorFact(e.Details, "idempotency_key").(string)
	return key
}
func mcpErrorUnknown(e *CLIError) bool {
	return e.requestSent && (mcpErrorFact(e.Details, "outcome") == "unknown" || apiOutcomeUnknown(e))
}
func (a *App) mcpCredentialSource(opts Options) string {
	if strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN")) != "" {
		return "token"
	}
	if credentialFromEnvironment() != "" {
		return "key"
	}
	if workloadIdentityID() != "" {
		return "identity"
	}
	if a.mcpAuth != nil {
		if auth, known := a.mcpAuth.snapshot(); known && (auth.APIKeyID != "" || auth.AuthType == "api_key") {
			return "key"
		}
	}
	if resolved, _, err := a.resolveConfig(opts); err == nil && resolved.APIKeyID != "" {
		return "key"
	}
	return "login"
}
func (a *App) mcpProfileCommand(command string, opts Options) string {
	if a.Registry == nil {
		if opts.Profile != "" {
			return command + " --profile " + quoteRemediationArgument(opts.Profile)
		}
		return command
	}
	if resolved, _, err := a.resolveConfig(opts); err == nil && resolved.ProfileName != "" {
		command += " --profile " + quoteRemediationArgument(resolved.ProfileName)
	}
	return command
}

func (a *App) mcpCredentialAction(cmd *Command, e *CLIError, opts Options, environment string) map[string]any {
	raw := mcpErrorObject(e)
	code := e.Code
	if value, _ := raw["code"].(string); value != "" {
		code = value
	}
	switch code {
	case "API_KEY_REQUIRED", "CREDENTIAL_LOOKUP_FAILED", "CREDENTIAL_FORMAT_INVALID":
		return mcpUserAction("set FLINT_API_KEY or FLINT_PROFILE in this server's MCP configuration, or unlock its credential store, then restart the server.")
	case "IDENTITY_VERIFICATION_REQUIRED":
		action := mcpUserAction("verify their identity in the Dashboard, then ask the agent to try again.")
		action["url"] = dashboardAPIKeysURL
		return action
	case "INSUFFICIENT_SCOPE", "INVALID_CREDENTIAL":
	default:
		if mcpErrorFact(raw, "reason") != "live_mode_required" {
			return nil
		}
	}
	scopes := mcpStrings(mcpErrorFact(raw, "missing_scopes"))
	sort.Strings(scopes)
	source := a.mcpCredentialSource(opts)
	keyModes := mcpStrings(mcpErrorFact(raw, "key_modes"))
	if len(keyModes) == 0 {
		keyModes = mcpStrings(mcpErrorFact(raw, "allowed_key_modes"))
	}
	eligible, liveOnly := missingScopeGuidance(scopes, environment)
	mode, _ := mcpErrorFact(raw, "scope_mode").(string)
	if cmd != nil && mode == "" && a.Registry != nil {
		if index, err := a.mcpOperationIndex(a.mcpPolicy(opts)); err == nil {
			if entry, ok := index.Lookup(cmd.CanonicalName); ok && entry.ScopeRequirement != nil {
				mode = entry.ScopeRequirement.Mode
			}
		}
	}
	liveByMetadata := len(liveOnly) > 0 && (mode != "any" || len(eligible) == 0)
	if mcpErrorFact(raw, "reason") == "live_mode_required" || len(keyModes) == 1 && keyModes[0] == "live" || liveByMetadata {
		text := "use a live credential for this operation"
		if len(scopes) > 0 {
			text += " with scopes " + strings.Join(scopes, ", ")
		}
		text += "; these scopes cannot be added to a sandbox key. Set the credential in this server's MCP configuration and restart with --allow-live."
		action := mcpUserAction(text)
		action["url"] = dashboardAPIKeysURL
		return action
	}
	if mode == "any" && len(eligible) > 0 {
		scopes = eligible[:1]
	}
	if code == "INVALID_CREDENTIAL" {
		switch source {
		case "login":
			action := mcpUserAction("sign in again with the selected profile, then restart this MCP server.")
			action["command"] = a.mcpProfileCommand("flint login", opts)
			return action
		case "key":
			action := mcpUserAction("replace FLINT_API_KEY in this server's MCP configuration with a valid key, then restart the server.")
			action["url"] = dashboardAPIKeysURL
			return action
		default:
			return mcpUserAction("replace the access token or workload credential this server uses, then restart the server.")
		}
	}
	names := strings.Join(scopes, ", ")
	if names == "" {
		names = "the operation's required scopes from flint_describe"
	}
	switch source {
	case "login":
		command := "flint reauth"
		for _, scope := range scopes {
			command += " --scope " + quoteRemediationArgument(scope)
		}
		action := mcpUserAction("grant scopes " + names + " to the saved login, then restart this MCP server.")
		action["command"] = a.mcpProfileCommand(command, opts)
		return action
	case "key":
		action := mcpUserAction("create a restricted API key with scopes " + names + ", set it as FLINT_API_KEY in this server's MCP configuration, then restart the server.")
		action["url"] = dashboardAPIKeysURL
		return action
	default:
		return mcpUserAction("grant scopes " + names + " to this workload identity or use an access token with those scopes, then restart the server.")
	}
}

// A route action is translated only when this server exposes the target tool.
func (a *App) mcpRouteCall(method, path string, body map[string]any, policy mcpPolicy, environment string) map[string]any {
	if a.Registry == nil {
		return nil
	}
	parsed, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(parsed.Path, "/v1/") {
		return nil
	}
	operation, ok := matchPublicOperation(strings.ToUpper(method), parsed.EscapedPath())
	if !ok {
		return nil
	}
	index, err := a.mcpOperationIndex(policy)
	if err != nil {
		return nil
	}
	for _, entry := range index.Entries {
		if entry.OperationID != operation.OperationID || policy.exposureReason(entry) != "" {
			continue
		}
		cmd, ok := a.Registry.ByName(entry.ID)
		if !ok {
			continue
		}
		args := maps.Clone(body)
		if args == nil {
			args = map[string]any{}
		}
		templateParts, pathParts := strings.Split(entry.Path, "/"), strings.Split(parsed.EscapedPath(), "/")
		for i, part := range templateParts {
			if strings.HasPrefix(part, "{") && i < len(pathParts) {
				value, _ := url.PathUnescape(pathParts[i])
				args[strings.Trim(part, "{}")] = value
			}
		}
		for name, values := range parsed.Query() {
			if len(values) == 1 {
				args[name] = values[0]
			} else {
				list := make([]any, len(values))
				for i, v := range values {
					list[i] = v
				}
				args[name] = list
			}
		}
		if environment == "live" && policy.allowLive {
			args["_flint"] = map[string]any{"live": true}
		}
		return a.mcpOperationCall(cmd, args, policy)
	}
	return nil
}

func mcpActionMethod(action map[string]any) string {
	if method, ok := action["method"].(string); ok {
		return method
	}
	kind, _ := action["action_type"].(string)
	switch strings.Split(kind, "_")[0] {
	case "get", "read", "review":
		return "GET"
	case "update":
		return "PATCH"
	case "replace":
		return "PUT"
	case "delete":
		return "DELETE"
	}
	return "POST"
}
func (a *App) mcpAPIActions(e *CLIError, policy mcpPolicy, environment string) []map[string]any {
	raw := mcpErrorObject(e)
	remediation, _ := raw["remediation"].(map[string]any)
	list, _ := remediation["next_actions"].([]any)
	actions := []map[string]any{}
	for _, item := range list {
		action, ok := item.(map[string]any)
		if !ok {
			continue
		}
		text, _ := action["reason_message"].(string)
		if text == "" {
			text = "Follow the API's suggested action."
		}
		path, _ := action["url"].(string)
		if action["requires_human_confirmation"] != true {
			body, _ := action["arguments"].(map[string]any)
			if body == nil {
				body, _ = action["merchant_account_session"].(map[string]any)
			}
			if call := a.mcpRouteCall(mcpActionMethod(action), path, body, policy, environment); call != nil {
				// Required new values need inspection before the target write can be formed.
				if len(mcpStrings(action["required_fields"])) > 0 || mcpActionMethod(action) != "GET" {
					input, _ := call["arguments"].(map[string]any)
					operation, _ := input["operation"].(string)
					if operation == "" {
						if index, err := a.mcpOperationIndex(policy); err == nil {
							if entry, ok := index.Lookup(fmt.Sprint(call["tool"])); ok {
								operation = entry.ID
							}
						}
					}
					call = map[string]any{"tool": "flint_describe", "arguments": map[string]any{"operation": operation}}
					text += " Inspect the target arguments before preparing the requested change."
				}
				actions = append(actions, mcpAgentAction(text, call))
				continue
			}
		}
		next := mcpUserAction(text)
		if path != "" {
			next["url"] = path
		}
		actions = append(actions, next)
	}
	return actions
}

func (a *App) mcpRecoveryActions(cmd *Command, args map[string]any, policy mcpPolicy, key, environment string) []map[string]any {
	actions := []map[string]any{}
	if cmd == nil || a.Registry == nil {
		return actions
	}
	index, err := a.mcpOperationIndex(policy)
	if err != nil {
		return actions
	}
	entry, ok := index.Lookup(cmd.CanonicalName)
	if !ok {
		return actions
	}
	if entry.RecoveryRead != "" {
		read, ok := index.Lookup(entry.RecoveryRead)
		if ok && policy.exposureReason(read) == "" {
			target, exists := a.Registry.ByName(read.ID)
			if exists {
				inputs := map[string]any{"idempotency_key": key}
				// Recovery lists can also be under a resource parent.
				op, _ := openAPIOperationByID(target.OperationID)
				for _, parameter := range op.Parameters {
					if parameter["in"] == "path" {
						name, _ := parameter["name"].(string)
						if value, ok := args[name]; ok {
							inputs[name] = value
						}
					}
				}
				if environment == "live" && policy.allowLive {
					inputs["_flint"] = map[string]any{"live": true}
				}
				actions = append(actions, mcpAgentAction("Look up the result before acting again. An empty list does not prove the write had no effect.", a.mcpOperationCall(target, inputs, policy)))
			}
		}
	}
	if len(actions) > 0 {
		return actions
	}
	path := entry.Path
	for name, value := range args {
		if name != "_flint" {
			path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(fmt.Sprint(value)))
		}
	}
	for _, read := range recoveryReads(entry.Method, path, key) {
		if call := a.mcpRouteCall(read["method"], read["path"], nil, policy, environment); call != nil {
			actions = append(actions, mcpAgentAction("Read the affected resource before acting again.", call))
		}
	}
	if len(actions) > 0 {
		return actions
	}
	if strings.HasSuffix(entry.Path, "}") {
		if call := a.mcpRouteCall("GET", path, nil, policy, environment); call != nil {
			return []map[string]any{mcpAgentAction("Read the affected resource before acting again.", call)}
		}
	}
	parent := path[:max(0, strings.LastIndex(path, "/"))]
	if call := a.mcpRouteCall("GET", parent, nil, policy, environment); call != nil {
		actions = append(actions, mcpAgentAction("Read the affected resource before acting again.", call))
	}
	return actions
}

func (a *App) mcpNextActions(cmd *Command, args map[string]any, opts Options, policy mcpPolicy, e *CLIError, environment string) []map[string]any {
	actions := a.mcpAPIActions(e, policy, environment)
	credential := a.mcpCredentialAction(cmd, e, opts, environment)
	if credential != nil {
		actions = append(actions, credential)
	}
	key := mcpErrorKey(e)
	unknown := cmd != nil && mcpResultIsWrite(cmd, args) && mcpErrorUnknown(e)
	if unknown {
		recovery := a.mcpRecoveryActions(cmd, args, policy, key, environment)
		if len(recovery) == 0 {
			recovery = []map[string]any{mcpUserAction("inspect the affected resources and request logs before acting again.")}
		}
		if commandIdempotencyClass(cmd) != "none" && key != "" {
			recovery[0]["text"] = fmt.Sprint(recovery[0]["text"]) + " If you call this write again, use the same idempotency_key " + quoteRemediationArgument(key) + "."
		}
		return append(actions, recovery...)
	}
	if credential != nil {
		return actions
	}

	switch e.Code {
	case "UNKNOWN_OPERATION":
		query, _ := args["operation"].(string)
		if strings.TrimSpace(query) == "" {
			query, _ = mcpErrorFact(e.Details, "input").(string)
		}
		if strings.TrimSpace(query) == "" {
			query = "operations"
		}
		return append(actions, mcpAgentAction("Search for an operation this server can run.", map[string]any{"tool": "flint_search", "arguments": map[string]any{"query": query}}))
	case "OPERATION_NOT_AVAILABLE":
		switch mcpErrorFact(e.Details, "reason") {
		case "wrong_tool":
			if call, ok := mcpErrorFact(e.Details, "call").(map[string]any); ok {
				return append(actions, mcpAgentAction(e.Message, call))
			}
		case "read_only_server":
			return append(actions, mcpUserAction("restart this MCP server without --read-only to permit writes."))
		case "outside_tool_scope":
			return append(actions, mcpUserAction("restart this MCP server with a wider --tools selection that includes the requested operation."))
		case "credential_type":
			return append(actions, mcpUserAction("restart this MCP server with the credential type this operation accepts; check flint_describe for its accepted credentials."))
		}
	case "CONFIRMATION_NEEDS_USER":
		text := "run this command in an interactive terminal with the same credential this server uses and from the same project directory. The terminal will ask for confirmation."
		if credentialFromEnvironment() != "" {
			text += " The server's key is FLINT_API_KEY in the MCP client's env configuration."
		}
		action := mcpUserAction(text)
		if command, ok := mcpErrorFact(e.Details, "command").(string); ok && command != "" {
			action["command"] = command
		}
		actions = append(actions, action)
		for _, recovery := range a.mcpRecoveryActions(cmd, args, policy, key, environment) {
			recovery["text"] = "After the user says the command ran, " + strings.ToLower(fmt.Sprint(recovery["text"]))
			actions = append(actions, recovery)
		}
		return actions
	case "CONFIRMATION_DECLINED":
		return append(actions, mcpAgentAction("The operation was not performed. Do not retry unless the user asks.", map[string]any{"tool": "flint_help", "arguments": map[string]any{"topic": "safety"}}))
	case "CONFIRMATION_REQUIRED":
		if cmd != nil {
			confirmed := maps.Clone(args)
			if confirmed == nil {
				confirmed = map[string]any{}
			}
			controls, _ := confirmed["_flint"].(map[string]any)
			controls = maps.Clone(controls)
			if controls == nil {
				controls = map[string]any{}
			}
			controls["confirm"] = true
			confirmed["_flint"] = controls
			text := "Check the effect with dry_run, then supply confirm: true."
			if environment != "sandbox" && !policy.allowDestructive {
				text += " The server will ask the user to approve it."
			}
			return append(actions, mcpAgentAction(text, a.mcpOperationCall(cmd, confirmed, policy)))
		}
		return append(actions, mcpUserAction("approve this operation in the MCP client before the agent proceeds."))
	case "LIVE_ACKNOWLEDGEMENT_REQUIRED":
		if cmd != nil && policy.allowLive {
			acknowledged := maps.Clone(args)
			if acknowledged == nil {
				acknowledged = map[string]any{}
			}
			controls, _ := acknowledged["_flint"].(map[string]any)
			controls = maps.Clone(controls)
			if controls == nil {
				controls = map[string]any{}
			}
			controls["live"] = true
			acknowledged["_flint"] = controls
			return append(actions, mcpAgentAction("Acknowledge the live environment with live: true.", a.mcpOperationCall(cmd, acknowledged, policy)))
		}
	case "LIVE_ACCESS_DISABLED":
		return append(actions, mcpUserAction("restart this MCP server with --allow-live, or configure a sandbox credential."))
	case "INVALID_REQUEST", "INVALID_REQUEST_BODY", "UNKNOWN_OUTPUT_FIELD", "RESULT_TOO_LARGE":
		if e.Code == "RESULT_TOO_LARGE" && cmd == nil {
			input := maps.Clone(args)
			if input == nil {
				input = map[string]any{}
			}
			tool := "flint_search"
			if input["operation"] != nil || input["event"] != nil {
				tool = "flint_describe"
				input["depth"] = 1
			} else if input["query"] != nil {
				input["limit"] = 5
			} else {
				input = map[string]any{"query": "webhook events", "limit": 5}
			}
			return append(actions, mcpAgentAction("Narrow the discovery result before inspecting more fields.", map[string]any{"tool": tool, "arguments": input}))
		}
		if cmd != nil {
			input := map[string]any{"operation": cmd.CanonicalName}
			if e.Code == "RESULT_TOO_LARGE" {
				input["kind"] = "result"
			} else if validator, err := requestBodyValidator(cmd); err == nil {
				tag, labels, _, _ := mcpInputUnionMetadata(validator.document, schemaview.New(validator.document))
				if variant, ok := args[tag].(string); ok {
					for _, allowed := range labels {
						if containsString(allowed, variant) {
							input["variant"] = variant
							break
						}
					}
				}
			}
			return append(actions, mcpAgentAction("Inspect the declared fields, correct the named inputs, and call again.", map[string]any{"tool": "flint_describe", "arguments": input}))
		}
	}
	raw := mcpErrorObject(e)
	remediation, _ := raw["remediation"].(map[string]any)
	retry := remediationRetry(remediation)
	if cmd != nil && !unknown && (!mcpResultIsWrite(cmd, args) || e.requestSent) {
		switch retry {
		case "same_request":
			identical := maps.Clone(args)
			if identical == nil {
				identical = map[string]any{}
			}
			controls, _ := identical["_flint"].(map[string]any)
			controls = maps.Clone(controls)
			if controls == nil {
				controls = map[string]any{}
			}
			if key != "" && commandIdempotencyClass(cmd) != "none" {
				controls["idempotency_key"] = key
			}
			identical["_flint"] = controls
			return append(actions, mcpAgentAction("After the supplied delay, resend the identical request.", a.mcpOperationCall(cmd, identical, policy)))
		case "after_refresh":
			refresh := a.mcpRecoveryActions(cmd, args, policy, key, environment)
			for _, action := range refresh {
				text := fmt.Sprint(action["text"]) + " Rebuild the request from the current state."
				if commandIdempotencyClass(cmd) != "none" {
					text += " Use a new idempotency_key."
				}
				action["text"] = text
			}
			if len(refresh) > 0 {
				return append(actions, refresh...)
			}
		}
	}
	if len(actions) > 0 {
		return actions
	}
	return []map[string]any{mcpAgentAction("Inspect the error and follow its next_steps before deciding what to do.", map[string]any{"tool": "flint_help", "arguments": map[string]any{"topic": "errors"}})}
}
