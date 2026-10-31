package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"sort"
	"strings"
)

type arrayReadProjection struct{ Field, Item string }

func appendArrayEditArguments(doc *openAPIDocument, path, method string, operation openAPIOperation, args []Arg) []Arg {
	if method != "PATCH" {
		return args
	}
	_, schema := requestContentSchema(operation.RequestBody)
	resolved, err := argumentReader().Resolve(schema, "")
	if err != nil {
		panic(err)
	}
	props, _ := resolved.Schema["properties"].(map[string]any)
	if props["expected_version"] == nil {
		return args
	}
	_, readable := doc.Paths[path]["get"]
	// Object arrays always remain --input replacements, including skipped routes.
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw := props[name]
		property, _ := raw.(map[string]any)
		field, err := argumentReader().Resolve(property, "")
		if err != nil {
			panic(err)
		}
		if publicSchemaType(field.Schema) != "array" {
			continue
		}
		for i := range args {
			if args[i].Name == "input" {
				args[i].Description += " " + name + " replaces the whole list."
			}
		}
	}
	if !readable {
		return args
	}
	for _, arg := range append([]Arg(nil), args...) {
		if !arg.bodyGenerated || !arg.Repeat || arg.Type == "boolean" {
			continue
		}
		for _, action := range []string{"add", "remove"} {
			edit := arg
			edit.Name = action + "_" + arg.Name
			edit.Flag = "--" + action + "-" + strings.ReplaceAll(arg.BodyPath, "_", "-")
			edit.ArrayEdit = action
			edit.Required = false
			edit.RequiredFor = nil
			edit.Nullable = false
			edit.Variants = nil
			edit.Description = fmt.Sprintf("%s values in %s using GET and a version-guarded PATCH; retries once on VERSION_CONFLICT. Client dry runs require a replacement flag instead.", strings.Title(action), arg.BodyPath)
			args = append(args, edit)
		}
	}
	for i := range args {
		if args[i].bodyGenerated && args[i].Repeat && args[i].ArrayEdit == "" {
			args[i].Description += " Replaces the whole list."
		}
	}
	return args
}

func hasArrayEdits(cmd *Command, opts Options) bool {
	for _, arg := range cmd.Arguments {
		if arg.ArrayEdit != "" && len(opts.Raw[strings.TrimPrefix(arg.Flag, "--")]) > 0 {
			return true
		}
	}
	return false
}

func (a *App) prepareArrayEdits(ctx context.Context, cmd *Command, opts Options, resolved ResolvedConfig, key, baseURL string, body map[string]any, req *preparedRequest) *CLIError {
	if !hasArrayEdits(cmd, opts) {
		return nil
	}
	if opts.DryRunClient() || opts.Preview {
		return usageError("ARRAY_EDIT_REQUIRES_READ", "Array edits require a GET of the current resource. For a client dry run, use the whole-list replacement flag or --input.", "dry-run")
	}
	if _, explicitVersion := body["expected_version"]; explicitVersion {
		return usageError("CONFLICTING_FLAGS", "Array edits read expected_version automatically; omit --expected-version and expected_version from --input.", "expected_version")
	}
	type change struct{ add, remove []any }
	changes := map[string]*change{}
	for _, arg := range cmd.Arguments {
		if arg.ArrayEdit == "" {
			continue
		}
		values := argumentValues(arg, opts)
		if len(values) == 0 {
			continue
		}
		if _, replacement := body[arg.BodyPath]; replacement {
			return usageError("CONFLICTING_FLAGS", "Use either array edits or a whole-list replacement for "+arg.BodyPath+".", arg.Name)
		}
		if changes[arg.BodyPath] == nil {
			changes[arg.BodyPath] = &change{}
		}
		for _, value := range values {
			if arg.IDPrefix != "" {
				value = strings.TrimSpace(value)
				if e := validateIDPrefix(value, arg); e != nil {
					return e
				}
			}
			if arg.AcceptsHistoryRef && strings.HasPrefix(value, "@last") {
				id, e := a.resolveHistoryRef(value, arg.IDPrefix, historyScopeFromResolved(resolved))
				if e != nil {
					return e
				}
				value = id
			}
			converted, err := convertValue(value, arg.Type)
			if err != nil {
				return usageError("INVALID_ARGUMENT", fmt.Sprintf("Invalid value for %s: %v", arg.Flag, err), arg.Name)
			}
			if arg.ArrayEdit == "add" {
				changes[arg.BodyPath].add = append(changes[arg.BodyPath].add, converted)
			} else {
				changes[arg.BodyPath].remove = append(changes[arg.BodyPath].remove, converted)
			}
		}
	}
	for field, c := range changes {
		for _, value := range c.add {
			if arrayContains(c.remove, value) {
				return usageError("CONFLICTING_FLAGS", "Cannot add and remove the same value in "+field+".", field)
			}
		}
	}
	readCtx := context.WithValue(ctx, responseContractKey{}, (*Command)(nil))
	response, e := a.doRequestWithin(readCtx, opts.Timeout, baseURL, key, "GET", req.Path, nil, "", opts.Debug)
	if e != nil {
		return e
	}
	envelope, _ := response.Value.(map[string]any)
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		return usageError("ARRAY_EDIT_READ_INVALID", "GET did not return a resource object.", "")
	}
	version, present := data["version"]
	if !present || version == nil {
		return usageError("ARRAY_EDIT_READ_INVALID", "GET did not return the resource version.", "version")
	}
	changed := false
	for field, c := range changes {
		raw, present := data[field]
		if !present {
			if projection, ok := publicOperationOverlays[cmd.OperationID].ArrayReads[field]; ok {
				references, ok := data[projection.Field].([]any)
				if ok {
					projected := make([]any, 0, len(references))
					for _, reference := range references {
						object, _ := reference.(map[string]any)
						value, ok := object[projection.Item].(string)
						if !ok {
							return usageError("ARRAY_EDIT_READ_INVALID", "GET category references omit their handle; cannot safely edit category_handles.", field)
						}
						projected = append(projected, value)
					}
					raw, present = projected, true
				}
			}
		}
		if !present {
			return usageError("ARRAY_EDIT_READ_INVALID", "GET did not return "+field+"; cannot safely edit its list.", field)
		}
		current, ok := raw.([]any)
		if !ok && raw != nil {
			return usageError("ARRAY_EDIT_READ_INVALID", "GET field "+field+" is not an array.", field)
		}
		next := []any{}
		for _, value := range current {
			if !arrayContains(c.remove, value) {
				next = append(next, value)
			}
		}
		for _, value := range c.add {
			if !arrayContains(next, value) {
				next = append(next, value)
			}
		}
		if !reflect.DeepEqual(current, next) && !(len(current) == 0 && len(next) == 0) {
			changed = true
		}
		body[field] = next
	}
	otherChanges := false
	for field := range body {
		if changes[field] == nil {
			otherChanges = true
		}
	}
	body["expected_version"] = version
	req.ArrayEditNoop = !changed && !otherChanges
	req.ArrayEditValue = response.Value
	return nil
}

func arrayContains(values []any, value any) bool {
	encoded, _ := json.Marshal(value)
	return slices.ContainsFunc(values, func(other any) bool {
		raw, _ := json.Marshal(other)
		if string(raw) == string(encoded) {
			return true
		}
		left, leftOK := new(big.Rat).SetString(string(raw))
		right, rightOK := new(big.Rat).SetString(string(encoded))
		return leftOK && rightOK && left.Cmp(right) == 0
	})
}
