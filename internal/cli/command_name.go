package cli

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var commandSegment = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
var operationFirstWord = regexp.MustCompile(`^[a-z]+`)

var boundOperations = map[string]string{"getAuthContext": "auth.status", "streamWebhookEvents": "listen"}

func commandExclusion(doc *openAPIDocument, path string, operation openAPIOperation) string {
	security := operation.Security
	if security == nil {
		security = doc.Security
	}
	switch {
	case strings.HasPrefix(path, "/v1/oauth/") && len(security) == 0:
		return "E1 unauthenticated OAuth protocol route"
	case path == "/v1/feedback-reports" || strings.HasPrefix(path, "/v1/feedback-reports/"):
		return "E2 feedback goes through support open"
	case operation.FlintAudience == "flint_hosted":
		return "E3 Flint-hosted account only"
	case boundOperations[operation.OperationID] != "":
		return "E4 bound by CLI-owned command " + boundOperations[operation.OperationID]
	}
	return ""
}

// commandName is the route rule. Operation IDs only distinguish POST action
// verbs from nouns; GET collection names come from the response shape.
func commandName(doc *openAPIDocument, path, method string, operation openAPIOperation) (string, string, error) {
	fail := func(reason string) (string, string, error) {
		return "", "", fmt.Errorf("%s %s (%s): %s", method, path, operation.OperationID, reason)
	}
	if !strings.HasPrefix(path, "/v1/") {
		return fail("path is outside /v1/")
	}
	segments := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	var nouns []string
	for _, segment := range segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			continue
		}
		segment = strings.TrimSuffix(segment, ".json")
		if !commandSegment.MatchString(segment) {
			return fail("segment outside the naming rule: " + segment)
		}
		nouns = append(nouns, segment)
	}
	if len(nouns) == 0 {
		return fail("path has no literal segments")
	}
	last := segments[len(segments)-1]
	kind, verb := "singleton", ""
	verbs := map[string]string{"GET": "get", "PATCH": "update", "PUT": "replace", "DELETE": "delete"}
	switch {
	case strings.HasPrefix(last, "{"):
		kind, verb = "item", verbs[method]
	case method == "GET":
		verb = "get"
		if operationReturnsArray(doc, operation) {
			kind, verb = "collection", "list"
		}
	case method == "POST":
		if strings.Split(nouns[len(nouns)-1], "-")[0] == operationFirstWord.FindString(operation.OperationID) {
			return strings.Join(nouns, "."), "action", nil
		}
		kind, verb = "collection", "create"
	default:
		verb = verbs[method]
	}
	if verb == "" {
		return fail("no naming rule for method on this path")
	}
	return strings.Join(append(nouns, verb), "."), kind, nil
}

// Collection naming depends on data's shape, independently of page-token support.
func operationReturnsArray(doc *openAPIDocument, operation openAPIOperation) bool {
	var matches func(map[string]any, int, func(map[string]any) bool) bool
	matches = func(schema map[string]any, depth int, predicate func(map[string]any) bool) bool {
		if schema == nil || depth > 32 {
			return false
		}
		if predicate(schema) {
			return true
		}
		if ref, ok := schema["$ref"].(string); ok {
			if name, local := strings.CutPrefix(ref, "#/components/schemas/"); local && matches(doc.Components.Schemas[name], depth+1, predicate) {
				return true
			}
		}
		if branches, ok := schema["allOf"].([]any); ok {
			for _, branch := range branches {
				if object, ok := branch.(map[string]any); ok && matches(object, depth+1, predicate) {
					return true
				}
			}
		}
		return false
	}
	return matches(responseSchema(operation.Responses), 0, func(schema map[string]any) bool {
		properties, _ := schema["properties"].(map[string]any)
		data, _ := properties["data"].(map[string]any)
		return matches(data, 0, func(schema map[string]any) bool { return schema["type"] == "array" })
	})
}

func validateCommandNames(commands []*Command) error {
	names, tools := map[string]*Command{}, map[string]*Command{}
	reserved := map[string]bool{}
	for _, name := range []string{"api", "completion", "auth", "config", "context", "doctor", "help", "history", "init", "listen", "login", "logout", "mcp", "reauth", "schema", "signup", "support", "trigger", "upgrade", "version", "whoami"} {
		reserved[name] = true
	}
	for _, name := range helpTopicNames {
		reserved[name] = true
	}
	// Sort to make both the failure and the two operation IDs reproducible.
	sorted := append([]*Command(nil), commands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].OperationID < sorted[j].OperationID })
	for _, command := range sorted {
		if other := names[command.Name]; other != nil {
			return fmt.Errorf("command name collision %s: %s and %s", command.Name, other.OperationID, command.OperationID)
		}
		names[command.Name] = command
		tool := mcpToolName(command.Name)
		if other := tools[tool]; other != nil {
			return fmt.Errorf("MCP name collision %s: %s and %s", tool, other.OperationID, command.OperationID)
		}
		tools[tool] = command
		if len(tool) > 64 {
			return fmt.Errorf("MCP name exceeds 64 characters: %s (%s)", tool, command.OperationID)
		}
		words := strings.Split(command.Name, ".")
		if reserved[words[0]] {
			return fmt.Errorf("reserved command word %s (%s)", words[0], command.OperationID)
		}
		if command.Kind == "action" {
			switch words[len(words)-1] {
			case "get", "list", "create", "update", "replace", "delete":
				return fmt.Errorf("action uses a standard verb %s (%s)", command.Name, command.OperationID)
			}
		}
	}
	for _, command := range sorted {
		words := strings.Split(command.Name, ".")
		for n := 1; n < len(words); n++ {
			if other := names[strings.Join(words[:n], ".")]; other != nil {
				return fmt.Errorf("command prefix collision: %s and %s", other.OperationID, command.OperationID)
			}
		}
	}
	return nil
}
