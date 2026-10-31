package cli

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Body flags are a view of the contract. Only nested shortcuts are overlays.
func publicBodyArguments(operation openAPIOperation, existing []Arg) []Arg {
	media, schema := requestContentSchema(operation.RequestBody)
	if media != "application/json" {
		return nil
	}
	resolved, err := argumentReader().Resolve(schema, "")
	if err != nil {
		panic(err)
	}
	props, _ := resolved.Schema["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	required := mcpSchemaStrings(resolved.Schema["required"])
	var args []Arg
	for _, name := range names {
		property, _ := props[name].(map[string]any)
		field, err := argumentReader().Resolve(property, "")
		if err != nil {
			panic(err)
		}
		node := field.Schema
		nullable := schemaNullable(node)
		repeat := publicSchemaType(node) == "array"
		if repeat {
			items, _ := node["items"].(map[string]any)
			item, err := argumentReader().Resolve(items, "")
			if err != nil {
				panic(err)
			}
			node = item.Schema
		}
		typ := publicSchemaType(node)
		if node["properties"] != nil || node["additionalProperties"] != nil {
			continue
		}
		if typ != "string" && typ != "integer" && typ != "number" && typ != "boolean" {
			continue
		}
		arg := flag(name, openAPIArgumentType(typ, node), name, "", "", slices.Contains(required, name))
		arg.Description, _ = field.Schema["description"].(string)
		arg.Nullable, arg.Repeat = nullable, repeat
		if node["format"] == "date" {
			arg.Type = "date"
		}
		arg.Enum = mcpSchemaStrings(node["enum"])
		if value, ok := node["const"].(string); ok {
			arg.Enum = []string{value}
		}
		idName := name
		if repeat && strings.HasSuffix(idName, "_ids") {
			idName = strings.TrimSuffix(idName, "s")
		}
		arg.IDPrefix = publicResourceIDPrefixes[idName]
		arg.AcceptsHistoryRef = arg.IDPrefix != "" || isPublicResourceID(idName)
		if globalFlags[strings.TrimPrefix(arg.Flag, "--")] {
			arg.Flag = "--body-" + strings.ReplaceAll(name, "_", "-")
		}
		for _, other := range existing {
			if other.Flag == arg.Flag {
				arg.Flag = "--body-" + strings.ReplaceAll(name, "_", "-")
			}
		}
		if arg.Type == "time" {
			arg.Description += " Accepts RFC3339, calendar dates in the reporting timezone, now, +30d or -2h; relative body times require a sign."
		}
		if arg.Nullable {
			arg.Description += " Use null to clear."
		}
		if resolved.Variants != nil {
			arg.Variants = resolved.Membership[name]
			for tag, fields := range resolved.Variants.Requires {
				if slices.Contains(fields, name) {
					arg.RequiredFor = append(arg.RequiredFor, tag)
				}
			}
			sort.Strings(arg.RequiredFor)
		}
		args = append(args, arg)
	}
	return args
}

func schemaNullable(schema map[string]any) bool {
	return schema["nullable"] == true || slices.Contains(mcpSchemaStrings(schema["type"]), "null")
}

func resolveBodyTime(value string, now time.Time, location *time.Location) (string, *CLIError) {
	if value != "now" && !calendarTime(value) && !strings.HasPrefix(value, "+") && !strings.HasPrefix(value, "-") {
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			if _, err := resolveTime(value, now, location); err == nil {
				return "", usageError("AMBIGUOUS_RELATIVE_TIME", "Write +"+value+" for a time from now or -"+value+" for a time ago.", "")
			}
		}
	}
	instant, err := resolveTime(value, now, location)
	if err != nil {
		return "", usageError("INVALID_TIME", fmt.Sprint(err), "")
	}
	return instant, nil
}
