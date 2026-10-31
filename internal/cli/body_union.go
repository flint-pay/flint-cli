package cli

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/flint-pay/flint-cli/internal/schemaview"
)

type requestUnion struct {
	discriminator string
	view          schemaview.Resolved
}

var requestUnions sync.Map

func commandRequestUnion(cmd *Command) requestUnion {
	if cached, ok := requestUnions.Load(cmd.OperationID); ok {
		return cached.(requestUnion)
	}
	operation, _ := openAPIOperationByID(cmd.OperationID)
	_, schema := requestContentSchema(operation.RequestBody)
	resolved, err := argumentReader().Resolve(schema, "")
	if err != nil {
		panic(err)
	}
	var discriminator func(map[string]any) string
	discriminator = func(node map[string]any) string {
		if ref, ok := node["$ref"].(string); ok {
			node, _ := resolveComponentReference(ref)
			return discriminator(node)
		}
		if d, ok := node["discriminator"].(map[string]any); ok {
			name, _ := d["propertyName"].(string)
			return name
		}
		for _, raw := range schemaBranches(node, "allOf") {
			if name := discriminator(raw); name != "" {
				return name
			}
		}
		return ""
	}
	result := requestUnion{discriminator(schema), resolved}
	actual, _ := requestUnions.LoadOrStore(cmd.OperationID, result)
	return actual.(requestUnion)
}

func schemaBranches(schema map[string]any, key string) []map[string]any {
	var out []map[string]any
	if branches, ok := schema[key].([]any); ok {
		for _, raw := range branches {
			if node, ok := raw.(map[string]any); ok {
				out = append(out, node)
			}
		}
	}
	return out
}

func inferRequestUnion(cmd *Command, opts Options, body, input map[string]any) *CLIError {
	if cmd.CanonicalName == "api" || cmd.OperationID == "" {
		return nil
	}
	union := commandRequestUnion(cmd)
	if union.discriminator == "" || union.view.Variants == nil {
		// Optional access-change and sandbox unions deliberately require explicit
		// security choices. These diagnostics never choose a variant for the caller.
		props, _ := union.view.Schema["properties"].(map[string]any)
		if _, accessField := props["access"]; accessField {
			if _, scopes := body["scopes"]; scopes && body["access"] == nil {
				return usageError("MISSING_REQUIRED_ARGUMENT", "Add --access restricted to set scopes (or set access in --input).", "access")
			}
			if _, issueField := props["issue_test_key"]; issueField && body["issue_test_key"] == nil && (body["access"] != nil || body["scopes"] != nil || body["test_key_name"] != nil) {
				return usageError("MISSING_REQUIRED_ARGUMENT", "Add --issue-test-key true to issue a sandbox key (or set issue_test_key in --input).", "issue_test_key")
			}
		}
	} else {
		disc := union.discriminator
		tag, _ := body[disc].(string)
		_, hasTag := body[disc]
		original, inputHasTag := input[disc]
		if inputHasTag && tag != "" && !reflect.DeepEqual(original, body[disc]) {
			return usageError("CONFLICTING_ARGUMENTS", fmt.Sprintf("--%s conflicts with %s from --input.", strings.ReplaceAll(disc, "_", "-"), disc), disc)
		}
		candidates := make([]string, 0, len(union.view.Variants.Requires))
		for value := range union.view.Variants.Requires {
			candidates = append(candidates, value)
		}
		sort.Strings(candidates)
		specific := false
		for _, arg := range cmd.Arguments {
			if arg.BodyPath == "" || len(opts.Raw[strings.TrimPrefix(arg.Flag, "--")]) == 0 {
				continue
			}
			root := strings.Split(arg.BodyPath, ".")[0]
			if root == disc {
				continue
			}
			membership := union.view.Membership[root]
			if len(membership) == 0 {
				continue
			}
			specific = true
			if tag != "" && !slices.Contains(membership, tag) {
				return usageError("FLAG_NOT_IN_VARIANT", fmt.Sprintf("%s applies only when --%s is %s.", arg.Flag, strings.ReplaceAll(disc, "_", "-"), strings.Join(membership, " or ")), arg.Name)
			}
			candidates = slices.DeleteFunc(candidates, func(candidate string) bool { return !slices.Contains(membership, candidate) })
		}
		if !hasTag && specific {
			if len(candidates) != 1 {
				e := usageError("AMBIGUOUS_VARIANT", fmt.Sprintf("These flags do not select one --%s variant. Candidates: %s. Set --%s explicitly and use flags from that variant.", strings.ReplaceAll(disc, "_", "-"), strings.Join(candidates, ", "), strings.ReplaceAll(disc, "_", "-")), disc)
				e.Details = map[string]any{"candidates": candidates}
				return e
			}
			body[disc] = candidates[0]
		}
	}
	for _, arg := range cmd.Arguments {
		if arg.ShortcutTagPath == "" || len(opts.Raw[strings.TrimPrefix(arg.Flag, "--")]) == 0 {
			continue
		}
		tag, present := lookupPath(body, arg.ShortcutTagPath)
		if present && tag != arg.ShortcutTagValue {
			return usageError("FLAG_NOT_IN_VARIANT", fmt.Sprintf("%s requires %s %s.", arg.Flag, arg.ShortcutTagPath, arg.ShortcutTagValue), arg.Name)
		}
		if err := setBodyPath(body, arg.ShortcutTagPath, arg.ShortcutTagValue); err != nil {
			return usageError("CONFLICTING_ARGUMENTS", err.Error(), arg.Name)
		}
	}
	return nil
}
