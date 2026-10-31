package cli

import (
	"encoding/json"
	"strings"
)

func (secret listenSecret) redacted() bool {
	return secret.source == "flag" || secret.source == "environment"
}

func listenEndpointID(opts Options) string {
	if id := lastRawOption(opts, "webhook-endpoint-id"); id != "" {
		return id
	}
	return "whep_cli_listen"
}

func listenSelectedTypes(opts Options) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, value := range opts.Raw["event-type"] {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item != "" && !seen[item] {
				result = append(result, item)
				seen[item] = true
			}
		}
	}
	return result
}

// Stream read resources can have metadata absent from delivered webhook bodies.
// Keep the properties published for this event, including new envelope fields.
func projectListenEnvelope(raw json.RawMessage, eventType string) (json.RawMessage, error) {
	doc, err := loadOpenAPI()
	if err != nil {
		return nil, err
	}
	schema := doc.Components.Schemas["Webhook_"+strings.ReplaceAll(eventType, ".", "_")+"_merchant"]
	if schema == nil {
		schema = doc.Components.Schemas["MerchantWebhookEnvelope"]
	}
	allowed := map[string]bool{}
	seen := map[string]bool{}
	var visit func(map[string]any)
	visit = func(node map[string]any) {
		if ref, ok := node["$ref"].(string); ok && !seen[ref] {
			seen[ref] = true
			visit(doc.Components.Schemas[strings.TrimPrefix(ref, "#/components/schemas/")])
		}
		properties, _ := node["properties"].(map[string]any)
		for name := range properties {
			allowed[name] = true
		}
		branches, _ := node["allOf"].([]any)
		for _, branch := range branches {
			child, _ := branch.(map[string]any)
			visit(child)
		}
	}
	visit(schema)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	changed := false
	for name := range body {
		if !allowed[name] {
			delete(body, name)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(body)
}
