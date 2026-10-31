package cli

import (
	"github.com/flint-pay/flint-cli/internal/schemaview"
	"maps"
)

// commandDiscoverySchema is the boundary for future describe and typed tools.
// The existing full schema exports and execution validator stay authoritative.
func commandDiscoverySchema(cmd *Command, input bool) (*schemaview.Reader, map[string]any, *CLIError) {
	var schema map[string]any
	var err *CLIError
	if input {
		schema, err = requestBodySchema(cmd)
	} else {
		schema, _ = resourceOutputSchema(cmd)
	}
	if err != nil {
		return nil, nil, err
	}
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	// resourceOutputSchema returns the definitions separately from the resource.
	var defs map[string]any
	if input {
		defs, _ = schema["$defs"].(map[string]any)
	} else {
		_, defs = resourceOutputSchema(cmd)
	}
	doc, _ := loadOpenAPI()
	components := map[string]any{}
	for name, node := range doc.Components.Schemas {
		components[name] = node
	}
	root := map[string]any{"$defs": defs, "components": map[string]any{"schemas": components}}
	if input {
		props := map[string]any{}
		required := []string{}
		if cmd.Local {
			for _, arg := range cmd.Arguments {
				if arg.Name == "input" {
					continue
				}
				kind := arg.Type
				if kind == "time" || kind == "" {
					kind = "string"
				}
				prop := map[string]any{"type": kind, "description": arg.Description}
				if len(arg.Enum) > 0 {
					prop["enum"] = arg.Enum
				}
				if arg.Repeat {
					prop = map[string]any{"type": "array", "items": prop}
				}
				props[arg.Name] = prop
				if arg.Required {
					required = append(required, arg.Name)
				}
			}
		} else if operation, ok := openAPIOperationByID(cmd.OperationID); ok {
			for _, param := range operation.Parameters {
				location, _ := param["in"].(string)
				if location != "path" && location != "query" {
					continue
				}
				name, _ := param["name"].(string)
				property, _ := param["schema"].(map[string]any)
				property = maps.Clone(property)
				if property == nil {
					continue
				}
				if text, ok := param["description"].(string); ok {
					property["description"] = text
				}
				if location == "query" {
					property = queryDiscoverySchema(property)
				}
				props[name] = property
				if param["required"] == true {
					required = append(required, name)
				}
			}
		}
		// Parameters sit outside body variants and survive a closed selected branch.
		schema = map[string]any{"allOf": []any{schema, map[string]any{"type": "object", "properties": props, "required": required}}}
	}
	return schemaview.New(root), schema, nil
}
func queryDiscoverySchema(schema map[string]any) map[string]any {
	schema = maps.Clone(schema)
	if schema["format"] == "date-time" {
		delete(schema, "format")
		description, _ := schema["description"].(string)
		schema["description"] = queryTimeDescription(description)
	}
	if item, ok := schema["items"].(map[string]any); ok {
		schema["items"] = queryDiscoverySchema(item)
	}
	return schema
}
