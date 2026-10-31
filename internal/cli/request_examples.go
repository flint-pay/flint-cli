package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type requestExample struct {
	Name string         `json:"name"`
	Body map[string]any `json:"body"`
}

// Media examples take precedence over schema examples. Named examples keep a
// stable order so help and schema input select the same body on every platform.
func operationRequestExamples(operation openAPIOperation) []requestExample {
	content, _ := operation.RequestBody["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	if media == nil {
		return nil
	}
	collect := func(source map[string]any, named bool) []requestExample {
		var out []requestExample
		if body, ok := source["example"].(map[string]any); ok {
			out = append(out, requestExample{"default", body})
		}
		if examples, ok := source["examples"].(map[string]any); ok && named {
			names := make([]string, 0, len(examples))
			for name := range examples {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				example, _ := examples[name].(map[string]any)
				if body, ok := example["value"].(map[string]any); ok {
					out = append(out, requestExample{name, body})
				}
			}
		}
		if examples, ok := source["examples"].([]any); ok && !named {
			for i, value := range examples {
				if body, ok := value.(map[string]any); ok {
					out = append(out, requestExample{fmt.Sprintf("example_%d", i+1), body})
				}
			}
		}
		return out
	}
	if examples := collect(media, true); len(examples) > 0 {
		return examples
	}
	schema, _ := media["schema"].(map[string]any)
	if ref, ok := schema["$ref"].(string); ok {
		if resolved, ok := resolveComponentReference(ref); ok {
			schema = resolved
		}
	}
	return collect(schema, false)
}

func commandRequestExamples(cmd *Command) []requestExample {
	operation, _ := openAPIOperationByID(cmd.OperationID)
	return operationRequestExamples(operation)
}

// Keep complete, valid bodies within the discovery budget; never truncate JSON.
func compactRequestExamples(cmd *Command, variant string) []requestExample {
	var selected []requestExample
	discriminator := ""
	if variant != "" {
		discriminator = commandRequestUnion(cmd).discriminator
	}
	for _, example := range commandRequestExamples(cmd) {
		if discriminator != "" && example.Body[discriminator] != variant {
			continue
		}
		candidate := append(append([]requestExample(nil), selected...), example)
		raw, err := json.Marshal(candidate)
		if err == nil && len(raw) <= 600 {
			selected = candidate
		}
	}
	return selected
}

func specRequestCommandExample(cmd *Command, operation openAPIOperation) string {
	examples := operationRequestExamples(operation)
	if len(examples) == 0 {
		// No unexplained request.json: schema inspection is the available next step.
		return "flint schema input " + cmd.Name + " --output json"
	}
	// publicAPIExample supplies path and required query arguments. Body fields
	// below must come exclusively from the documented example.
	pathAndQuery := []Arg{}
	for _, arg := range cmd.Arguments {
		if arg.BodyPath == "" {
			pathAndQuery = append(pathAndQuery, arg)
		}
	}
	base := []string{publicAPIExample(cmd.Name, pathAndQuery, false)}
	finish := func(parts []string) string {
		if commandIdempotencyClass(cmd) == "required" {
			parts = append(parts, "--idempotency-key", "saved-action-1")
		}
		if operation.FlintDestructive {
			parts = append(parts, "--confirm")
		}
		return strings.Join(parts, " ")
	}
	for _, example := range examples {
		if flags, ok := requestExampleFlags(cmd, example.Body); ok {
			return finish(append(append([]string(nil), base...), flags...))
		}
	}
	body, _ := json.Marshal(examples[0].Body)
	return finish(append(base, "--input", quoteRemediationArgument(string(body))))
}

// Use flags only when they preserve the complete documented body. Objects and
// object arrays without a matching shortcut stay in the inline input example.
func requestExampleFlags(cmd *Command, source map[string]any) ([]string, bool) {
	body := map[string]any{}
	seen := map[string]bool{}
	var flags []string
	for _, arg := range cmd.Arguments {
		if arg.BodyPath == "" || arg.Flag == "" || arg.ArrayEdit != "" || seen[arg.BodyPath] {
			continue
		}
		value, exists := lookupPath(source, arg.BodyPath)
		if !exists {
			continue
		}
		values, ok := exampleFlagValues(arg, value)
		if !ok {
			continue
		}
		if err := setBodyPath(body, arg.BodyPath, value); err != nil {
			continue
		}
		seen[arg.BodyPath] = true
		for _, value := range values {
			flags = append(flags, arg.Flag+"="+quoteRemediationArgument(value))
		}
	}
	if !reflect.DeepEqual(body, source) {
		return nil, false
	}
	return flags, true
}

func exampleFlagValues(arg Arg, value any) ([]string, bool) {
	if value == nil {
		return []string{"null"}, arg.Nullable
	}
	values := []any{value}
	if arg.Repeat {
		var ok bool
		values, ok = value.([]any)
		if !ok || len(values) == 0 {
			return nil, false
		}
	}
	var out []string
	for _, item := range values {
		var text string
		switch item := item.(type) {
		case string:
			text = item
			if arg.Nullable && text == "null" || arg.Repeat && strings.Contains(text, ",") {
				return nil, false
			}
		case bool, float64, json.Number:
			raw, _ := json.Marshal(item)
			text = string(raw)
		default:
			return nil, false
		}
		if _, err := convertValue(text, arg.Type); err != nil {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}
