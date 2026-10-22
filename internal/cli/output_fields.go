package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func prepareOptionalFields(value any, cmd *Command, opts Options) (any, *CLIError) {
	normalized, e := normalizeOutput(value)
	if e != nil {
		return nil, e
	}
	fields := opts.Select
	if opts.Field != "" {
		fields = []string{opts.Field}
	}
	for _, field := range fields {
		var resources []map[string]any
		if opts.Field != "" {
			if m, ok := normalized.(map[string]any); ok {
				resources = []map[string]any{m}
			}
		} else if envelope, ok := normalized.(map[string]any); ok {
			data, hasData := envelope["data"]
			switch v := data.(type) {
			case []any:
				for _, resource := range v {
					if m, ok := resource.(map[string]any); ok {
						resources = append(resources, m)
					}
				}
			case map[string]any:
				resources = []map[string]any{v}
				// Single-resource envelopes sometimes nest the resource under its type.
				if _, exists := lookupPath(v, field); !exists {
					for key, child := range v {
						if m, ok := child.(map[string]any); ok {
							_, exists := lookupPath(m, field)
							if exists || len(v) == 1 && cmd != nil && !cmd.Local && outputSchemaDeclaresPath(cmd, "data."+key+"."+field) {
								resources = []map[string]any{m}
								break
							}
						}
					}
				}
			default:
				if !hasData {
					resources = []map[string]any{envelope}
				}
			}
		}
		found := false
		names := map[string]bool{}
		for _, resource := range resources {
			if _, exists := lookupPath(resource, field); exists {
				found = true
			}
			for key := range resource {
				names[key] = true
			}
		}
		declared := cmd != nil && !cmd.Local && outputSchemaDeclaresPath(cmd, field)
		if !found && !declared {
			candidates := make([]string, 0, len(names))
			for name := range names {
				candidates = append(candidates, name)
			}
			sort.Strings(candidates)
			suggestions := []string{}
			if suggestion := closestName(field, candidates); suggestion != "" {
				suggestions = append(suggestions, suggestion)
			}
			param, code := "select", "INVALID_SELECT"
			if opts.Field != "" {
				param, code = "field", "FIELD_NOT_FOUND"
			}
			message := fmt.Sprintf("Field path does not exist: %s.", field)
			if len(suggestions) > 0 {
				message += " Did you mean " + suggestions[0] + "?"
			}
			err := usageError(code, message, param)
			err.Details = suggestionDetails(field, suggestions)
			return nil, err
		}
		for _, resource := range resources {
			if _, exists := lookupPath(resource, field); !exists {
				if err := setMissingOutputField(resource, field); err != nil {
					return nil, usageError("FIELD_NOT_FOUND", "Field path does not exist: "+field, "field")
				}
			}
		}
	}
	return normalized, nil
}

func setMissingOutputField(resource map[string]any, field string) error {
	parts := strings.Split(field, ".")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], ".")
		if value, exists := lookupPath(resource, parent); exists && value == nil {
			var container any = map[string]any{}
			if _, err := strconv.Atoi(parts[i]); err == nil {
				container = []any{}
			}
			if err := setBodyPath(resource, parent, container); err != nil {
				return err
			}
		}
	}
	return setBodyPath(resource, field, nil)
}
