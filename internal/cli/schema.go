package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"

	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

type openAPIDocument struct {
	OpenAPI           string                                 `json:"openapi"`
	JSONSchemaDialect string                                 `json:"jsonSchemaDialect"`
	FlintAPIVersion   string                                 `json:"x-flint-api-version"`
	FlintCLIScopes    []string                               `json:"x-flint-cli-bootstrap-scopes"`
	Info              map[string]any                         `json:"info"`
	Paths             map[string]map[string]openAPIOperation `json:"paths"`
	Components        struct {
		Schemas map[string]map[string]any `json:"schemas"`
	} `json:"components"`
	Webhooks        map[string]any `json:"webhooks"`
	WebhookMetadata map[string]any `json:"x-flint-webhooks"`
}

func embeddedInitialCLIScopes() []string {
	doc, err := loadOpenAPI()
	if err != nil {
		panic("load embedded OpenAPI bootstrap scopes: " + err.Error())
	}
	if len(doc.FlintCLIScopes) == 0 {
		panic("embedded OpenAPI snapshot has no x-flint-cli-bootstrap-scopes")
	}
	return append([]string(nil), doc.FlintCLIScopes...)
}

type openAPIOperation struct {
	Security            []map[string][]string     `json:"security"`
	OperationID         string                    `json:"operationId"`
	Summary             string                    `json:"summary"`
	Parameters          []map[string]any          `json:"parameters"`
	RequestBody         map[string]any            `json:"requestBody"`
	Responses           map[string]map[string]any `json:"responses"`
	FlintRouteClass     string                    `json:"x-flint-route-class"`
	FlintCLIAction      string                    `json:"x-flint-cli-action"`
	FlintDestructive    bool                      `json:"x-flint-destructive"`
	FlintRequiredScopes []string                  `json:"x-flint-required-scopes"`
	FlintScopesMode     string                    `json:"x-flint-required-scopes-mode"`
}

func openAPIOperationByID(operationID string) (openAPIOperation, bool) {
	doc, err := loadOpenAPI()
	if err != nil {
		return openAPIOperation{}, false
	}
	_ = doc
	operation, ok := openAPIOperations[operationID]
	return operation, ok
}

func validateExpansions(operationID string, values []string) *CLIError {
	if len(values) == 0 {
		return nil
	}
	valid, err := expansionValues(operationID)
	if err != nil {
		return cliError(ExitUsage, "internal_error", "SCHEMA_SNAPSHOT_INVALID", err.Error())
	}
	allowed := map[string]bool{}
	for _, value := range valid {
		allowed[value] = true
	}
	if len(allowed) == 0 {
		return usageError("UNSUPPORTED_EXPANSION", "This command's public API operation does not support expand.", "expand")
	}
	for _, value := range values {
		if !allowed[value] {
			e := usageError("UNSUPPORTED_EXPANSION", fmt.Sprintf("Unsupported expansion %q. Valid expansions: %s.", value, strings.Join(valid, ", ")), "expand")
			return e
		}
	}
	return nil
}

func expansionValues(operationID string) ([]string, error) {
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, methods := range doc.Paths {
		for _, operation := range methods {
			if operation.OperationID != operationID {
				continue
			}
			for _, parameter := range operation.Parameters {
				if parameter["name"] != "expand" {
					continue
				}
				schema, _ := parameter["schema"].(map[string]any)
				items, _ := schema["items"].(map[string]any)
				if enums, ok := items["enum"].([]any); ok {
					for _, value := range enums {
						allowed[fmt.Sprint(value)] = true
					}
				}
			}
		}
	}
	valid := make([]string, 0, len(allowed))
	for candidate := range allowed {
		valid = append(valid, candidate)
	}
	sort.Strings(valid)
	return valid, nil
}

func loadOpenAPI() (openAPIDocument, error) {
	openAPIOnce.Do(func() {
		openAPIErr = json.Unmarshal(apispec.OpenAPI, &openAPICached)
		openAPIOperations = map[string]openAPIOperation{}
		for _, methods := range openAPICached.Paths {
			for _, op := range methods {
				if op.OperationID != "" {
					openAPIOperations[op.OperationID] = op
				}
			}
		}
	})
	return openAPICached, openAPIErr
}

var (
	openAPIOperations map[string]openAPIOperation
	openAPIOnce       sync.Once
	openAPICached     openAPIDocument
	openAPIErr        error
)

func commandDocument(c *Command, requested string) map[string]any {
	b, _ := json.Marshal(c)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	if requested != "" && requested != c.CanonicalName {
		out["name"] = requested
		out["canonical_name"] = c.CanonicalName
		out["command"] = pathWithFlint(strings.Split(requested, ".")...)
		out["canonical_command"] = c.CanonicalPath
	}
	return out
}

func schemaForCommand(c *Command, input bool) (map[string]any, *CLIError) {
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, cliError(ExitUsage, "internal_error", "SCHEMA_SNAPSHOT_INVALID", err.Error())
	}
	name := c.OutputSchema
	if input {
		name = c.InputSchema
	}
	var root map[string]any
	if name != "" {
		var ok bool
		root, ok = doc.Components.Schemas[name]
		if !ok {
			return nil, usageError("SCHEMA_NOT_FOUND", fmt.Sprintf("Schema %s is not present in the public API snapshot.", name), "command")
		}
	}
	if input {
		if operation, ok := openAPIOperationByID(c.OperationID); ok {
			content, _ := operation.RequestBody["content"].(map[string]any)
			jsonContent, _ := content["application/json"].(map[string]any)
			if schema, ok := jsonContent["schema"].(map[string]any); ok {
				if schemaRefName(schema) == "" {
					combined := deepCopyMap(root)
					for key, value := range schema {
						if key == "allOf" && requestSchemaName(operation.RequestBody) != "" {
							continue
						}
						if key == "properties" {
							props, _ := combined["properties"].(map[string]any)
							if props == nil {
								props = map[string]any{}
							}
							for property, definition := range value.(map[string]any) {
								props[property] = definition
							}
							combined[key] = props
						} else {
							combined[key] = value
						}
					}
					root = combined
				}
			}
		}
	}
	if root == nil {
		if input {
			return addCLIArgumentsToSchema(map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false}, c), nil
		}
		return map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema"}, nil
	}
	out := deepCopyMap(root)
	if input {
		applyCommandInputAlternatives(out, c, doc.Components.Schemas)
	}
	out["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	out["$id"] = "https://cli.withflintpay.com/schema/" + c.CanonicalName + map[bool]string{true: "/input", false: "/output"}[input]
	defs := reachableSchemaDefinitions(out, doc.Components.Schemas)
	out = rewriteSchemaRefs(out).(map[string]any)
	if len(defs) > 0 {
		out["$defs"] = defs
	}
	if input {
		out = addCLIArgumentsToSchema(out, c)
	}
	return out, nil
}

func applyCommandInputAlternatives(schema map[string]any, c *Command, schemas map[string]map[string]any) {
	if c.CanonicalName != "payment-intents.create" {
		return
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
		schema["properties"] = properties
	}
	standaloneProperties := map[string]any{"payment_options": deepCopyMap(properties["payment_options"].(map[string]any))}
	if nested, ok := schemas["CreateOrderPaymentIntentRequest"]; ok {
		if nestedProperties, ok := nested["properties"].(map[string]any); ok {
			for name, property := range nestedProperties {
				if _, exists := properties[name]; !exists || name == "payment_options" {
					if propertyMap, ok := property.(map[string]any); ok {
						properties[name] = deepCopyMap(propertyMap)
					} else {
						properties[name] = property
					}
				}
			}
		}
	}
	properties["order"] = map[string]any{"type": "string", "description": "Order ID. The payment amount derives from the order balance."}
	properties["amount"] = map[string]any{"type": "integer", "minimum": 1, "description": "Standalone amount in the smallest currency unit."}
	properties["currency"] = map[string]any{"type": "string", "description": "Standalone ISO currency code."}
	standaloneConditions := schema["allOf"]
	delete(schema, "allOf")
	delete(schema, "required")
	schema["oneOf"] = []any{
		map[string]any{"allOf": standaloneConditions, "properties": standaloneProperties, "required": []string{"amount_money", "payment_options"}, "not": map[string]any{"required": []string{"order"}}},
		map[string]any{"allOf": standaloneConditions, "properties": standaloneProperties, "required": []string{"amount", "currency", "payment_options"}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"order"}}, map[string]any{"required": []string{"amount_money"}}}}},
		map[string]any{"required": []string{"order"}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"amount_money"}}, map[string]any{"required": []string{"amount"}}, map[string]any{"required": []string{"currency"}}}}},
	}
}

func reachableSchemaDefinitions(root map[string]any, schemas map[string]map[string]any) map[string]any {
	definitions := reachableSchemaComponents(root, schemas)
	for name, schema := range definitions {
		definitions[name] = rewriteSchemaRefs(schema)
	}
	return definitions
}

func reachableSchemaComponents(root map[string]any, schemas map[string]map[string]any) map[string]any {
	wanted := map[string]bool{}
	collectComponentRefs(root, wanted)
	defs := map[string]any{}
	for len(wanted) > 0 {
		var name string
		for candidate := range wanted {
			name = candidate
			break
		}
		delete(wanted, name)
		if _, exists := defs[name]; exists {
			continue
		}
		schema, exists := schemas[name]
		if !exists {
			continue
		}
		copy := deepCopyMap(schema)
		collectComponentRefs(copy, wanted)
		defs[name] = copy
	}
	return defs
}

func collectComponentRefs(value any, refs map[string]bool) {
	walkJSONSchemas(value, func(schema map[string]any) {
		if ref, ok := schema["$ref"].(string); ok {
			if name, found := strings.CutPrefix(ref, "#/components/schemas/"); found && name != "" {
				refs[name] = true
			}
		}
	})
}

// Visit only schema-valued keywords. Examples, defaults, enums, and property
// names can contain arbitrary JSON that must not be rewritten as schema syntax.
func walkJSONSchemas(value any, visit func(map[string]any)) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	visit(schema)
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "dependentSchemas"} {
		children, _ := schema[keyword].(map[string]any)
		for _, child := range children {
			walkJSONSchemas(child, visit)
		}
	}
	for _, keyword := range []string{"items", "contains", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "propertyNames", "not", "if", "then", "else", "contentSchema"} {
		walkJSONSchemas(schema[keyword], visit)
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		switch children := schema[keyword].(type) {
		case []any:
			for _, child := range children {
				walkJSONSchemas(child, visit)
			}
		case []map[string]any:
			for _, child := range children {
				walkJSONSchemas(child, visit)
			}
		}
	}
}

func addCLIArgumentsToSchema(schema map[string]any, c *Command) map[string]any {
	// An empty API body still admits path arguments and CLI controls in MCP.
	if schema["maxProperties"] == float64(0) {
		delete(schema, "maxProperties")
	}
	if c.CanonicalName == "api" {
		schema["additionalProperties"] = true
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
		schema["properties"] = properties
	}
	requiredSet := map[string]bool{}
	if existing, ok := schema["required"].([]any); ok {
		for _, v := range existing {
			requiredSet[fmt.Sprint(v)] = true
		}
	}
	for _, arg := range c.Arguments {
		// Body convenience flags are documented in `schema command`; the input
		// schema stays faithful to the public request body so agents do not have
		// to provide both a public field and its CLI shorthand.
		if arg.Name == "input" || (arg.BodyPath != "" && arg.Positional == 0 && arg.Query == "") {
			continue
		}
		property := map[string]any{"type": map[string]string{"integer": "integer", "boolean": "boolean"}[arg.Type], "description": arg.Description}
		if property["type"] == "" {
			property["type"] = "string"
		}
		if arg.Repeat {
			property = map[string]any{"type": "array", "items": property, "description": arg.Description}
			if arg.Required {
				property["minItems"] = 1
			}
		}
		properties[arg.Name] = property
		if arg.Required {
			requiredSet[arg.Name] = true
		}
	}
	controls := map[string]any{
		"live":     map[string]any{"type": "boolean"},
		"merchant": map[string]any{"type": "string"},
		"profile":  map[string]any{"type": "string"},
		"timeout":  map[string]any{"type": "string", "description": "Transport and retry timeout."},
		"debug":    map[string]any{"type": "boolean"},
		"quiet":    map[string]any{"type": "boolean"},
	}
	if c.Mutation {
		controls["idempotency_key"] = map[string]any{"type": "string"}
		controls["dry_run"] = map[string]any{"type": "string", "enum": []string{"client"}}
	}
	if c.CanonicalName == "api" {
		controls["idempotency_key"] = map[string]any{"type": "string"}
		controls["dry_run"] = map[string]any{"type": "string", "enum": []string{"client"}}
		controls["confirm"] = map[string]any{"type": "boolean"}
		controls["preview"] = map[string]any{"type": "boolean"}
		controls["paginate"] = map[string]any{"type": "boolean"}
		controls["page_size"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
		controls["page_token"] = map[string]any{"type": "string"}
		controls["all"] = map[string]any{"type": "boolean"}
	}
	if c.Sensitive || c.Destructive {
		controls["confirm"] = map[string]any{"type": "boolean"}
	}
	if c.CanonicalName == "history" {
		controls["confirm"] = map[string]any{"type": "boolean", "description": "Required when clear is true."}
	}
	if c.Mutation && c.Destructive {
		controls["preview"] = map[string]any{"type": "boolean"}
	}
	if c.Stream {
		controls["max_events"] = map[string]any{"type": "integer", "minimum": 1}
		controls["for"] = map[string]any{"type": "string"}
	}
	if c.Supports.WaitFor {
		controls["wait_for"] = map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}
		controls["for"] = map[string]any{"type": "string"}
	}
	if c.Supports.Pagination && !c.Stream {
		controls["all"] = map[string]any{"type": "boolean"}
	}
	if c.Stream || c.Supports.Pagination || c.Supports.WaitFor || c.CanonicalName == "api" {
		controls["progress"] = map[string]any{"type": "string", "enum": []string{"auto", "plain", "json", "quiet"}}
	}
	if c.Supports.Open {
		controls["open"] = map[string]any{"type": "boolean"}
	}
	if expansions, err := expansionValues(c.OperationID); err == nil && len(expansions) > 0 {
		controls["expand"] = map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "enum": expansions}}
	}
	if c.Supports.Field {
		controls["field"] = map[string]any{"type": "string"}
		controls["select"] = map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}
		controls["jq"] = map[string]any{"type": "string"}
	}
	properties["_flint"] = map[string]any{"type": "object", "description": "Optional CLI execution controls.", "additionalProperties": false, "properties": controls}
	if len(requiredSet) > 0 {
		required := make([]string, 0, len(requiredSet))
		for name := range requiredSet {
			required = append(required, name)
		}
		sort.Strings(required)
		schema["required"] = required
	}
	// CLI arguments accompany the selected request branch in MCP input.
	// Closed public branches must admit those arguments as well.
	if variants, ok := schema["oneOf"].([]any); ok {
		defs, _ := schema["$defs"].(map[string]any)
		for _, raw := range variants {
			branch, _ := raw.(map[string]any)
			if ref, ok := branch["$ref"].(string); ok {
				branch, _ = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
			}
			if branch == nil || branch["additionalProperties"] != false {
				continue
			}
			branchProperties, _ := branch["properties"].(map[string]any)
			for name, property := range properties {
				if _, exists := branchProperties[name]; !exists {
					branchProperties[name] = property
				}
			}
		}
	}
	return schema
}

func rewriteSchemaRefs(value any) any {
	walkJSONSchemas(value, func(schema map[string]any) {
		if ref, ok := schema["$ref"].(string); ok {
			schema["$ref"] = strings.Replace(ref, "#/components/schemas/", "#/$defs/", 1)
		}
		// OAS-only keywords are annotations at this JSON Schema export boundary.
		// Preserve them for readers, including relocated discriminator targets.
		discriminator, _ := schema["discriminator"].(map[string]any)
		mapping, _ := discriminator["mapping"].(map[string]any)
		for action, target := range mapping {
			if ref, ok := target.(string); ok {
				mapping[action] = strings.Replace(ref, "#/components/schemas/", "#/$defs/", 1)
			}
		}
	})
	return value
}

func deepCopyMap(src map[string]any) map[string]any {
	dst := map[string]any{}
	maps.Copy(dst, src)
	b, _ := json.Marshal(dst)
	_ = json.Unmarshal(b, &dst)
	return dst
}

func commandList(reg *Registry) []map[string]any {
	out := make([]map[string]any, 0, len(reg.Commands))
	for _, c := range reg.Commands {
		out = append(out, commandDocument(c, c.Name))
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out
}

func errorCatalog() map[string]any {
	return map[string]any{
		"exit_codes": []map[string]any{{"code": 0, "meaning": "success"}, {"code": 1, "meaning": "api_error"}, {"code": 2, "meaning": "usage_error"}, {"code": 3, "meaning": "auth_or_config_error"}, {"code": 4, "meaning": "confirmation_required"}, {"code": 5, "meaning": "network_or_timeout_error"}, {"code": 6, "meaning": "wait_condition_unmet"}},
		"types":      []string{"usage_error", "validation_error", "authentication_error", "authorization_error", "confirmation_required", "network_error", "timeout_error", "rate_limit_error", "conflict_error", "not_found_error", "unavailable_error"},
		"listen_codes": []map[string]any{
			{"code": "STREAM_BOUND_REQUIRED", "exit_code": ExitUsage},
			{"code": "INVALID_FORWARD_URL", "exit_code": ExitUsage},
			{"code": "INVALID_CURSOR", "exit_code": ExitUsage},
			{"code": "WEBHOOK_SECRET_GENERATION_FAILED", "exit_code": ExitNetwork},
			{"code": "REQUEST_TIMEOUT", "exit_code": ExitNetwork},
			{"code": "NETWORK_ERROR", "exit_code": ExitNetwork},
			{"code": "RESPONSE_READ_FAILED", "exit_code": ExitNetwork},
			{"code": "STREAM_READ_FAILED", "exit_code": ExitNetwork},
			{"code": "INVALID_STREAM_CONTENT_TYPE", "exit_code": ExitAPI},
			{"code": "INVALID_STREAM_EVENT", "exit_code": ExitAPI},
			{"code": "INVALID_STREAM_EVENT_ID", "exit_code": ExitAPI},
			{"code": "INVALID_STREAM_PAYLOAD", "exit_code": ExitAPI},
			{"code": "WEBHOOK_PAYLOAD_NOT_VISIBLE", "exit_code": ExitAuth},
			{"code": "OUTPUT_WRITE_FAILED", "exit_code": ExitNetwork},
		},
	}
}

func eventCatalog() (any, error) {
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, err
	}
	requestSchemas := []any{}
	for _, rawPath := range doc.Webhooks {
		path := rawPath.(map[string]any)
		post := path["post"].(map[string]any)
		body := post["requestBody"].(map[string]any)
		for _, rawMedia := range body["content"].(map[string]any) {
			requestSchemas = append(requestSchemas, rawMedia.(map[string]any)["schema"])
		}
	}
	components := reachableSchemaComponents(map[string]any{"anyOf": requestSchemas}, doc.Components.Schemas)
	return map[string]any{
		"openapi": doc.OpenAPI, "jsonSchemaDialect": doc.JSONSchemaDialect, "info": doc.Info,
		"webhooks": doc.Webhooks, "components": map[string]any{"schemas": components},
		"x-flint-webhooks": doc.WebhookMetadata,
	}, nil
}
