package cli

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

// Prefix hints and validation must follow the IDs the API documents. A field
// name shared by two resources, such as payment_attempt_id, would otherwise
// steer users to the wrong resource.
func TestResourceIDPrefixesMatchSnapshotExamples(t *testing.T) {
	var doc any
	if err := json.Unmarshal(apispec.OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	exampleID := regexp.MustCompile(`^[a-z]+_[0-9A-Za-z]{10,}$`)
	var mismatches []string
	check := func(name string, example any) {
		prefix := publicResourceIDPrefixes[name]
		id, _ := example.(string)
		if prefix != "" && exampleID.MatchString(id) && !strings.HasPrefix(id, prefix) {
			mismatches = append(mismatches, name+" maps to "+prefix+" but the API documents "+id)
		}
	}
	var walk func(any)
	walk = func(node any) {
		switch value := node.(type) {
		case []any:
			for _, child := range value {
				walk(child)
			}
		case map[string]any:
			if properties, ok := value["properties"].(map[string]any); ok {
				for name, raw := range properties {
					property, _ := raw.(map[string]any)
					check(name, property["example"])
					if items, ok := property["items"].(map[string]any); ok {
						check(strings.TrimSuffix(name, "s"), items["example"])
					}
				}
			}
			if name, ok := value["name"].(string); ok && value["in"] != nil {
				schema, _ := value["schema"].(map[string]any)
				check(name, schema["example"])
			}
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(doc)
	slices.Sort(mismatches)
	mismatches = slices.Compact(mismatches)
	if len(mismatches) > 0 {
		t.Fatalf("resource ID prefixes differ from the API snapshot:\n%s", strings.Join(mismatches, "\n"))
	}
}

func TestPaymentAttemptArgumentsUseTheirOwnPrefixes(t *testing.T) {
	for _, scenario := range []struct{ operationID, argument, prefix, history string }{
		{"blockPaymentIntentAttemptSignals", "payment_attempt_id", "pia_", "payment_attempt"},
		{"getOrderPaymentAttempt", "order_payment_attempt_id", "opat_", "order_payment_attempt"},
	} {
		t.Run(scenario.operationID, func(t *testing.T) {
			operation, ok := openAPIOperationByID(scenario.operationID)
			if !ok {
				t.Fatal("operation missing")
			}
			found := false
			for _, argument := range publicAPIArguments("", operation) {
				if argument.Name == scenario.argument {
					found = true
					if argument.IDPrefix != scenario.prefix || !argument.AcceptsHistoryRef {
						t.Fatalf("argument = %#v", argument)
					}
				}
			}
			if !found {
				t.Fatal("argument missing")
			}
			if got := resourcePrefixes[scenario.prefix]; got != scenario.history {
				t.Fatalf("history resource for %s = %q, want %s", scenario.prefix, got, scenario.history)
			}
		})
	}
}
