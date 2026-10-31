package cli

import (
	"fmt"
	"sort"
	"strings"
)

// rootHelpIndex lives beside the tree renderers.

func commandHasRequestBody(cmd *Command) bool {
	if cmd.InputSchema != "" {
		return true
	}
	operation, _ := openAPIOperationByID(cmd.OperationID)
	return operation.RequestBody != nil
}

func requiredBodyFields(cmd *Command) []string {
	schema, e := requestBodySchema(cmd)
	if e != nil {
		return nil
	}
	defs, _ := schema["$defs"].(map[string]any)
	seen, required := map[string]bool{}, map[string]bool{}
	var visit func(map[string]any)
	visit = func(node map[string]any) {
		if ref, ok := node["$ref"].(string); ok && !seen[ref] {
			seen[ref] = true
			target, _ := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
			visit(target)
		}
		if values, ok := node["required"].([]any); ok {
			for _, value := range values {
				required[fmt.Sprint(value)] = true
			}
		}
		if values, ok := node["required"].([]string); ok {
			for _, value := range values {
				required[value] = true
			}
		}
		branches, _ := node["allOf"].([]any)
		for _, branch := range branches {
			child, _ := branch.(map[string]any)
			visit(child)
		}
	}
	visit(schema)
	fields := make([]string, 0, len(required))
	for field := range required {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

func eventHelpIndex() ([]map[string]any, error) {
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(doc.Webhooks))
	for name := range doc.Webhooks {
		names = append(names, name)
	}
	sort.Strings(names)
	events := make([]map[string]any, 0, len(names))
	for _, name := range names {
		path, _ := doc.Webhooks[name].(map[string]any)
		post, _ := path["post"].(map[string]any)
		summary, _ := post["summary"].(string)
		if summary == "" {
			summary, _ = post["description"].(string)
		}
		summary = strings.Join(strings.Fields(summary), " ")
		if line, _, ok := strings.Cut(summary, ". "); ok {
			summary = line + "."
		}
		events = append(events, map[string]any{"type": name, "description": summary})
	}
	return events, nil
}

func eventHelpDetail(name string) (map[string]any, *CLIError) {
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, cliError(ExitSoftware, errorTypeInternal, "SCHEMA_SNAPSHOT_INVALID", err.Error())
	}
	raw, ok := doc.Webhooks[name]
	if !ok {
		return nil, usageError("UNKNOWN_EVENT_TYPE", "Unknown event type: "+name+". Run flint help events to list event types.", "event_type")
	}
	path, _ := raw.(map[string]any)
	return map[string]any{"type": name, "webhook": path, "components": map[string]any{"schemas": reachableSchemaComponents(path, doc.Components.Schemas)}}, nil
}

func errorEnvelopeHelp() map[string]any {
	return map[string]any{"envelope": "{\"error\": {...}}", "fields": map[string]string{
		"type": "Error category.", "code": "Stable error code.", "message": "Human-readable explanation.",
		"param": "Invalid flag or body field, when known.", "details": "Object containing extra context; INVALID_REQUEST_BODY uses problems, UNKNOWN_FLAG and UNKNOWN_COMMAND use input and suggestions (an array).",
		"request_id": "API request identifier, when available.", "http_status": "HTTP response status, when available.", "retryable": "True when retrying the same request is safe and no usable response arrived, Flint returned remediation.retry=same_request, or a non-Flint response has HTTP 429, 500, 502, 503, or 504.",
		"remediation": "Next actions; CLI remediation is also retained under details.remediation.",
	}}
}
