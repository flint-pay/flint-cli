package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

type bodyValidator struct {
	schema *jsonschema.Schema
	// Top-level fields the schema documents. open means undeclared fields are
	// typed by additionalProperties or patternProperties rather than unknown.
	declared []string
	open     bool
}

var bodyValidators sync.Map

func requestBodyValidator(c *Command) (*bodyValidator, *CLIError) {
	key := c.CanonicalName + "\x00" + c.OperationID + "\x00" + c.InputSchema
	if cached, ok := bodyValidators.Load(key); ok {
		return cached.(*bodyValidator), nil
	}
	schema, e := requestBodySchema(c)
	if e != nil {
		return nil, e
	}
	invalid := func(err error) *CLIError {
		return cliError(ExitUsage, "internal_error", "SCHEMA_SNAPSHOT_INVALID", "Request body schema for "+c.CanonicalName+" is invalid: "+err.Error())
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, invalid(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, invalid(err)
	}
	resourceURL := "https://cli.withflintpay.com/schema/" + c.CanonicalName + "/body.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resourceURL, document); err != nil {
		return nil, invalid(err)
	}
	compiled, err := compiler.Compile(resourceURL)
	if err != nil {
		return nil, invalid(err)
	}
	v := &bodyValidator{schema: compiled}
	declared := map[string]bool{}
	defs, _ := schema["$defs"].(map[string]any)
	v.open = collectDeclaredProperties(schema, defs, declared, map[string]bool{})
	for name := range declared {
		v.declared = append(v.declared, name)
	}
	sort.Strings(v.declared)
	bodyValidators.Store(key, v)
	return v, nil
}

// Collect top-level property names across composed and referenced branches.
func collectDeclaredProperties(schema map[string]any, defs map[string]any, declared, seen map[string]bool) bool {
	open := false
	if ref, ok := schema["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		if target, ok := defs[name].(map[string]any); ok && !seen[name] {
			seen[name] = true
			open = collectDeclaredProperties(target, defs, declared, seen) || open
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for name := range properties {
		declared[name] = true
	}
	if additional, ok := schema["additionalProperties"].(map[string]any); ok && len(additional) > 0 {
		open = true
	}
	if patterns, ok := schema["patternProperties"].(map[string]any); ok && len(patterns) > 0 {
		open = true
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := schema[keyword].([]any)
		for _, raw := range branches {
			if branch, ok := raw.(map[string]any); ok {
				open = collectDeclaredProperties(branch, defs, declared, seen) || open
			}
		}
	}
	return open
}

var flintControlFlags = map[string]string{
	"dry_run": "--dry-run=client", "wait_for": "--wait-for", "max_events": "--max-events",
	"page_size": "--page-size", "page_token": "--page-token", "idempotency_key": "--idempotency-key",
}

// validateRequestBody checks the final JSON body against the command's request
// schema. It returns warnings for undocumented top-level fields.
func validateRequestBody(c *Command, label string, body map[string]any) ([]map[string]any, *CLIError) {
	if controls, ok := body["_flint"]; ok {
		flags := []string{}
		if m, ok := controls.(map[string]any); ok {
			for name := range m {
				flag, known := flintControlFlags[name]
				if !known {
					flag = "--" + strings.ReplaceAll(name, "_", "-")
				}
				flags = append(flags, flag)
			}
		}
		sort.Strings(flags)
		message := "The input contains _flint, which holds execution controls for MCP tool calls only; the CLI would send it to the API as a body field. Remove it and pass the controls as flags"
		if len(flags) > 0 {
			message += ": " + strings.Join(flags, " ")
		} else {
			message += " such as --live, --idempotency-key, or --dry-run=client"
		}
		e := usageError("INVALID_REQUEST_BODY", message+".", "_flint")
		e.Details = []map[string]any{{"path": "$._flint", "message": "_flint is not a request body field", "flags": flags}}
		return nil, e
	}
	v, e := requestBodyValidator(c)
	if e != nil {
		return nil, e
	}
	var warnings []map[string]any
	if !v.open {
		names := make([]string, 0, len(body))
		for name := range body {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !sortedContains(v.declared, name) {
				warning := map[string]any{"path": jsonFieldPath([]string{name}), "message": "not a documented " + label + " request field"}
				if suggestion := closestName(name, v.declared); suggestion != "" {
					warning["suggestion"] = suggestion
					warning["message"] = warning["message"].(string) + "; did you mean " + suggestion + "?"
				}
				warnings = append(warnings, warning)
			}
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, usageError("INVALID_INPUT", err.Error(), "input")
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, usageError("INVALID_INPUT", err.Error(), "input")
	}
	err = v.schema.Validate(instance)
	if err == nil {
		return warnings, nil
	}
	validationErr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return nil, usageError("INVALID_REQUEST_BODY", err.Error(), "input")
	}
	var violations []map[string]any
	reported, seen := map[any]bool{}, map[string]bool{}
	for _, violation := range bodyViolations(validationErr, v.declared) {
		if key := fmt.Sprint(violation["path"], "\x00", violation["message"]); !seen[key] {
			seen[key] = true
			reported[violation["path"]] = true
			violations = append(violations, violation)
		}
	}
	for _, warning := range warnings {
		if !reported[warning["path"]] {
			violations = append(violations, warning)
		}
	}
	lines := make([]string, 0, len(violations))
	for i, violation := range violations {
		if i == 10 {
			lines = append(lines, fmt.Sprintf("  ... and %d more", len(violations)-i))
			break
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", violation["path"], violation["message"]))
	}
	name := strings.Fields(label)[0]
	message := fmt.Sprintf("The request body does not match the %s request schema:\n%s\nRun flint schema input %s to see the expected body.", label, strings.Join(lines, "\n"), name)
	invalid := usageError("INVALID_REQUEST_BODY", message, "input")
	invalid.Details = violations
	return nil, invalid
}

func bodyViolations(e *jsonschema.ValidationError, declared []string) []map[string]any {
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		out := make([]map[string]any, 0, len(k.Missing))
		for _, name := range k.Missing {
			out = append(out, map[string]any{"path": jsonFieldPath(append(append([]string(nil), e.InstanceLocation...), name)), "message": "required field is missing"})
		}
		return out
	case *kind.AdditionalProperties:
		out := make([]map[string]any, 0, len(k.Properties))
		for _, name := range k.Properties {
			violation := map[string]any{"path": jsonFieldPath(append(append([]string(nil), e.InstanceLocation...), name)), "message": "field is not allowed"}
			if len(e.InstanceLocation) == 0 {
				if suggestion := closestName(name, declared); suggestion != "" {
					violation["suggestion"] = suggestion
					violation["message"] = "field is not allowed; did you mean " + suggestion + "?"
				}
			}
			out = append(out, violation)
		}
		return out
	case *kind.OneOf, *kind.AnyOf:
		// Report the closest alternative instead of every branch's failures.
		var best []map[string]any
		for _, cause := range e.Causes {
			candidate := bodyViolations(cause, declared)
			if best == nil || len(candidate) < len(best) {
				best = candidate
			}
		}
		if best != nil {
			return best
		}
	}
	if len(e.Causes) > 0 {
		var out []map[string]any
		for _, cause := range e.Causes {
			out = append(out, bodyViolations(cause, declared)...)
		}
		return out
	}
	message := "this combination of fields is not allowed"
	if _, ok := e.ErrorKind.(*kind.Not); !ok {
		leaf := &jsonschema.ValidationError{ErrorKind: e.ErrorKind}
		message = leaf.BasicOutput().Error.String()
	}
	return []map[string]any{{"path": jsonFieldPath(e.InstanceLocation), "message": message}}
}

func jsonFieldPath(tokens []string) string {
	var b strings.Builder
	b.WriteString("$")
	for _, token := range tokens {
		if _, err := strconv.Atoi(token); err == nil {
			b.WriteString("[" + token + "]")
		} else {
			b.WriteString("." + token)
		}
	}
	return b.String()
}

func closestName(input string, candidates []string) string {
	best, bestDistance := "", 3
	for _, candidate := range candidates {
		if distance := editDistance(strings.ToLower(input), strings.ToLower(candidate)); distance < bestDistance || (distance == bestDistance && best != "" && candidate < best) {
			best, bestDistance = candidate, distance
		}
	}
	if bestDistance > 2 || bestDistance >= len([]rune(input)) {
		return ""
	}
	return best
}

func sortedContains(values []string, target string) bool {
	i := sort.SearchStrings(values, target)
	return i < len(values) && values[i] == target
}
