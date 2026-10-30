package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/flint-pay/flint-cli/internal/opindex"
	"github.com/flint-pay/flint-cli/internal/schemaview"
)

// These definitions are the approved discovery contract. Each call decodes a fresh
// copy so policy filtering cannot change another connection's tool list.
const mcpDiscoveryDefinitions = `
[
  {
    "name": "flint_search",
    "title": "Find Flint operations",
    "description": "Search the Flint operations this server can run, by task words or a resource name, such as \"refund an order\" or \"webhook deliveries\". Returns operation IDs with their kind (read or write) and whether they need confirm or an idempotency key.",
    "inputSchema": {
      "type": "object",
      "additionalProperties": false,
      "required": [
        "query"
      ],
      "properties": {
        "query": {
          "type": "string",
          "minLength": 1,
          "maxLength": 200,
          "description": "Words describing the task, a resource such as orders, or an operation ID prefix such as orders.line-items."
        },
        "kind": {
          "type": "string",
          "enum": [
            "read",
            "write"
          ],
          "description": "Only read or only write operations."
        },
        "limit": {
          "type": "integer",
          "minimum": 1,
          "maximum": 50,
          "default": 10
        }
      }
    },
    "annotations": {
      "title": "Find Flint operations",
      "readOnlyHint": true,
      "destructiveHint": false,
      "idempotentHint": true,
      "openWorldHint": false
    }
  },
  {
    "name": "flint_describe",
    "title": "Describe a Flint operation",
    "description": "Show one operation's arguments (kind=arguments, the default) or result fields (kind=result), one level at a time. Pass path to open a nested field, and variant for one branch of an operation with several request shapes. Also shows the operation's method and route, required scopes, and its idempotency and confirm rules. Pass event instead of operation for a webhook event's payload.",
    "inputSchema": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "operation": {
          "type": "string",
          "minLength": 1,
          "maxLength": 128,
          "description": "Operation ID from flint_search, such as refunds.create or orders.list."
        },
        "event": {
          "type": "string",
          "minLength": 1,
          "description": "Webhook event type, such as payment_intent.succeeded, instead of operation."
        },
        "kind": {
          "type": "string",
          "enum": [
            "arguments",
            "result"
          ],
          "default": "arguments"
        },
        "path": {
          "type": "string",
          "description": "Dot path of a nested field to open, such as line_items or data.pricing_amounts."
        },
        "variant": {
          "type": "string",
          "description": "Discriminator value of one request shape, such as quick_pay."
        },
        "depth": {
          "type": "integer",
          "minimum": 1,
          "maximum": 3,
          "default": 1,
          "description": "Levels of nested fields to include."
        }
      }
    },
    "annotations": {
      "title": "Describe a Flint operation",
      "readOnlyHint": true,
      "destructiveHint": false,
      "idempotentHint": true,
      "openWorldHint": false
    }
  },
  {
    "name": "flint_read",
    "title": "Run a Flint read operation",
    "description": "Run a read operation (kind read in flint_search), such as orders.list or refunds.get. Changes nothing. Lists return one page and next_page_token; pass it as arguments.page_token.",
    "inputSchema": {
      "type": "object",
      "additionalProperties": false,
      "required": [
        "operation"
      ],
      "properties": {
        "operation": {
          "type": "string",
          "minLength": 1,
          "maxLength": 128,
          "description": "Operation ID from flint_search, such as refunds.create or orders.list."
        },
        "arguments": {
          "type": "object",
          "description": "The operation's path, query, and body arguments by name. flint_describe lists them."
        },
        "select": {
          "type": "array",
          "minItems": 1,
          "maxItems": 50,
          "items": {
            "type": "string",
            "minLength": 1
          },
          "description": "Return only these fields of each resource, as dot paths such as status or pricing_amounts.total_money. Paths come from flint_describe kind=result."
        },
        "all": {
          "type": "boolean",
          "description": "Fetch every page of a list and return them merged, within the result size limit."
        },
        "wait_for": {
          "type": "array",
          "minItems": 1,
          "items": {
            "type": "string",
            "minLength": 3
          },
          "description": "For get operations: poll until each FIELD=VALUE holds, such as status=succeeded."
        },
        "wait_seconds": {
          "type": "integer",
          "minimum": 1,
          "maximum": 120,
          "default": 60,
          "description": "How long wait_for polls."
        }
      }
    },
    "annotations": {
      "title": "Run a Flint read operation",
      "readOnlyHint": true,
      "destructiveHint": false,
      "idempotentHint": true,
      "openWorldHint": true
    }
  },
  {
    "name": "flint_write",
    "title": "Run a Flint write operation",
    "description": "Run an operation that creates, changes, or deletes data (kind write in flint_search), such as refunds.create. Check arguments with dry_run: true first when unsure.",
    "inputSchema": {
      "type": "object",
      "additionalProperties": false,
      "required": [
        "operation"
      ],
      "properties": {
        "operation": {
          "type": "string",
          "minLength": 1,
          "maxLength": 128,
          "description": "Operation ID from flint_search, such as refunds.create or orders.list."
        },
        "arguments": {
          "type": "object",
          "description": "The operation's path, query, and body arguments by name. flint_describe lists them."
        },
        "idempotency_key": {
          "type": "string",
          "minLength": 1,
          "maxLength": 255,
          "description": "Unique key for this write. Required where flint_describe says so. Reuse the same key to retry the same request; a new key can run the write twice."
        },
        "confirm": {
          "type": "boolean",
          "description": "Confirms an operation flint_describe marks confirm. In live mode the user also approves it."
        },
        "dry_run": {
          "type": "boolean",
          "description": "Validate and return the prepared request and its effect without sending it."
        },
        "select": {
          "type": "array",
          "minItems": 1,
          "maxItems": 50,
          "items": {
            "type": "string",
            "minLength": 1
          },
          "description": "Return only these fields of each resource, as dot paths such as status or pricing_amounts.total_money. Paths come from flint_describe kind=result."
        }
      }
    },
    "annotations": {
      "title": "Run a Flint write operation",
      "readOnlyHint": false,
      "destructiveHint": true,
      "idempotentHint": false,
      "openWorldHint": true
    }
  },
  {
    "name": "flint_help",
    "title": "Flint MCP help",
    "description": "Explain this server: topic safety (live access, confirm, dry runs, idempotency keys), errors, events (webhook event types), or test-cards (sandbox payment tokens). Omit topic for an index of resource groups.",
    "inputSchema": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "topic": {
          "type": "string",
          "description": "safety, errors, events, test-cards, or a resource group such as orders."
        }
      }
    },
    "annotations": {
      "title": "Flint MCP help",
      "readOnlyHint": true,
      "destructiveHint": false,
      "idempotentHint": true,
      "openWorldHint": false
    }
  }
]
`

func mcpDiscoveryTools(policy mcpPolicy, version string) []map[string]any {
	var definitions []map[string]any
	_ = json.Unmarshal([]byte(mcpDiscoveryDefinitions), &definitions)
	tools := make([]map[string]any, 0, len(definitions))
	for _, tool := range definitions {
		name := tool["name"].(string)
		if name == "flint_write" && policy.readOnly {
			continue
		}
		if !policy.defaultToolSet() && (name == "flint_read" || name == "flint_write") {
			continue
		}
		if policy.allowLive && (name == "flint_read" || name == "flint_write") {
			properties := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
			properties["live"] = map[string]any{"type": "boolean", "description": "Required on calls that use a live (production) credential."}
		}
		if version < "2025-06-18" {
			delete(tool, "title")
		}
		if version < "2025-03-26" {
			delete(tool, "annotations")
		}
		tools = append(tools, tool)
	}
	return tools
}

func (a *App) mcpOperationIndex(policy mcpPolicy) (*opindex.Index, error) {
	if policy.index != nil {
		return policy.index, nil
	}
	return a.operationIndex()
}

func (a *App) mcpAdvertisedTools(policy mcpPolicy, version string) ([]map[string]any, *CLIError) {
	tools := mcpDiscoveryTools(policy, version)
	if policy.defaultToolSet() {
		return tools, nil
	}
	index, err := a.mcpOperationIndex(policy)
	if err != nil {
		return nil, mcpDiscoverySnapshotError(err)
	}
	policy.index = index
	for _, cmd := range a.mcpTools(policy) {
		schema, e := schemaForMCPTool(cmd, policy)
		if e != nil {
			return nil, e
		}
		entry, _ := index.Lookup(cmd.CanonicalName)
		meta := map[string]any{"com.withflintpay/operation": entry.ID}
		if entry.Sensitive {
			meta["com.withflintpay/sensitive"] = true
		}
		description := strings.ReplaceAll(mcpToolDescription(cmd, schema), "_flint.", "")
		if entry.ConfirmRequired() {
			description += " Needs confirm: true."
		}
		tool := map[string]any{"name": mcpToolName(entry.ID), "description": description, "inputSchema": schema, "_meta": meta}
		if version >= "2025-06-18" {
			tool["title"] = entry.Title
		}
		if version >= "2025-03-26" {
			hints := mcpToolAnnotations(cmd)
			hints["title"] = entry.Title
			hints["readOnlyHint"] = entry.Kind == "read"
			hints["destructiveHint"] = entry.Kind == "write" && (entry.Irreversible || entry.Overwrites)
			hints["idempotentHint"] = entry.Kind == "read" || entry.Method == "PUT" || entry.Method == "DELETE" || entry.Idempotency == "required"
			tool["annotations"] = hints
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func mcpDiscoverySnapshotError(err error) *CLIError {
	return cliError(ExitUsage, errorTypeInternal, "SCHEMA_SNAPSHOT_INVALID", "The embedded discovery contract could not be read: "+err.Error())
}

func (a *App) mcpResolveOperation(name string, policy mcpPolicy) (*Command, opindex.Entry, *CLIError) {
	index, err := a.mcpOperationIndex(policy)
	if err != nil {
		return nil, opindex.Entry{}, mcpDiscoverySnapshotError(err)
	}
	entry, ok := index.Lookup(name)
	if !ok {
		hits, _ := index.Search(name, opindex.Filter{Exposed: func(entry opindex.Entry) bool { return entry.Type == "operation" && policy.exposureReason(entry) == "" }, Limit: 3})
		suggestions := []string{}
		for _, hit := range hits {
			suggestions = append(suggestions, hit.ID)
		}
		e := usageError("UNKNOWN_OPERATION", "Unknown Flint operation: "+name+". Use flint_search to find an operation, or ask the user to upgrade the Flint CLI if the operation is newer.", "operation")
		e.Details = suggestionDetails(name, suggestions)
		return nil, opindex.Entry{}, e
	}
	if reason := policy.exposureReason(entry); reason != "" {
		e := usageError("OPERATION_NOT_AVAILABLE", "This server cannot run "+entry.ID+": "+reason+".", "operation")
		e.Details = map[string]any{"reason": reason, "operation": entry.ID}
		return nil, entry, e
	}
	for _, cmd := range a.mcpAllTools() {
		if cmd.CanonicalName == entry.ID {
			return cmd, entry, nil
		}
	}
	return nil, entry, usageError("OPERATION_NOT_AVAILABLE", "This operation is unavailable on this server.", "operation")
}

// Discovery uses only the connection snapshot or existing credential metadata.
// It does not authenticate, refresh tokens, or contact the API.
func (a *App) mcpKnownScopes() ([]string, bool) {
	if a.mcpAuth != nil {
		if auth, known := a.mcpAuth.snapshot(); known {
			return auth.Scopes, true
		}
	}
	key := credentialFromEnvironment()
	if key == "" {
		return nil, false
	}
	baseURL, err := a.baseURLForCredential(key)
	if err != nil {
		return nil, false
	}
	cached, known := a.cachedAuthContext(baseURL, key)
	return cached.Data.Scopes, known
}

func mcpDiscoverySummary(entry opindex.Entry, index *opindex.Index, scopes []string, known bool) map[string]any {
	if entry.Type == "event" {
		return map[string]any{"event": entry.ID, "title": entry.Title}
	}
	if entry.Type == "topic" {
		return map[string]any{"help_topic": entry.ID, "title": entry.Title}
	}
	result := map[string]any{"operation": entry.ID, "kind": entry.Kind, "title": entry.Title}
	if entry.ConfirmRequired() {
		result["confirm"] = true
	}
	if entry.Idempotency == "required" {
		result["idempotency_key"] = "required"
	}
	if entry.Audience == "buyer" {
		result["credential"] = "buyer"
	}
	if missing := index.MissingScopes(entry, scopes, known); len(missing) > 0 {
		result["missing_scopes"] = missing
	}
	return result
}

func (a *App) mcpDiscovery(tool string, args map[string]any, opts Options, policy mcpPolicy) (any, *CLIError) {
	// Apply the same public schema used by tools/list, including unknown-property checks.
	found := false
	for _, definition := range mcpDiscoveryTools(policy, latestMCPProtocolVersion) {
		if definition["name"] == tool {
			found = true
			if e := mcpInputError(mcpSchemaProblems(definition["inputSchema"].(map[string]any), args, mcpInputBodyLocation, "")); e != nil {
				return nil, e
			}
			break
		}
	}
	if !found {
		return nil, usageError("UNKNOWN_OPERATION", "Unknown discovery tool: "+tool+".", "tool")
	}
	index, err := a.mcpOperationIndex(policy)
	if err != nil {
		return nil, mcpDiscoverySnapshotError(err)
	}
	scopes, known := a.mcpKnownScopes()
	if policy.credential.Schemes == nil {
		known = false
	}
	switch tool {
	case "flint_search":
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) == "" {
			return nil, usageError("INVALID_REQUEST", "query must contain task words or a resource name.", "query")
		}
		kind, _ := args["kind"].(string)
		limit := 10
		if value, exists := args["limit"]; exists {
			n, _ := schemaNumber(value)
			limit = int(n)
		}
		hits, total := index.Search(query, opindex.Filter{Kind: kind, Limit: limit, Exposed: func(e opindex.Entry) bool { return policy.exposureReason(e) == "" }})
		results := make([]map[string]any, 0, len(hits))
		for _, hit := range hits {
			results = append(results, mcpDiscoverySummary(hit.Entry, index, scopes, known))
		}
		value := map[string]any{"query": query, "total": total, "results": results}
		if len(results) == 0 {
			value["hint"] = "Call flint_help without a topic for the resource group index."
		}
		return value, nil
	case "flint_describe":
		return a.mcpDescribe(args, policy, index, scopes, known)
	case "flint_help":
		topic, _ := args["topic"].(string)
		return a.mcpDiscoveryHelp(strings.TrimSpace(topic), opts, policy, index, scopes, known)
	}
	return nil, usageError("UNKNOWN_OPERATION", "Unknown discovery tool.", "tool")
}

func mcpDescribeOptions(args map[string]any) schemaview.Options {
	path, _ := args["path"].(string)
	variant, _ := args["variant"].(string)
	depth := 1
	if value, ok := args["depth"]; ok {
		n, _ := schemaNumber(value)
		depth = int(n)
	}
	return schemaview.Options{Path: path, Variant: variant, Depth: depth}
}

func mcpDescribeView(reader *schemaview.Reader, schema map[string]any, opts schemaview.Options) (schemaview.View, *CLIError) {
	view, err := reader.Describe(schema, opts)
	if err == nil {
		return view, nil
	}
	param := "variant"
	var pathError *schemaview.PathError
	if errors.As(err, &pathError) {
		param = "path"
	}
	if opts.Depth < 1 || opts.Depth > 3 {
		param = "depth"
	}
	e := usageError("INVALID_REQUEST", err.Error(), param)
	e.Details = []map[string]any{{"code": "INVALID_VALUE", "param": param, "message": err.Error()}}
	return schemaview.View{}, e
}

func (a *App) mcpDescribe(args map[string]any, policy mcpPolicy, index *opindex.Index, scopes []string, known bool) (any, *CLIError) {
	operation, _ := args["operation"].(string)
	event, _ := args["event"].(string)
	if (operation == "") == (event == "") {
		return nil, usageError("INVALID_REQUEST", "Pass exactly one of operation or event.", "operation")
	}
	opts := mcpDescribeOptions(args)
	if event != "" {
		return a.mcpDescribeEvent(event, opts, index)
	}
	cmd, entry, e := a.mcpResolveOperation(operation, policy)
	if e != nil {
		return nil, e
	}
	result := map[string]any{"operation": entry.ID, "title": entry.Title, "kind": entry.Kind, "route": "local"}
	if entry.Path != "" {
		result["route"] = entry.Method + " " + entry.Path
	}
	if entry.Description != "" && !strings.EqualFold(strings.TrimSuffix(entry.Description, "."), strings.TrimSuffix(entry.Title, ".")) {
		result["description"] = entry.Description
	}
	if entry.Audience == "buyer" {
		result["credential"] = "buyer"
	}
	if entry.Kind == "write" {
		result["idempotency_key"] = entry.Idempotency
	}
	if entry.ConfirmRequired() {
		result["confirm"] = true
	}
	effects := []string{}
	if entry.Irreversible {
		effects = append(effects, "irreversible")
	}
	if entry.Overwrites {
		effects = append(effects, "overwrites")
	}
	if entry.Sensitive {
		effects = append(effects, "sensitive")
	}
	if len(effects) > 0 {
		result["effects"] = effects
	}
	if entry.ScopeRequirement != nil {
		result["scope_requirement"] = entry.ScopeRequirement
	}
	if missing := index.MissingScopes(entry, scopes, known); len(missing) > 0 {
		result["missing_scopes"] = missing
	}
	if entry.Paginated {
		result["pagination"] = "page_size (default 10 here, max 100), page_token, next_page_token"
	}
	input := args["kind"] != "result"
	public := *cmd
	if entry.Audience == "local" {
		public.Local = true
	}
	reader, schema, e := commandDiscoverySchema(&public, input)
	if e != nil {
		return nil, e
	}
	if input && public.Local {
		resolved, err := reader.Resolve(schema, "")
		if err != nil {
			return nil, mcpDiscoverySnapshotError(err)
		}
		schema = maps.Clone(resolved.Schema)
		props, _ := schema["properties"].(map[string]any)
		props = maps.Clone(props)
		schema["properties"] = props
		if cmd.Stream || entry.ID == "trigger" {
			maximum := 120
			if entry.ID == "trigger" {
				maximum = 60
			}
			props["max_seconds"] = map[string]any{"type": "integer", "minimum": 1, "maximum": maximum, "default": maximum, "description": "Stop after this many seconds."}
			// Terminal duration and count flags become bounded operation arguments.
			delete(props, "for")
		}
		if cmd.Stream {
			props["max_events"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 100, "description": "Stop after this many events."}
		}
	}
	view, e := mcpDescribeView(reader, schema, opts)
	if e != nil {
		return nil, e
	}
	if input {
		if entry.Paginated {
			for _, field := range view.Fields {
				if field["name"] == "page_size" {
					field["default"] = 10
					field["description"] = "Maximum number of results to return, from 1 to 100. Defaults to 10 here."
				}
			}
		}
		result["arguments"] = view.Fields
	} else {
		result["result"] = "data is this resource"
		if entry.Paginated {
			result["result"] = "data is a list of these, with next_page_token"
		}
		if cmd.ResponseMediaType != "" && cmd.ResponseMediaType != "application/json" {
			result["result"] = cmd.ResponseMediaType + " download; save_to writes the file"
		}
		result["fields"] = view.Fields
	}
	if view.Variants != nil {
		result["variants"] = view.Variants
	}
	if view.OneOf != nil {
		result["one_of"] = view.OneOf
	}
	return result, nil
}

func (a *App) mcpDescribeEvent(event string, opts schemaview.Options, index *opindex.Index) (any, *CLIError) {
	var entry opindex.Entry
	for _, candidate := range index.Entries {
		if candidate.Type == "event" && candidate.ID == event {
			entry = candidate
			break
		}
	}
	if entry.ID == "" {
		return nil, usageError("UNKNOWN_EVENT_TYPE", "Unknown webhook event: "+event+". Call flint_help with topic events.", "event")
	}
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, mcpDiscoverySnapshotError(err)
	}
	catalog, e := eventCatalogFor(func(name string) bool { return name == event })
	if e != nil {
		return nil, mcpDiscoverySnapshotError(e)
	}
	reader := schemaview.New(catalog)
	payloads, _ := doc.WebhookMetadata["event_payload_schemas"].(map[string]any)
	schema, _ := payloads[event].(map[string]any)
	if schema == nil {
		value, _ := lookupPath(doc.Webhooks[event], "post.requestBody.content.application/json.schema")
		schema, _ = value.(map[string]any)
	}
	resolved, e := reader.Resolve(schema, "")
	if e != nil {
		return nil, mcpDiscoverySnapshotError(e)
	}
	node := resolved.Schema
	// Public webhook envelopes place the resource in data.object.
	if data, e := reader.Lookup(node, "data.object"); e == nil {
		node = data
	} else if object, e := reader.Lookup(node, "object"); e == nil {
		node = object
	}
	object := strings.Split(event, ".")[0]
	if value, e := reader.Lookup(node, "object"); e == nil {
		objectSchema, err := reader.Resolve(value, "")
		if err == nil {
			if text, ok := objectSchema.Schema["const"].(string); ok {
				object = text
			}
			if values := mcpAnySlice(objectSchema.Schema["enum"]); len(values) == 1 {
				object = fmt.Sprint(values[0])
			}
		}
	}
	view, cliErr := mcpDescribeView(reader, node, opts)
	if cliErr != nil {
		return nil, cliErr
	}
	result := map[string]any{"event": event, "title": entry.Title, "object": object, "fields": view.Fields}
	if view.Variants != nil {
		result["variants"] = view.Variants
	}
	if view.OneOf != nil {
		result["one_of"] = view.OneOf
	}
	return result, nil
}

func (a *App) mcpDiscoveryHelp(topic string, opts Options, policy mcpPolicy, index *opindex.Index, scopes []string, known bool) (any, *CLIError) {
	switch topic {
	case "":
		groups := map[string]int{}
		for _, entry := range index.Entries {
			if entry.Type != "operation" || policy.exposureReason(entry) != "" {
				continue
			}
			group, _, _ := strings.Cut(entry.ID, ".")
			groups[group]++
		}
		return map[string]any{"summary": "Find operations with flint_search, inspect arguments and result fields with flint_describe, then call the operation. Results contain one compact JSON text block and optional notes. Lists default to 10 items; reads have a 49,152-byte limit.", "groups": groups, "topics": []string{"safety", "errors", "events", "test-cards"}}, nil
	case "safety":
		return map[string]any{"topic": "safety", "live": "Without --allow-live this server refuses live credentials. With it, every live call needs live: true. Unverified credentials use live confirmation rules.", "confirm": "Sensitive or irreversible operations need confirm: true. Without confirm the call returns CONFIRMATION_REQUIRED. In a sandbox the agent confirms after checking the effect; elicitation is never used. In live or unverified environments the user must also approve through elicitation; without it CONFIRMATION_NEEDS_USER supplies the exact terminal command, whether confirm is set or not. --allow-destructive permits the agent to confirm live or unverified operations and changes nothing in sandbox. Local operations count as sandbox.", "dry_run": "dry_run: true validates and prepares the request without sending it and needs neither confirmation nor live acknowledgement. Its effect lists resource IDs and amounts formatted for their currency.", "idempotency": "Give every write a unique idempotency_key and reuse it only for the same request after a timeout or network error.", "server": "The agent cannot change the server's profile, merchant guard, context, or startup flags. A read-only server exposes no writes.", "mode": mcpInstructions(policy)}, nil
	case "errors":
		return map[string]any{"topic": "errors", "shape": "{error, cli} in one JSON text block: API errors pass through unchanged, including details arrays and error_source; cli contains origin, operation, exit_code, environment, and next_actions. Local errors use error_source caller; validation failures use INVALID_REQUEST and public details[].param paths.", "origin": []string{"api", "cli"}, "retry": []string{"none", "same_request", "after_refresh"}, "actor": "An agent next action can be followed directly. For a user next action, stop and tell the user.", "outcome": "unknown means a write may have committed. Check its recovery read before acting again; if you call that write again, keep the same idempotency_key. Follow API remediation.retry: same_request resends identical arguments; after_refresh reads current state, rebuilds, and uses a new key; none never automatically resends. Operations whose idempotency class is none use no key.", "cli_codes": []string{"UNKNOWN_OPERATION", "OPERATION_NOT_AVAILABLE", "LIVE_ACCESS_DISABLED", "CONFIRMATION_REQUIRED", "CONFIRMATION_NEEDS_USER", "CONFIRMATION_DECLINED", "RESULT_TOO_LARGE", "CLIENT_TIMEOUT", "CLIENT_CANCELED", "CREDENTIAL_FORMAT_INVALID", "MCP_COMMAND_FAILED"}}, nil
	case "events":
		groups := map[string][]string{}
		count := 0
		for _, entry := range index.Entries {
			if entry.Type != "event" {
				continue
			}
			group, name, _ := strings.Cut(entry.ID, ".")
			groups[group] = append(groups[group], name)
			count++
		}
		for _, names := range groups {
			sort.Strings(names)
		}
		return map[string]any{"groups": groups, "count": count, "next": "Names are grouped by resource: join resource.name for the event type. flint_search finds events; flint_describe with event shows payloads."}, nil
	case "test-cards":
		value, _ := helpTopic("test-cards")
		return value, nil
	}
	if _, ok := index.Lookup(topic); ok {
		return a.mcpDescribe(map[string]any{"operation": topic}, policy, index, scopes, known)
	}
	results := []map[string]any{}
	for _, entry := range index.Entries {
		if entry.Type == "operation" && mcpPrefixMatches(topic, entry.ID) && policy.exposureReason(entry) == "" {
			results = append(results, mcpDiscoverySummary(entry, index, scopes, known))
		}
	}
	if len(results) > 0 {
		return map[string]any{"group": topic, "operations": results}, nil
	}
	return nil, usageError("UNKNOWN_OPERATION", "Unknown Flint help topic or resource group: "+topic+". Call flint_help without a topic for the index.", "topic")
}
