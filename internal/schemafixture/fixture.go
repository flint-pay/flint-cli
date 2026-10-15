// Package schemafixture derives small JSON values that satisfy a JSON Schema.
// Tests use it to send valid request bodies for every generated command.
package schemafixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// Minimal returns a value containing only what the schema requires. It follows
// local #/$defs references and searches oneOf, anyOf, and if/then/else choices
// until the value validates, preferring const, enum, and example values.
func Minimal(schema map[string]any) (any, error) {
	return MinimalMatching(schema, nil)
}

// MinimalMatching is Minimal for values that transform, such as a CLI merging
// flag values into an input body, turns into a valid instance.
func MinimalMatching(schema map[string]any, transform func(any) any) (any, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("https://fixture.invalid/schema.json", document); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile("https://fixture.invalid/schema.json")
	if err != nil {
		return nil, err
	}
	g := &generator{root: document.(map[string]any)}
	lastErr := errors.New("no candidate generated")
	for range 2000 {
		g.next, g.failed, g.stack = 0, false, map[string]bool{}
		value := g.value(g.root, 0)
		if !g.failed {
			candidate := value
			if transform != nil {
				candidate = transform(value)
			}
			if lastErr = compiled.Validate(candidate); lastErr == nil {
				return value, nil
			}
		}
		if !g.advance() {
			break
		}
	}
	return nil, fmt.Errorf("no valid minimal value found: %w", lastErr)
}

type generator struct {
	root    map[string]any
	choices []int
	counts  []int
	next    int
	failed  bool
	stack   map[string]bool
}

// choose returns the current option for the next decision point. advance then
// walks the decision tree depth first, varying the latest decision first.
func (g *generator) choose(options int) int {
	i := g.next
	g.next++
	if i == len(g.choices) {
		g.choices = append(g.choices, 0)
		g.counts = append(g.counts, options)
	}
	g.counts[i] = options
	return min(g.choices[i], options-1)
}

func (g *generator) advance() bool {
	g.choices, g.counts = g.choices[:g.next], g.counts[:g.next]
	for i := len(g.choices) - 1; i >= 0; i-- {
		if g.choices[i]+1 < g.counts[i] {
			g.choices[i]++
			g.choices, g.counts = g.choices[:i+1], g.counts[:i+1]
			return true
		}
	}
	return false
}

// flatten merges $ref targets, allOf entries, and the chosen oneOf, anyOf, and
// conditional branches into one schema. Branch keywords narrow the parent's.
func (g *generator) flatten(schema map[string]any, depth int, refs *[]string) map[string]any {
	if depth > 24 {
		g.failed = true
		return map[string]any{}
	}
	out := map[string]any{}
	var branches []map[string]any
	if ref, ok := schema["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		if g.stack[name] && !slices.Contains(*refs, name) {
			// Recursive shapes are satisfiable through a non-recursive branch.
			g.failed = true
			return out
		}
		defs, _ := g.root["$defs"].(map[string]any)
		if target, ok := defs[name].(map[string]any); ok && !g.stack[name] {
			g.stack[name] = true
			*refs = append(*refs, name)
			branches = append(branches, target)
		}
	}
	for key, value := range schema {
		switch key {
		case "$ref", "allOf", "oneOf", "anyOf", "if", "then", "else", "not", "$defs":
		default:
			out[key] = value
		}
	}
	if all, ok := schema["allOf"].([]any); ok {
		for _, branch := range all {
			if branch, ok := branch.(map[string]any); ok {
				branches = append(branches, branch)
			}
		}
	}
	for _, keyword := range []string{"oneOf", "anyOf"} {
		if options, ok := schema[keyword].([]any); ok && len(options) > 0 {
			if branch, ok := options[g.choose(len(options))].(map[string]any); ok {
				branches = append(branches, branch)
			}
		}
	}
	if _, ok := schema["if"]; ok {
		switch g.choose(3) {
		case 1:
			if then, ok := schema["then"].(map[string]any); ok {
				branches = append(branches, then)
			}
		case 2:
			if otherwise, ok := schema["else"].(map[string]any); ok {
				branches = append(branches, otherwise)
			}
		}
	}
	for _, branch := range branches {
		flat := g.flatten(branch, depth+1, refs)
		for key, value := range flat {
			switch key {
			case "properties":
				properties, _ := out["properties"].(map[string]any)
				merged := map[string]any{}
				for name, property := range properties {
					merged[name] = property
				}
				for name, property := range value.(map[string]any) {
					if existing, ok := merged[name].(map[string]any); ok {
						if property, ok := property.(map[string]any); ok {
							merged[name] = map[string]any{"allOf": []any{existing, property}}
							continue
						}
					}
					merged[name] = property
				}
				out["properties"] = merged
			case "required":
				existing, _ := out["required"].([]any)
				out["required"] = append(append([]any(nil), existing...), value.([]any)...)
			default:
				out[key] = value
			}
		}
	}
	return out
}

func (g *generator) value(schema map[string]any, depth int) any {
	var refs []string
	schema = g.flatten(schema, depth, &refs)
	defer func() {
		for _, name := range refs {
			delete(g.stack, name)
		}
	}()
	if g.failed {
		return nil
	}
	if value, ok := schema["const"]; ok {
		return value
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		return values[g.choose(len(values))]
	}
	switch schemaType(schema) {
	case "object":
		out := map[string]any{}
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, raw := range required {
			name, _ := raw.(string)
			if _, done := out[name]; done {
				continue
			}
			property, _ := properties[name].(map[string]any)
			if property == nil {
				property = map[string]any{}
			}
			out[name] = g.value(property, depth+1)
		}
		if missing := intKeyword(schema, "minProperties") - len(out); missing > 0 && len(properties) > 0 {
			names := make([]string, 0, len(properties))
			for name := range properties {
				if _, done := out[name]; !done {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			start := g.choose(len(names))
			for i := 0; i < min(missing, len(names)); i++ {
				name := names[(start+i)%len(names)]
				property, _ := properties[name].(map[string]any)
				out[name] = g.value(property, depth+1)
			}
		}
		return out
	case "array":
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			items = map[string]any{}
		}
		out := []any{}
		for i := 0; i < intKeyword(schema, "minItems"); i++ {
			out = append(out, g.value(items, depth+1))
		}
		return out
	case "integer", "number":
		n := 1
		if minimum, ok := number(schema["minimum"]); ok && minimum > n {
			n = minimum
		}
		if minimum, ok := number(schema["exclusiveMinimum"]); ok && minimum >= n {
			n = minimum + 1
		}
		if maximum, ok := number(schema["maximum"]); ok && maximum < n {
			n = maximum
		}
		return json.Number(strconv.Itoa(n))
	case "boolean":
		return false
	case "null":
		return nil
	default:
		return stringValue(schema)
	}
}

func stringValue(schema map[string]any) string {
	pattern, _ := schema["pattern"].(string)
	matches := func(text string) bool {
		if pattern == "" {
			return true
		}
		ok, err := regexp.MatchString(pattern, text)
		return err == nil && ok
	}
	candidates := []string{}
	if example, ok := schema["example"].(string); ok {
		candidates = append(candidates, example)
	}
	if examples, ok := schema["examples"].([]any); ok && len(examples) > 0 {
		if example, ok := examples[0].(string); ok {
			candidates = append(candidates, example)
		}
	}
	if pattern != "" {
		if text, ok := patternExample(pattern); ok {
			candidates = append(candidates, text)
		}
	}
	candidates = append(candidates, map[any]string{
		"date-time": "2026-01-01T00:00:00Z", "date": "2026-01-01", "email": "dev@example.com",
		"uri": "https://example.com/test", "hostname": "example.com",
	}[schema["format"]], "test")
	text := "test"
	for _, candidate := range candidates {
		if candidate != "" && matches(candidate) {
			text = candidate
			break
		}
	}
	if minimum := intKeyword(schema, "minLength"); len([]rune(text)) < minimum {
		text += strings.Repeat("x", minimum-len([]rune(text)))
	}
	if maximum := intKeyword(schema, "maxLength"); maximum > 0 && len([]rune(text)) > maximum {
		text = string([]rune(text)[:maximum])
	}
	return text
}

// patternExample produces the shortest string for simple anchored patterns:
// literals, escapes, character classes, optional groups, and quantifiers.
func patternExample(pattern string) (string, bool) {
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "^"), "$")
	var out strings.Builder
	for i := 0; i < len(pattern); {
		var atom string
		switch c := pattern[i]; c {
		case '\\':
			if i+1 >= len(pattern) {
				return "", false
			}
			atom = map[byte]string{'d': "0", 'w': "a", 'S': "x", 's': " "}[pattern[i+1]]
			if atom == "" {
				atom = string(pattern[i+1])
			}
			i += 2
		case '[':
			end := strings.IndexByte(pattern[i+1:], ']')
			if end < 0 {
				return "", false
			}
			class := pattern[i+1 : i+1+end]
			i += end + 2
			switch {
			case strings.HasPrefix(class, "^"):
				atom = "x"
			case strings.HasPrefix(class, "\\"):
				atom = class[1:2]
			default:
				atom = class[:1]
			}
		case '(':
			end := strings.IndexByte(pattern[i:], ')')
			if end < 0 || end+i+1 >= len(pattern) || !strings.ContainsRune("?*", rune(pattern[end+i+1])) {
				return "", false
			}
			i += end + 2
			continue
		case '.':
			atom = "a"
			i++
		case '|', ')':
			return "", false
		default:
			atom = string(c)
			i++
		}
		count := 1
		if i < len(pattern) {
			switch pattern[i] {
			case '+':
				i++
			case '*', '?':
				count = 0
				i++
			case '{':
				end := strings.IndexByte(pattern[i:], '}')
				if end < 0 {
					return "", false
				}
				if _, err := fmt.Sscanf(pattern[i+1:i+end], "%d", &count); err != nil {
					return "", false
				}
				i += end + 1
			}
		}
		out.WriteString(strings.Repeat(atom, count))
	}
	return out.String(), true
}

func schemaType(schema map[string]any) string {
	switch t := schema["type"].(type) {
	case string:
		return t
	case []any:
		names := []string{}
		for _, name := range t {
			if name, ok := name.(string); ok && name != "null" {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			return names[0]
		}
		return "null"
	}
	if _, ok := schema["properties"]; ok {
		return "object"
	}
	if _, ok := schema["required"]; ok {
		return "object"
	}
	if _, ok := schema["items"]; ok {
		return "array"
	}
	return "string"
}

func intKeyword(schema map[string]any, key string) int {
	n, _ := number(schema[key])
	return n
}

func number(value any) (int, bool) {
	switch n := value.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}
