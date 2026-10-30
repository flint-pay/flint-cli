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
	FlintScopes       []openAPIScope                         `json:"x-flint-scopes"`
	FlintCLIScopes    []string                               `json:"x-flint-cli-bootstrap-scopes"`
	FlintCLIAllowed   []string                               `json:"x-flint-cli-allowed-scopes"`
	Info              map[string]any                         `json:"info"`
	Paths             map[string]map[string]openAPIOperation `json:"paths"`
	Components        struct {
		Schemas map[string]map[string]any `json:"schemas"`
	} `json:"components"`
	Webhooks        map[string]any `json:"webhooks"`
	WebhookMetadata map[string]any `json:"x-flint-webhooks"`
}

type openAPIScope struct {
	Name     string   `json:"name"`
	KeyModes []string `json:"key_modes"`
}

// Only explicit live-only metadata rules out obtaining a missing scope in a
// sandbox. Missing or unfamiliar metadata keeps the existing guidance.
func (doc openAPIDocument) scopeNeedsLiveKey(scope, environment string) bool {
	if environment == "sandbox" {
		environment = "test"
	}
	if environment != "test" {
		return false
	}
	for _, metadata := range doc.FlintScopes {
		if metadata.Name == scope {
			return len(metadata.KeyModes) == 1 && metadata.KeyModes[0] == "live"
		}
	}
	return false
}

func missingScopeGuidance(scopes []string, environment string) (eligible, liveOnly []string) {
	doc, _ := loadOpenAPI()
	eligible, liveOnly = []string{}, []string{}
	for _, scope := range scopes {
		if doc.scopeNeedsLiveKey(scope, environment) {
			liveOnly = append(liveOnly, scope)
		} else {
			eligible = append(eligible, scope)
		}
	}
	return eligible, liveOnly
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

func embeddedAllowedCLIScopes() []string {
	doc, err := loadOpenAPI()
	if err != nil {
		panic("load embedded OpenAPI allowed CLI scopes: " + err.Error())
	}
	if len(doc.FlintCLIAllowed) > 0 {
		return append([]string(nil), doc.FlintCLIAllowed...)
	}
	// Older snapshots predate x-flint-cli-allowed-scopes. Every scope used by
	// a public operation is still explicit in its operation metadata.
	set := make(map[string]bool)
	for _, scope := range doc.FlintCLIScopes {
		set[scope] = true
	}
	for _, methods := range doc.Paths {
		for _, operation := range methods {
			for _, scope := range operation.FlintRequiredScopes {
				set[scope] = true
			}
		}
	}
	result := make([]string, 0, len(set))
	for scope := range set {
		result = append(result, scope)
	}
	sort.Strings(result)
	return result
}

type openAPIOperation struct {
	Security            []map[string][]string     `json:"security"`
	OperationID         string                    `json:"operationId"`
	Summary             string                    `json:"summary"`
	Description         string                    `json:"description"`
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
		return cliError(ExitUsage, errorTypeInternal, "SCHEMA_SNAPSHOT_INVALID", err.Error())
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
	if operationID == "" {
		return []string{}, nil
	}
	if _, err := loadOpenAPI(); err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	operation := openAPIOperations[operationID]
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
	expansions, _ := expansionValues(c.OperationID)
	if expansions == nil {
		expansions = []string{}
	}
	out["expansions"] = expansions
	if requested != "" && requested != c.CanonicalName {
		out["name"] = requested
		out["canonical_name"] = c.CanonicalName
		out["command"] = pathWithFlint(strings.Split(requested, ".")...)
		out["canonical_command"] = c.CanonicalPath
	}
	return out
}

// schemaForCommand returns the output schema, or the MCP tool input schema:
// the request body plus path arguments, CLI alternatives, and _flint controls.
func schemaForCommand(c *Command, input bool) (map[string]any, *CLIError) {
	schema, e := commandSchema(c, input, input)
	if e != nil || !input {
		return schema, e
	}
	return addCLIArgumentsToSchema(schema, c), nil
}

// requestBodySchema describes exactly the JSON object that --input supplies.
func requestBodySchema(c *Command) (map[string]any, *CLIError) {
	return commandSchema(c, true, false)
}

func commandSchema(c *Command, input, alternatives bool) (map[string]any, *CLIError) {
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, cliError(ExitUsage, errorTypeInternal, "SCHEMA_SNAPSHOT_INVALID", err.Error())
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
			_, schema := requestContentSchema(operation.RequestBody)
			if schema != nil {
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
	} else if operation, ok := openAPIOperationByID(c.OperationID); ok {
		if schema := responseSchema(operation.Responses); schema != nil && schemaRefName(schema) == "" {
			root = deepCopyMap(schema)
		}
	}
	if root == nil {
		if input {
			return map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false}, nil
		}
		return map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema"}, nil
	}
	out := deepCopyMap(root)
	if alternatives {
		applyCommandInputAlternatives(out, c, doc.Components.Schemas)
	}
	out["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	out["$id"] = "https://cli.withflintpay.com/schema/" + c.CanonicalName + map[bool]string{true: "/input", false: "/output"}[input]
	defs := reachableSchemaDefinitions(out, doc.Components.Schemas)
	out = rewriteSchemaRefs(out).(map[string]any)
	if len(defs) > 0 {
		out["$defs"] = defs
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
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
		schema["properties"] = properties
	}
	if c.CanonicalName == "api" {
		// Older clients send body fields as extra top-level keys; body is the
		// documented form.
		schema["additionalProperties"] = true
		properties["body"] = map[string]any{"type": "object", "description": "JSON request body for POST, PUT, and PATCH routes."}
	}
	requiredSet := map[string]bool{}
	if existing, ok := schema["required"].([]any); ok {
		for _, v := range existing {
			requiredSet[fmt.Sprint(v)] = true
		}
	}
	parameters := operationParameterSchemas(c.OperationID)
	for _, arg := range c.Arguments {
		// Body convenience flags are documented in `schema command`; the input
		// schema stays faithful to the public request body so agents do not have
		// to provide both a public field and its CLI shorthand.
		if arg.Name == "input" || (arg.BodyPath != "" && arg.Positional == 0 && arg.Query == "") {
			continue
		}
		property := map[string]any{"type": map[string]string{"integer": "integer", "number": "number", "boolean": "boolean"}[arg.Type]}
		if property["type"] == "" {
			property["type"] = "string"
		}
		if arg.Type == "integer" && (arg.Name == "page_size" || arg.Query == "page_size") {
			property["minimum"] = 1
			property["maximum"] = 100
		}
		// Query and path parameters keep the enums, defaults, and bounds of the
		// public contract, so agents see the values the API accepts.
		parameter := parameters["path:"+arg.Name]
		if arg.Query != "" {
			parameter = parameters["query:"+arg.Query]
		}
		if arg.Repeat {
			parameter, _ = parameter["items"].(map[string]any)
		}
		for _, keyword := range []string{"enum", "default", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "minLength", "maxLength", "pattern", "format", "x-enum-descriptions"} {
			if value, ok := parameter[keyword]; ok {
				property[keyword] = value
			}
		}
		if arg.Query != "" && arg.Type == "time" {
			delete(property, "format")
		}
		if arg.Repeat {
			property = map[string]any{"type": "array", "items": property}
			if arg.Required {
				property["minItems"] = 1
			}
		}
		description := arg.Description
		if arg.IDPrefix != "" && !strings.Contains(description, arg.IDPrefix) {
			description = strings.TrimSuffix(strings.TrimSpace(description), ".")
			if description == "" {
				description = "Flint ID"
			}
			description += " (" + arg.IDPrefix + "...)"
		}
		if description != "" {
			property["description"] = description
		}
		if c.CanonicalName == "api" {
			switch arg.Name {
			case "method":
				property["enum"] = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "get", "post", "put", "patch", "delete"}
				if arg.Description == "" {
					property["description"] = "HTTP method of the documented public API route."
				}
			case "path":
				if arg.Description == "" {
					property["description"] = "Documented /v1 path with IDs filled in, optionally with a query string, e.g. /v1/customers/cus_123."
				}
			}
		}
		properties[arg.Name] = property
		if arg.Required {
			requiredSet[arg.Name] = true
		}
	}
	if controls := mcpControlSchemas(c); len(controls) > 0 {
		controlSchema := map[string]any{"type": "object", "description": "Execution controls; see help topic controls.", "additionalProperties": false, "properties": controls}
		if commandIdempotencyClass(c) == "required" {
			controlSchema["required"] = []string{"idempotency_key"}
			requiredSet["_flint"] = true
		}
		properties["_flint"] = controlSchema
	}
	if len(requiredSet) > 0 {
		required := make([]string, 0, len(requiredSet))
		for name := range requiredSet {
			required = append(required, name)
		}
		sort.Strings(required)
		schema["required"] = required
	}
	// CLI arguments accompany the selected request branch in MCP input.
	// Closed public branches must admit those arguments as well; the root
	// properties already validate them, so each branch only names them.
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
			for name := range properties {
				if _, exists := branchProperties[name]; !exists {
					branchProperties[name] = map[string]any{}
				}
			}
		}
	}
	return schema
}

// operationParameterSchemas maps "query:NAME" and "path:NAME" to the schema of
// each parameter of a public operation.
func operationParameterSchemas(operationID string) map[string]map[string]any {
	operation, ok := openAPIOperationByID(operationID)
	if !ok {
		return nil
	}
	out := map[string]map[string]any{}
	for _, parameter := range operation.Parameters {
		name, _ := parameter["name"].(string)
		location, _ := parameter["in"].(string)
		if schema, ok := parameter["schema"].(map[string]any); ok && name != "" {
			out[location+":"+name] = schema
		}
	}
	return out
}

// mcpControlSchemas lists the _flint execution controls that apply to a tool.
// Controls that do nothing over MCP (debug, quiet, color, progress, open) are
// not offered; help topic controls explains each one in full.
func mcpControlSchemas(c *Command) map[string]any {
	control := func(schema map[string]any, description string) map[string]any {
		schema["description"] = description
		return schema
	}
	controls := map[string]any{}
	api := c.CanonicalName == "api"
	// Local checks that validate the credential against the API also need the
	// live acknowledgement; only API tools select a profile or merchant guard.
	credentialed := !c.Local || c.CanonicalName == "doctor" || c.CanonicalName == "init" || c.CanonicalName == "config.validate"
	if credentialed {
		controls["live"] = control(map[string]any{"type": "boolean"}, "Acknowledge a live credential.")
	}
	if !c.Local {
		controls["merchant"] = control(map[string]any{"type": "string"}, "Require this mer_ merchant.")
		controls["profile"] = control(map[string]any{"type": "string"}, "CLI profile.")
	}
	if credentialed || c.CanonicalName == "help.search" || c.CanonicalName == "signup" {
		controls["timeout"] = control(map[string]any{"type": "string"}, "Total request budget including retries, e.g. 150s (default).")
	}
	if c.Mutation || api {
		controls["idempotency_key"] = control(map[string]any{"type": "string", "minLength": 1}, "Reuse the same key when retrying, or the write may run twice.")
		controls["dry_run"] = control(map[string]any{"type": "string", "enum": []string{"client"}}, "\"client\": prepare without sending.")
	}
	if c.Sensitive || c.Destructive || api {
		controls["confirm"] = control(map[string]any{"type": "boolean"}, "Confirm this operation.")
	}
	if (c.Mutation && c.Destructive) || api {
		controls["preview"] = control(map[string]any{"type": "boolean"}, "Show the effect without changing anything.")
	}
	if api {
		controls["paginate"] = control(map[string]any{"type": "boolean"}, "Return every page as records. GET only.")
		controls["page_size"] = control(map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "Resources per page.")
		controls["page_token"] = control(map[string]any{"type": "string"}, "next_page_token from a previous page.")
		controls["all"] = control(map[string]any{"type": "boolean"}, "Fetch and merge every page. GET only.")
	}
	if c.Supports.Pagination && !c.Stream {
		controls["all"] = control(map[string]any{"type": "boolean"}, "Fetch every page.")
	}
	if c.Stream {
		controls["max_events"] = control(map[string]any{"type": "integer", "minimum": 1}, "Stop after this many records.")
		controls["for"] = control(map[string]any{"type": "string"}, "Maximum duration, e.g. 30s.")
	}
	if c.Supports.WaitFor {
		controls["wait_for"] = control(map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}, "Poll until each FIELD=VALUE holds.")
		controls["for"] = control(map[string]any{"type": "string"}, "Maximum wait, e.g. 30s.")
	}
	if expansions, err := expansionValues(c.OperationID); err == nil && len(expansions) > 0 {
		controls["expand"] = control(map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "enum": expansions}}, "Include related resources.")
	}
	if c.Supports.Field {
		controls["field"] = control(map[string]any{"type": "string"}, "Return only this dot path.")
		controls["select"] = control(map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}, "Keep only these fields.")
		controls["jq"] = control(map[string]any{"type": "string"}, "jq expression for the result.")
	}
	return controls
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

// commandNameFromWords accepts customers.addresses.create, customers addresses
// create, or a mix, including a leading flint.
func commandNameFromWords(words []string) string {
	fields := strings.Fields(strings.ReplaceAll(strings.Join(words, " "), ".", " "))
	if len(fields) > 1 && fields[0] == "flint" {
		fields = fields[1:]
	}
	return strings.Join(fields, ".")
}

// commandNameSuggestions ranks command names, or with groups also their dotted
// group prefixes, by edit distance. A group named as a command yields its commands.
func commandNameSuggestions(r *Registry, input string, groups bool) []string {
	seen := map[string]bool{}
	var candidates, within []string
	for _, c := range r.Commands {
		if strings.HasPrefix(c.Name, input+".") {
			within = append(within, c.Name)
		}
		parts := strings.Split(c.Name, ".")
		for n := len(parts); n >= 1; n-- {
			candidate := strings.Join(parts[:n], ".")
			if (groups || n == len(parts)) && !seen[candidate] {
				seen[candidate] = true
				candidates = append(candidates, candidate)
			}
		}
	}
	var out []string
	for _, candidate := range candidates {
		if editDistance(input, candidate) <= max(2, len(input)/4) {
			out = append(out, candidate)
		}
	}
	sortStringsByDistance(out, input)
	if len(out) == 0 {
		sort.Strings(within)
		out = within
	}
	return out[:min(3, len(out))]
}

func errorCatalogTypes() []string {
	doc, err := loadOpenAPI()
	if err != nil {
		panic("load embedded OpenAPI error types: " + err.Error())
	}
	types := map[string]bool{}
	for _, typ := range localErrorTypes {
		types[typ] = true
	}
	properties, _ := doc.Components.Schemas["ErrorObject"]["properties"].(map[string]any)
	typeSchema, _ := properties["type"].(map[string]any)
	apiTypes, _ := typeSchema["enum"].([]any)
	for _, raw := range apiTypes {
		if typ, ok := raw.(string); ok {
			types[typ] = true
		}
	}
	result := make([]string, 0, len(types))
	for typ := range types {
		result = append(result, typ)
	}
	sort.Strings(result)
	return result
}

func errorCatalog() map[string]any {
	return map[string]any{
		"exit_codes": []map[string]any{{"code": 0, "meaning": "success"}, {"code": 1, "meaning": "api_error"}, {"code": 2, "meaning": "usage_error"}, {"code": 3, "meaning": "auth_or_config_error"}, {"code": 4, "meaning": "confirmation_required"}, {"code": 5, "meaning": "network_or_timeout_error"}, {"code": 6, "meaning": "wait_condition_unmet"}, {"code": 70, "meaning": "internal_error"}},
		"types":      errorCatalogTypes(),
		"listen_codes": []map[string]any{
			{"code": "STREAM_BOUND_REQUIRED", "exit_code": ExitUsage},
			{"code": "INVALID_FORWARD_URL", "exit_code": ExitUsage},
			{"code": "INVALID_CURSOR", "exit_code": ExitUsage},
			{"code": "INVALID_EVENT_TYPE", "exit_code": ExitUsage},
			{"code": "MULTIPLE_EVENT_TYPES", "exit_code": ExitUsage},
			{"code": "INVALID_WEBHOOK_SECRET", "exit_code": ExitUsage},
			{"code": "LISTEN_SECRET_INVALID", "exit_code": ExitAuth},
			{"code": "LISTEN_SECRET_SAVE_FAILED", "exit_code": ExitAuth},
			{"code": "WEBHOOK_SECRET_GENERATION_FAILED", "exit_code": ExitNetwork},
			{"code": "CLIENT_TIMEOUT", "exit_code": ExitNetwork},
			{"code": "CLIENT_CANCELED", "exit_code": ExitNetwork},
			{"code": "NETWORK_ERROR", "exit_code": ExitNetwork},
			{"code": "RESPONSE_READ_FAILED", "exit_code": ExitNetwork},
			{"code": "STREAM_READ_FAILED", "exit_code": ExitNetwork},
			{"code": "STREAM_NOT_CONNECTED", "exit_code": ExitNetwork},
			{"code": "INVALID_STREAM_CONTENT_TYPE", "exit_code": ExitAPI},
			{"code": "INVALID_STREAM_EVENT", "exit_code": ExitAPI},
			{"code": "INVALID_STREAM_EVENT_ID", "exit_code": ExitAPI},
			{"code": "INVALID_STREAM_PAYLOAD", "exit_code": ExitAPI},
			{"code": "WEBHOOK_FORWARD_FAILED", "exit_code": ExitAPI},
			{"code": "EXPECTED_EVENTS_NOT_RECEIVED", "exit_code": ExitWait},
			{"code": "OUTPUT_WRITE_FAILED", "exit_code": ExitNetwork},
		},
	}
}

func eventCatalog() (any, error) { return eventCatalogFor(nil) }

// eventCatalogFor returns the webhook catalog limited to the event types match
// accepts, with only the components those payloads reach. A nil match keeps
// every event type.
func eventCatalogFor(match func(string) bool) (map[string]any, error) {
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, err
	}
	webhooks := doc.Webhooks
	metadata := doc.WebhookMetadata
	if match != nil {
		webhooks = map[string]any{}
		for eventType, path := range doc.Webhooks {
			if match(eventType) {
				webhooks[eventType] = path
			}
		}
		metadata = map[string]any{}
		for key, value := range doc.WebhookMetadata {
			payloads, ok := value.(map[string]any)
			if !ok || (key != "event_payload_schemas" && key != "partner_event_payload_schemas") {
				metadata[key] = value
				continue
			}
			filtered := map[string]any{}
			for eventType, schema := range payloads {
				if match(eventType) {
					filtered[eventType] = schema
				}
			}
			metadata[key] = filtered
		}
	}
	requestSchemas := []any{}
	for _, rawPath := range webhooks {
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
		"webhooks": webhooks, "components": map[string]any{"schemas": components},
		"x-flint-webhooks": metadata,
	}, nil
}

// eventTypeSummaries lists every webhook event type with its one-line summary.
func eventTypeSummaries() ([]map[string]string, error) {
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, err
	}
	summaries := make([]map[string]string, 0, len(doc.Webhooks))
	for eventType, rawPath := range doc.Webhooks {
		summary, _ := lookupPath(rawPath, "post.summary")
		text, _ := summary.(string)
		summaries = append(summaries, map[string]string{"event_type": eventType, "description": text})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i]["event_type"] < summaries[j]["event_type"] })
	return summaries, nil
}
