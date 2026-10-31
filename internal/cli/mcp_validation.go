package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/flint-pay/flint-cli/internal/opindex"
	"github.com/flint-pay/flint-cli/internal/schemaview"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// Locations are implicit in public params, but determine their reporting order.
const (
	mcpInputPathLocation = iota
	mcpInputQueryLocation
	mcpInputBodyLocation
	mcpInputControlLocation
)

type mcpInputProblem struct {
	location int
	tokens   []string
	indexes  []bool
	param    string
	code     string
	message  string
	priority int // Discriminator diagnostics replace a plain required/enum error.
}

// mcpValidateInputs keeps execution validation on the full request schema. The
// discovery reader is used only to explain errors and choose a union's failures.
func mcpValidateInputs(cmd *Command, entry opindex.Entry, args, controls map[string]any, policy mcpPolicy) (map[string]any, *CLIError) {
	args = maps.Clone(args)
	if args == nil {
		args = map[string]any{}
	}
	problems := mcpControlProblems(cmd, entry, controls, policy)
	if mcpLocalOperation(cmd) {
		properties := map[string]any{}
		required := []string{}
		for _, arg := range cmd.Arguments {
			typ := arg.Type
			if typ == "" || typ == "time" {
				typ = "string"
			}
			property := map[string]any{"type": typ}
			if len(arg.Enum) > 0 {
				property["enum"] = arg.Enum
			}
			if arg.Repeat {
				property = map[string]any{"type": "array", "items": property}
			}
			properties[arg.Name] = property
			if arg.Required {
				required = append(required, arg.Name)
			}
		}
		if cmd.Stream {
			properties["max_events"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
			properties["max_seconds"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 120}
		}
		if entry.ID == "trigger" {
			properties["max_seconds"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 60}
		}
		schema := map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
		problems = append(problems, mcpSchemaProblems(schema, args, mcpInputBodyLocation, "")...)
	} else {
		operation, ok := openAPIOperationByID(cmd.OperationID)
		if !ok {
			return nil, usageError("UNKNOWN_OPERATION", "The operation is not in the embedded API contract.", "operation")
		}
		body := maps.Clone(args)
		for _, parameter := range operation.Parameters {
			location, _ := parameter["in"].(string)
			if location != "path" && location != "query" {
				continue
			}
			name, _ := parameter["name"].(string)
			value, exists := args[name]
			delete(body, name)
			rank := mcpInputPathLocation
			if location == "query" {
				rank = mcpInputQueryLocation
			}
			if !exists {
				if parameter["required"] == true {
					problems = append(problems, mcpPlainProblem(rank, name, "REQUIRED", name+" is required."))
				}
				continue
			}
			schema, _ := parameter["schema"].(map[string]any)
			if location == "query" {
				schema = queryDiscoverySchema(schema)
				if schema["type"] == "array" {
					if text, ok := value.(string); ok {
						value = []any{text}
						args[name] = value
					}
				}
			}
			problems = append(problems, mcpSchemaProblems(schema, value, rank, name)...)
		}
		validator, e := requestBodyValidator(cmd)
		if e != nil {
			return nil, e
		}
		instance, err := mcpValidationInstance(body)
		if err != nil {
			problems = append(problems, mcpPlainProblem(mcpInputBodyLocation, "body", "INVALID_VALUE", "body must be a JSON object."))
		} else {
			compiled, err := mcpCompileInputSchema(validator.document)
			if err != nil {
				return nil, cliError(ExitUsage, errorTypeInternal, "SCHEMA_SNAPSHOT_INVALID", "The operation's input schema could not be compiled.")
			}
			translator := mcpInputTranslator{document: validator.document, instance: instance, location: mcpInputBodyLocation}
			if failure, ok := compiled.Validate(instance).(*jsonschema.ValidationError); ok {
				problems = append(problems, translator.translate(failure, nil)...)
			}
			// Preserve the existing execution validator's rejection of undocumented
			// fields in named objects even where older snapshots omitted closure.
			defs, _ := validator.document["$defs"].(map[string]any)
			for _, warning := range unknownBodyFields(instance, validator.document, defs, nil, 0) {
				for _, tokens := range mcpLegacyWarningTokens(instance, fmt.Sprint(warning["path"]), validator.document) {
					problem := translator.problem(tokens, "UNKNOWN_FIELD", "", nil)
					if suggestion, ok := warning["suggestion"].(string); ok {
						problem.message += " Did you mean " + suggestion + "?"
						problem.priority = 1
					}
					problems = append(problems, problem)
				}
			}
		}
	}
	if e := mcpInputError(problems); e != nil {
		return nil, e
	}
	return args, nil
}

func mcpControlProblems(cmd *Command, entry opindex.Entry, controls map[string]any, policy mcpPolicy) []mcpInputProblem {
	properties := map[string]any{}
	names := mcpReadControls
	if entry.Kind == "write" {
		names = mcpWriteControls
	}
	for _, name := range names {
		switch name {
		case "live":
			if policy.allowLive {
				properties[name] = map[string]any{"type": "boolean"}
			}
		case "all", "confirm", "dry_run":
			properties[name] = map[string]any{"type": "boolean"}
		case "idempotency_key":
			properties[name] = map[string]any{"type": "string", "minLength": 1, "maxLength": 255}
		case "select", "wait_for":
			property := map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "pattern": `\S`}}
			if name == "select" {
				property["maxItems"] = 50
			}
			properties[name] = property
		case "wait_seconds":
			properties[name] = map[string]any{"type": "integer", "minimum": 1, "maximum": 120}
		}
	}
	required := []string{}
	if entry.Kind == "write" && entry.Idempotency == "required" {
		required = append(required, "idempotency_key")
	}
	schema := map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	if controls == nil {
		controls = map[string]any{}
	}
	problems := mcpSchemaProblems(schema, controls, mcpInputControlLocation, "")
	for i := range problems {
		problem := &problems[i]
		if problem.param == "idempotency_key" && problem.code == "REQUIRED" {
			problem.message = "idempotency_key is required for " + entry.ID + ". Choose a unique key and reuse it if you retry this request."
		}
		if _, known := properties[problem.param]; !known && problem.code == "UNKNOWN_FIELD" && (containsString(mcpReadControls, problem.param) || containsString(mcpWriteControls, problem.param)) {
			problem.code = "NOT_ALLOWED"
			problem.message = problem.param + " is not an available control."
		}
	}
	for name := range controls {
		if name == "all" && !entry.Paginated || (name == "wait_for" || name == "wait_seconds") && !entry.Get {
			problems = append(problems, mcpPlainProblem(mcpInputControlLocation, name, "NOT_ALLOWED", name+" is not allowed for "+entry.ID+"."))
		}
	}
	// Validate each well-typed selection separately to report every bad index.
	if instance, err := mcpValidationInstance(controls["select"]); err == nil {
		if values, ok := instance.([]any); ok {
			for i, value := range values {
				field, ok := value.(string)
				if !ok || strings.TrimSpace(field) == "" {
					continue
				}
				if e := mcpValidateSelect(cmd, map[string]any{"select": []any{field}}); e != nil {
					param := fmt.Sprintf("select[%d]", i)
					problems = append(problems, mcpInputProblem{location: mcpInputControlLocation, tokens: []string{"select", strconv.Itoa(i)}, indexes: []bool{false, true}, param: param, code: "INVALID_VALUE", message: e.Message})
				}
			}
		}
	}
	return problems
}

func mcpValidationInstance(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

var mcpInputSchemas sync.Map

// MCP asserts standard formats without changing the terminal validator's
// compatibility behavior. Unknown/custom formats remain schema annotations.
func mcpCompileInputSchema(document map[string]any) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	key := string(raw)
	if compiled, ok := mcpInputSchemas.Load(key); ok {
		return compiled.(*jsonschema.Schema), nil
	}
	resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource("urn:flint:mcp:input", resource); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile("urn:flint:mcp:input")
	if err != nil {
		return nil, err
	}
	actual, _ := mcpInputSchemas.LoadOrStore(key, compiled)
	return actual.(*jsonschema.Schema), nil
}

func mcpSchemaProblems(schema map[string]any, instance any, location int, fixedParam string) []mcpInputProblem {
	compiled, err := mcpCompileInputSchema(schema)
	value, valueErr := mcpValidationInstance(instance)
	if err == nil && valueErr == nil {
		err = compiled.Validate(value)
	} else if err == nil {
		err = valueErr
	}
	if err == nil {
		return nil
	}
	translator := mcpInputTranslator{document: schema, instance: value, location: location, fixedParam: fixedParam}
	if failure, ok := err.(*jsonschema.ValidationError); ok {
		return translator.translate(failure, nil)
	}
	return []mcpInputProblem{translator.problem(nil, "INVALID_VALUE", "must match the operation's input schema.", nil)}
}

type mcpInputTranslator struct {
	document   map[string]any
	instance   any
	location   int
	fixedParam string // Path/query inputs always use their published parameter name.
}

type mcpInputVariant struct {
	path          []string
	discriminator string
	value         string
	requires      []string
	fields        map[string]any
}

func (t mcpInputTranslator) problem(tokens []string, code, text string, variant *mcpInputVariant) mcpInputProblem {
	param := t.fixedParam
	if param == "" {
		param = mcpPublicInputPath(tokens, t.instance)
	}
	if text == "" {
		switch code {
		case "REQUIRED":
			text = "is required."
			if variant != nil && len(tokens) == len(variant.path)+1 && slices.Equal(tokens[:len(variant.path)], variant.path) && slices.Contains(variant.requires, tokens[len(tokens)-1]) {
				text = "is required when " + variant.discriminator + " is " + variant.value + "."
			}
		case "UNKNOWN_FIELD":
			text = "is not a known field."
		case "NOT_ALLOWED":
			text = "is not allowed."
		default:
			text = "has an invalid value."
		}
	}
	indexes := make([]bool, len(tokens))
	instance := t.instance
	for i, token := range tokens {
		_, indexes[i] = instance.([]any)
		instance = mcpInputValue(instance, []string{token})
	}
	if t.fixedParam != "" {
		tokens, indexes = []string{t.fixedParam}, nil
	}
	return mcpInputProblem{location: t.location, tokens: slices.Clone(tokens), indexes: indexes, param: param, code: code, message: param + " " + text}
}

func (t mcpInputTranslator) translate(e *jsonschema.ValidationError, variant *mcpInputVariant) []mcpInputProblem {
	path := e.InstanceLocation
	node := t.schemaAt(e.SchemaURL)
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		out := []mcpInputProblem{}
		discriminator, labels, _, _ := mcpInputUnionMetadata(node, schemaview.New(t.document))
		for _, name := range k.Missing {
			problem := t.problem(append(slices.Clone(path), name), "REQUIRED", "", variant)
			if name == discriminator {
				allowed := []string{}
				for _, values := range labels {
					allowed = append(allowed, values...)
				}
				allowed = slices.Compact(slices.Sorted(slices.Values(allowed)))
				problem.message = problem.param + " is required; allowed values are " + strings.Join(allowed, ", ") + "."
				problem.priority = 2
			}
			out = append(out, problem)
		}
		return out
	case *kind.Dependency:
		out := []mcpInputProblem{}
		for _, name := range k.Missing {
			out = append(out, t.problem(append(slices.Clone(path), name), "REQUIRED", "is required when "+mcpPublicInputPath(append(slices.Clone(path), k.Prop), t.instance)+" is provided.", nil))
		}
		return out
	case *kind.DependentRequired:
		out := []mcpInputProblem{}
		for _, name := range k.Missing {
			out = append(out, t.problem(append(slices.Clone(path), name), "REQUIRED", "is required when "+mcpPublicInputPath(append(slices.Clone(path), k.Prop), t.instance)+" is provided.", nil))
		}
		return out
	case *kind.AdditionalProperties:
		out := []mcpInputProblem{}
		reader := schemaview.New(t.document)
		resolved, _ := reader.Resolve(node, "")
		fields, _ := resolved.Schema["properties"].(map[string]any)
		for _, name := range k.Properties {
			code := "UNKNOWN_FIELD"
			if variant != nil && slices.Equal(path, variant.path) {
				if _, known := variant.fields[name]; known {
					code = "NOT_ALLOWED"
				}
			}
			problem := t.problem(append(slices.Clone(path), name), code, "", variant)
			if code == "UNKNOWN_FIELD" {
				if suggestion := closestName(name, slices.Sorted(maps.Keys(fields))); suggestion != "" {
					problem.message += " Did you mean " + suggestion + "?"
					problem.priority = 1
				}
			}
			out = append(out, problem)
		}
		return out
	case *kind.OneOf, *kind.AnyOf:
		return t.unionProblems(e, node, variant)
	case *kind.Not:
		if object, ok := mcpInputValue(t.instance, path).(map[string]any); ok {
			if forbidden, ok := node["not"].(map[string]any); ok {
				fields := mcpNotFields(forbidden, object)
				if len(fields) > 0 {
					out := []mcpInputProblem{}
					for _, name := range fields {
						out = append(out, t.problem(append(slices.Clone(path), name), "NOT_ALLOWED", "", variant))
					}
					return out
				}
			}
		}
		return []mcpInputProblem{t.problem(path, "NOT_ALLOWED", "contains a combination of fields that is not allowed.", variant)}
	case *kind.FalseSchema:
		code := "NOT_ALLOWED"
		if strings.HasSuffix(e.SchemaURL, "/unevaluatedProperties") {
			code = "UNKNOWN_FIELD"
		}
		return []mcpInputProblem{t.problem(path, code, "", variant)}
	case *kind.PropertyNames:
		return []mcpInputProblem{t.problem(append(slices.Clone(path), k.Property), "INVALID_VALUE", "has an invalid field name.", nil)}
	case *kind.UniqueItems:
		return []mcpInputProblem{t.problem(append(slices.Clone(path), strconv.Itoa(k.Duplicates[1])), "INVALID_VALUE", "duplicates an earlier item.", nil)}
	}
	if len(e.Causes) > 0 {
		out := []mcpInputProblem{}
		for _, cause := range e.Causes {
			out = append(out, t.translate(cause, variant)...)
		}
		return out
	}
	return []mcpInputProblem{t.problem(path, "INVALID_VALUE", mcpInputConstraintMessage(e.ErrorKind), variant)}
}

func (t mcpInputTranslator) unionProblems(e *jsonschema.ValidationError, node map[string]any, parent *mcpInputVariant) []mcpInputProblem {
	// A schema can combine a discriminated oneOf with an independent anyOf
	// constraint. Each error belongs to the keyword that actually failed.
	if _, anyOf := e.ErrorKind.(*kind.AnyOf); anyOf && node["oneOf"] != nil {
		node = maps.Clone(node)
		delete(node, "oneOf")
		delete(node, "discriminator")
	}
	reader := schemaview.New(t.document)
	name, labels, required, fields := mcpInputUnionMetadata(node, reader)
	if name != "" {
		if object, ok := mcpInputValue(t.instance, e.InstanceLocation).(map[string]any); ok {
			value, exists := object[name]
			label, _ := value.(string)
			allowed := []string{}
			selected := -1
			for i, values := range labels {
				allowed = append(allowed, values...)
				if slices.Contains(values, label) {
					selected = i
				}
			}
			allowed = slices.Compact(slices.Sorted(slices.Values(allowed)))
			if !exists || selected < 0 {
				code, text := "INVALID_VALUE", "must be one of "+strings.Join(allowed, ", ")+"."
				if !exists {
					code, text = "REQUIRED", "is required; allowed values are "+strings.Join(allowed, ", ")+"."
				}
				problem := t.problem(append(slices.Clone(e.InstanceLocation), name), code, text, nil)
				problem.priority = 2
				return []mcpInputProblem{problem}
			}
			if selected < len(e.Causes) {
				variant := &mcpInputVariant{path: e.InstanceLocation, discriminator: mcpPublicInputPath(append(slices.Clone(e.InstanceLocation), name), t.instance), value: label, requires: required[selected], fields: fields}
				return t.translate(e.Causes[selected], variant)
			}
		}
	}
	// Structural unions retain the existing closest-alternative behavior.
	var best []mcpInputProblem
	for _, cause := range e.Causes {
		candidate := mcpOrderedInputProblems(t.translate(cause, parent))
		if best == nil || len(candidate) < len(best) {
			best = candidate
		}
	}
	if len(best) > 0 {
		return best
	}
	return []mcpInputProblem{t.problem(e.InstanceLocation, "INVALID_VALUE", "must match exactly one allowed combination of fields.", parent)}
}

// Resolve each branch independently for metadata. Combining a shared enum and
// a branch const into a discovery allOf can lose its fixed tag in a lossy view.
func mcpInputUnionMetadata(node map[string]any, reader *schemaview.Reader) (string, [][]string, [][]string, map[string]any) {
	branches, _ := node["oneOf"].([]any)
	if len(branches) == 0 {
		branches, _ = node["anyOf"].([]any)
	}
	properties := make([]map[string]any, len(branches))
	required := make([][]string, len(branches))
	fields := map[string]any{}
	if view, err := reader.Resolve(node, ""); err == nil {
		if props, ok := view.Schema["properties"].(map[string]any); ok {
			maps.Copy(fields, props)
		}
	}
	for i, raw := range branches {
		branch, _ := raw.(map[string]any)
		view, _ := reader.Resolve(branch, "")
		properties[i], _ = view.Schema["properties"].(map[string]any)
		required[i] = mcpSchemaStrings(view.Schema["required"])
		maps.Copy(fields, properties[i])
	}
	fixed := func(i int, name string) []string {
		property, _ := properties[i][name].(map[string]any)
		view, _ := reader.Resolve(property, "")
		if value, ok := view.Schema["const"].(string); ok {
			return []string{value}
		}
		return mcpSchemaStrings(view.Schema["enum"])
	}
	discriminator, _ := node["discriminator"].(map[string]any)
	name, _ := discriminator["propertyName"].(string)
	if name == "" && len(properties) > 0 {
		for _, candidate := range slices.Sorted(maps.Keys(properties[0])) {
			seen := map[string]bool{}
			valid := true
			for i := range branches {
				values := fixed(i, candidate)
				if len(values) == 0 {
					valid = false
					break
				}
				for _, value := range values {
					if seen[value] {
						valid = false
					}
					seen[value] = true
				}
			}
			if valid {
				name = candidate
				break
			}
		}
	}
	labels := make([][]string, len(branches))
	mapping, _ := discriminator["mapping"].(map[string]any)
	for i, raw := range branches {
		labels[i] = fixed(i, name)
		branch, _ := raw.(map[string]any)
		if len(labels[i]) == 0 {
			for label, ref := range mapping {
				if ref != nil && ref == branch["$ref"] {
					labels[i] = append(labels[i], label)
				}
			}
		}
	}
	if len(required) > 0 {
		common := slices.Clone(required[0])
		for _, values := range required[1:] {
			common = slices.DeleteFunc(common, func(name string) bool { return !slices.Contains(values, name) })
		}
		common = append(common, mcpSchemaStrings(node["required"])...)
		for i, values := range required {
			required[i] = slices.DeleteFunc(slices.Clone(values), func(name string) bool { return slices.Contains(common, name) })
		}
	}
	return name, labels, required, fields
}

func mcpSchemaStrings(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		out := []string{}
		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

func mcpNotFields(node map[string]any, object map[string]any) []string {
	if required := mcpSchemaStrings(node["required"]); len(required) > 0 && len(node) == 1 {
		for _, field := range required {
			if _, exists := object[field]; !exists {
				return nil
			}
		}
		// For an unordered exclusion, the input to remove sorts last.
		required = slices.Sorted(slices.Values(required))
		return required[len(required)-1:]
	}
	if branches, ok := node["anyOf"].([]any); ok && len(node) == 1 {
		out := []string{}
		for _, raw := range branches {
			branch, _ := raw.(map[string]any)
			out = append(out, mcpNotFields(branch, object)...)
		}
		return out
	}
	return nil
}

func mcpInputConstraintMessage(constraint jsonschema.ErrorKind) string {
	switch k := constraint.(type) {
	case *kind.Type:
		return "must be " + strings.Join(k.Want, " or ") + "."
	case *kind.Enum:
		values := []string{}
		for _, value := range k.Want {
			if _, object := value.(map[string]any); object {
				return "must be an allowed value."
			}
			if _, array := value.([]any); array {
				return "must be an allowed value."
			}
			values = append(values, fmt.Sprint(value))
		}
		return "must be one of " + strings.Join(values, ", ") + "."
	case *kind.Const:
		switch k.Want.(type) {
		case map[string]any, []any:
			return "must be the required value."
		}
		return "must be " + fmt.Sprint(k.Want) + "."
	case *kind.Format:
		return "must be a valid " + k.Want + "."
	case *kind.Pattern:
		return "must match the required pattern."
	case *kind.MinLength:
		return fmt.Sprintf("must contain at least %d characters.", k.Want)
	case *kind.MaxLength:
		return fmt.Sprintf("must contain at most %d characters.", k.Want)
	case *kind.MinItems:
		return fmt.Sprintf("must contain at least %d items.", k.Want)
	case *kind.MaxItems:
		return fmt.Sprintf("must contain at most %d items.", k.Want)
	case *kind.MinProperties:
		return fmt.Sprintf("must contain at least %d fields.", k.Want)
	case *kind.MaxProperties:
		return fmt.Sprintf("must contain at most %d fields.", k.Want)
	case *kind.Minimum:
		return "must be at least " + mcpInputBound(k.Want) + "."
	case *kind.Maximum:
		return "must be at most " + mcpInputBound(k.Want) + "."
	case *kind.ExclusiveMinimum:
		return "must be greater than " + mcpInputBound(k.Want) + "."
	case *kind.ExclusiveMaximum:
		return "must be less than " + mcpInputBound(k.Want) + "."
	case *kind.MultipleOf:
		return "must be a multiple of " + k.Want.RatString() + "."
	}
	return "has an invalid value."
}

func mcpInputBound(bound *big.Rat) string {
	value, _ := bound.Float64()
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func (t mcpInputTranslator) schemaAt(location string) map[string]any {
	_, fragment, _ := strings.Cut(location, "#")
	fragment, _ = url.PathUnescape(fragment)
	var value any = t.document
	for _, token := range strings.Split(strings.TrimPrefix(fragment, "/"), "/") {
		if token == "" {
			continue
		}
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch node := value.(type) {
		case map[string]any:
			value = node[token]
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(node) {
				return nil
			}
			value = node[index]
		default:
			return nil
		}
	}
	node, _ := value.(map[string]any)
	return node
}

func mcpInputValue(value any, tokens []string) any {
	for _, token := range tokens {
		switch node := value.(type) {
		case map[string]any:
			value = node[token]
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(node) {
				return nil
			}
			value = node[index]
		default:
			return nil
		}
	}
	return value
}

func mcpPublicInputPath(tokens []string, instance any) string {
	if len(tokens) == 0 {
		return "body"
	}
	var path strings.Builder
	for i, token := range tokens {
		if _, array := instance.([]any); array {
			path.WriteString("[" + token + "]")
		} else if token != "" && strings.IndexFunc(token, func(r rune) bool {
			return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-')
		}) < 0 {
			if i > 0 {
				path.WriteByte('.')
			}
			path.WriteString(token)
		} else {
			raw, _ := json.Marshal(token)
			path.WriteByte('[')
			path.Write(raw)
			path.WriteByte(']')
		}
		instance = mcpInputValue(instance, []string{token})
	}
	return path.String()
}

// The older warning walker formats paths with a $. prefix. Recover tokens from
// the actual instance so dotted map keys and numeric object keys remain exact.
func mcpLegacyWarningTokens(value any, target string, document map[string]any) [][]string {
	var out [][]string
	defs, _ := document["$defs"].(map[string]any)
	var visit func(any, []string, map[string]any)
	visit = func(value any, path []string, schema map[string]any) {
		if len(path) > 64 {
			return
		}
		switch value := value.(type) {
		case map[string]any:
			properties, open := bodyProperties(schema, defs, map[string]bool{})
			for name, child := range value {
				childPath := append(slices.Clone(path), name)
				property, known := properties[name]
				if !known && !open && len(properties) > 0 && jsonFieldPath(childPath) == target {
					out = append(out, childPath)
				} else if known {
					childSchema, _ := property.(map[string]any)
					visit(child, childPath, childSchema)
				}
			}
		case []any:
			items := bodyArrayItems(schema, defs, map[string]bool{})
			for i, child := range value {
				visit(child, append(slices.Clone(path), strconv.Itoa(i)), items)
			}
		}
	}
	visit(value, nil, document)
	return out
}

func mcpPlainProblem(location int, param, code, message string) mcpInputProblem {
	return mcpInputProblem{location: location, tokens: []string{param}, param: param, code: code, message: message}
}

func mcpOrderedInputProblems(problems []mcpInputProblem) []mcpInputProblem {
	sort.SliceStable(problems, func(i, j int) bool {
		a, b := problems[i], problems[j]
		if a.location != b.location {
			return a.location < b.location
		}
		if !slices.Equal(a.tokens, b.tokens) {
			for index := 0; index < min(len(a.tokens), len(b.tokens)); index++ {
				if a.tokens[index] != b.tokens[index] {
					left, le := strconv.Atoi(a.tokens[index])
					right, re := strconv.Atoi(b.tokens[index])
					if index < len(a.indexes) && index < len(b.indexes) && a.indexes[index] && b.indexes[index] && le == nil && re == nil {
						return left < right
					}
					return a.tokens[index] < b.tokens[index]
				}
			}
			return len(a.tokens) < len(b.tokens)
		}
		if a.priority != b.priority {
			return a.priority > b.priority
		}
		if a.code != b.code {
			return a.code < b.code
		}
		return a.message < b.message
	})
	out := []mcpInputProblem{}
	seen, discriminator := map[string]bool{}, map[string]bool{}
	for _, problem := range problems {
		paramKey := fmt.Sprint(problem.location) + "\x00" + problem.param
		if discriminator[paramKey] {
			continue
		}
		key := paramKey + "\x00" + problem.code
		if !seen[key] {
			out = append(out, problem)
			seen[key] = true
		}
		if problem.priority >= 2 {
			discriminator[paramKey] = true
		}
	}
	return out
}

func mcpInputError(problems []mcpInputProblem) *CLIError {
	problems = mcpOrderedInputProblems(problems)
	if len(problems) == 0 {
		return nil
	}
	if len(problems) > 50 {
		problems = problems[:50]
	}
	details := make([]map[string]any, 0, len(problems))
	for _, problem := range problems {
		details = append(details, map[string]any{"code": problem.code, "param": problem.param, "message": problem.message})
	}
	message := "1 argument is invalid."
	if len(problems) > 1 {
		message = fmt.Sprintf("%d arguments are invalid.", len(problems))
	}
	return &CLIError{ExitCode: ExitUsage, Type: errorTypeValidation, Code: "INVALID_REQUEST", Param: problems[0].param, Message: message, Details: details}
}
