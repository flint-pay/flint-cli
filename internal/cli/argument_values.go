package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/flint-pay/flint-cli/internal/schemaview"
)

var argumentReader = sync.OnceValue(func() *schemaview.Reader {
	doc, err := loadOpenAPI()
	if err != nil {
		panic(err)
	}
	components := map[string]any{}
	for name, schema := range doc.Components.Schemas {
		components[name] = schema
	}
	return schemaview.New(map[string]any{"components": map[string]any{"schemas": components}})
})

const queryTimeGuidance = "Also accepts today, this_week, yesterday, a date, or a duration such as 7d; calendar values use the reporting timezone."

func queryTimeDescription(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return queryTimeGuidance
	}
	if !strings.HasSuffix(description, ".") {
		description += "."
	}
	return description + " " + queryTimeGuidance
}

// Enrich existing arguments only. A4 owns generating new body flags.
func applyArgumentValues(cmd *Command) {
	reader := argumentReader()
	operation, _ := openAPIOperationByID(cmd.OperationID)
	_, body := requestContentSchema(operation.RequestBody)
	parameters := operationParameterSchemas(cmd.OperationID)
	for i := range cmd.Arguments {
		arg := &cmd.Arguments[i]
		var schema map[string]any
		switch {
		case arg.Query != "":
			schema = parameters["query:"+arg.Query]
		case arg.Positional > 0:
			schema = parameters["path:"+arg.Name]
		case arg.BodyPath != "":
			schema, _ = reader.Lookup(body, arg.BodyPath)
		}
		if schema != nil {
			resolved, err := reader.Resolve(schema, "")
			if err != nil {
				panic(err)
			}
			schema = resolved.Schema
			if arg.Description == "" {
				arg.Description, _ = schema["description"].(string)
			}
			if publicSchemaType(schema) == "array" {
				item, _ := schema["items"].(map[string]any)
				resolved, err = reader.Resolve(item, "")
				if err != nil {
					panic(err)
				}
				schema = resolved.Schema
			}
			values := mcpSchemaStrings(schema["enum"])
			if value, ok := schema["const"].(string); ok {
				values = []string{value}
			}
			if len(values) > 0 {
				arg.Enum = append([]string(nil), values...)
			}
		}
		switch cmd.CanonicalName + "/" + arg.Name {
		case "config.set/key":
			arg.Enum = []string{"profile", "merchant"}
			arg.Description = "Setting to change"
		case "config.set/value":
			arg.Description = "Value to save for the setting"
		case "config.unset/key":
			arg.Enum = []string{"merchant"}
			arg.Description = "Saved profile setting to clear"
		case "context.switch/context_id":
			arg.Description = "Authorized context to activate; omit to choose interactively"
		case "auth.reauth/no_open":
			arg.Description = "Print the approval link without opening a browser"
		case "support.open/area":
			arg.Enum = append([]string(nil), supportProductAreas...)
		}
		if arg.Type == "time" && arg.Query != "" {
			arg.Description = queryTimeDescription(arg.Description)
		}
	}
}

func argumentValues(arg Arg, opts Options) []string {
	if arg.Positional > 0 {
		if len(opts.Positionals) >= arg.Positional {
			return []string{opts.Positionals[arg.Positional-1]}
		}
		return nil
	}
	values := opts.Raw[strings.TrimPrefix(arg.Flag, "--")]
	if !arg.Repeat {
		return values
	}
	out := []string{}
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			out = append(out, strings.TrimSpace(item))
		}
	}
	return out
}

func validateArgumentValues(cmd *Command, opts *Options) *CLIError {
	for _, arg := range cmd.Arguments {
		if len(arg.Enum) == 0 || cmd.CanonicalName == "listen" && arg.Name == "event_type" {
			continue
		}
		for _, value := range argumentValues(arg, *opts) {
			if slices.Contains(arg.Enum, value) || arg.Nullable && value == "null" {
				continue
			}
			suggestions := []string{}
			for _, candidate := range arg.Enum {
				if editDistance(value, candidate) <= max(2, len(value)/3) {
					suggestions = append(suggestions, candidate)
				}
			}
			sortStringsByDistance(suggestions, value)
			suggestions = suggestions[:min(3, len(suggestions))]
			label := arg.Flag
			if label == "" {
				label = arg.Name
			}
			message := fmt.Sprintf("%s must be one of: %s.", label, strings.Join(arg.Enum, ", "))
			if len(suggestions) > 0 {
				message += " Did you mean " + suggestions[0] + "?"
			}
			message += " If this value was added recently, run flint upgrade to update the embedded API contract."
			err := usageError("INVALID_FLAG_VALUE", message, arg.Name)
			err.Details = map[string]any{"input": value, "allowed": arg.Enum, "suggestions": suggestions}
			return err
		}
	}
	return nil
}

func argumentValueLabel(arg Arg) string {
	if len(arg.Enum) > 0 && len(arg.Enum) <= 6 {
		return strings.Join(arg.Enum, "|")
	}
	switch {
	case arg.IDPrefix != "":
		return arg.IDPrefix + "ID"
	case arg.Type == "integer":
		return "N"
	case arg.Type == "number":
		return "NUMBER"
	case arg.Type == "time":
		return "TIME"
	case arg.Type == "date":
		return "YYYY-MM-DD"
	case arg.Type == "boolean":
		return ""
	default:
		return "TEXT"
	}
}

func printArgumentHelp(w io.Writer, cmd *Command, arg Arg) {
	// The current CLI-owned listener accepts one filter; L1 owns multiple filters.
	if cmd.CanonicalName == "listen" && arg.Name == "event_type" {
		arg.Repeat = false
	}

	label := arg.Flag
	if label == "" {
		label = arg.Name
		if arg.Variadic {
			label += "..."
		}
	}
	value := argumentValueLabel(arg)
	if arg.Repeat {
		// Existing body shortcuts accept repeated flags until A4 adds comma input.
		if arg.Query != "" || arg.BodyPath != "" {
			value += "[," + value + "...]"
		}
	}
	if value != "" {
		label += " " + value
	}
	description := strings.TrimSpace(arg.Description)
	if len(arg.RequiredFor) > 0 {
		description += " (required for " + strings.Join(arg.RequiredFor, "|") + ")"
	}
	if arg.Required {
		description += " (required)"
	}
	fmt.Fprintf(w, "  %-38s %s\n", label, description)
	if len(arg.Enum) > 6 && len(arg.Enum) <= 12 {
		fmt.Fprintln(w, "    Values: "+strings.Join(arg.Enum, ", "))
	}
	if len(arg.Enum) > 12 {
		if arg.Name == "event_type" || arg.BodyPath == "enabled_events" {
			fmt.Fprintf(w, "    %d values: see flint help events.\n", len(arg.Enum))
		} else {
			fmt.Fprintf(w, "    %d values: flint schema command %s --output json\n", len(arg.Enum), cmd.CanonicalName)
		}
	}
	if arg.Repeat {
		if arg.Query != "" && arg.Explode != nil && !*arg.Explode {
			fmt.Fprintln(w, "    Separate values with commas.")
		} else {
			fmt.Fprintln(w, "    Repeat this flag for multiple values.")
		}
	}
	if arg.AcceptsHistoryRef {
		ref := "@last"
		if arg.IDPrefix != "" {
			ref += "." + strings.TrimSuffix(arg.IDPrefix, "_")
		}
		fmt.Fprintln(w, "    Accepts "+ref+" for the most recent resource this profile created or changed.")
	}
}
