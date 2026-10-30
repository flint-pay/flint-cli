package cli

import (
	"context"
	"fmt"
	"maps"
	"math/big"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

// The API uses ISO 4217 minor units. Currencies not listed use two decimals.
func mcpMoneyDisplay(amount any, currency string) string {
	exponent := 2
	switch currency {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF", "UGX", "UYI", "VND", "VUV", "XAF", "XOF", "XPF":
		exponent = 0
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		exponent = 3
	}
	value, ok := new(big.Rat).SetString(fmt.Sprint(amount))
	if !ok || !value.IsInt() {
		return ""
	}
	number := new(big.Int).Set(value.Num())
	negative := number.Sign() < 0
	number.Abs(number)
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
	whole, fraction := new(big.Int), new(big.Int)
	whole.QuoRem(number, scale, fraction)
	display := whole.String()
	if exponent > 0 {
		digits := fraction.String()
		display += "." + strings.Repeat("0", exponent-len(digits)) + digits
	}
	if negative {
		display = "-" + display
	}
	return display + " " + currency
}

func mcpEffect(entryTitle string, confirm, irreversible, sensitive bool, body map[string]any, resources []string) map[string]any {
	amounts := []map[string]any{}
	var visit func(any, string)
	visit = func(value any, path string) {
		switch node := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(node))
			for key := range node {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				field := key
				if path != "" {
					field = path + "." + key
				}
				if money, ok := node[key].(map[string]any); ok && strings.HasSuffix(key, "_money") {
					currency, _ := money["currency"].(string)
					if display := mcpMoneyDisplay(money["amount"], currency); currency != "" && display != "" {
						amounts = append(amounts, map[string]any{"field": field, "amount": money["amount"], "currency": currency, "display": display})
					}
				}
				visit(node[key], field)
			}
		case []any:
			for i, item := range node {
				visit(item, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	visit(body, "")
	summary := strings.TrimSuffix(entryTitle, ".")
	facts := []string{}
	for _, amount := range amounts {
		facts = append(facts, amount["display"].(string))
	}
	facts = append(facts, resources...)
	if len(facts) > 0 {
		summary += ": " + strings.Join(facts, ", ")
	}
	return map[string]any{"summary": summary, "confirm_required": confirm, "irreversible": irreversible, "sensitive": sensitive, "amounts": amounts, "resources": resources}
}

func (a *App) mcpDryRunResult(cmd *Command, args map[string]any, value any) any {
	envelope, ok := value.(map[string]any)
	if !ok {
		return value
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		return value
	}
	index, err := a.mcpOperationIndex(mcpPolicy{})
	if err != nil {
		return value
	}
	entry, ok := index.Lookup(cmd.CanonicalName)
	if !ok {
		return value
	}
	data = maps.Clone(data)
	data["dry_run"], data["operation"] = true, entry.ID
	headers, _ := data["headers"].(map[string]any)
	if key, ok := headers["Idempotency-Key"].(string); ok && key != "" {
		data["idempotency_key"] = key
	}
	delete(data, "headers")
	delete(data, "persistent_side_effects")
	delete(data, "preview")
	body, _ := data["body"].(map[string]any)
	data["effect"] = mcpPreparedEffect(entry, preparedRequest{Path: fmt.Sprint(data["path"]), BodyValue: body})
	permission := map[string]any{"status": "unknown", "reason": "Credential permissions were not checked for this dry run."}
	if entry.ScopeRequirement != nil {
		permission["scope_requirement"] = entry.ScopeRequirement
	}
	if entry.ScopeRequirement == nil {
		permission = map[string]any{"status": "pass"}
	}
	if a.mcpAuth != nil {
		if auth, known := a.mcpAuth.snapshot(); known {
			permission = map[string]any{"status": "pass"}
			if entry.ScopeRequirement != nil {
				permission["scope_requirement"] = entry.ScopeRequirement
			}
			if missing := index.MissingScopes(entry, auth.Scopes, true); len(missing) > 0 {
				permission["status"] = "fail"
				permission["missing_scopes"] = missing
			}
		}
	}
	data["permission_check"] = permission
	out := maps.Clone(envelope)
	out["data"] = data
	return out
}

// The dry run and approval prompt describe the same prepared path and body.
func mcpPreparedEffect(entry opindex.Entry, req preparedRequest) map[string]any {
	resources := []string{}
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			resources = append(resources, id)
		}
	}
	path := req.Path
	concrete, _ := url.Parse(path)
	templateParts := strings.Split(entry.Path, "/")
	concreteParts := []string{}
	if concrete != nil {
		concreteParts = strings.Split(concrete.EscapedPath(), "/")
	}
	for i, part := range templateParts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "_id}") && i < len(concreteParts) {
			id, _ := url.PathUnescape(concreteParts[i])
			add(id)
		}
	}
	keys := make([]string, 0, len(req.BodyValue))
	for key := range req.BodyValue {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.HasSuffix(key, "_id") {
			if id, ok := req.BodyValue[key].(string); ok {
				add(id)
			}
		}
	}
	return mcpEffect(entry.Title, entry.ConfirmRequired(), entry.Irreversible, entry.Sensitive, req.BodyValue, resources)
}

func mcpValidateSelect(cmd *Command, controls map[string]any) *CLIError {
	raw, ok := controls["select"]
	if !ok {
		return nil
	}
	fields := mcpStrings(raw)
	for i, field := range fields {
		if e := validateOutputPaths(cmd, []outputPath{{field, "select"}}); e != nil {
			e.Code = "INVALID_REQUEST"
			e.Param = fmt.Sprintf("select[%d]", i)
			e.Details = map[string]any{"problems": []map[string]any{{"code": "INVALID_VALUE", "param": e.Param, "message": e.Message}}}
			return e
		}
	}
	return nil
}

func mcpOwnIDField(cmd *Command) string {
	if runtimeOutputSchema(cmd) {
		return ""
	}
	schema, e := schemaForCommand(cmd, false)
	if e != nil {
		return ""
	}
	defs, _ := schema["$defs"].(map[string]any)
	var identity func(map[string]any, string, int) string
	identity = func(node map[string]any, name string, depth int) string {
		if node == nil || depth > 64 {
			return ""
		}
		if ref, ok := node["$ref"].(string); ok {
			name = strings.TrimPrefix(ref, "#/$defs/")
			target, _ := defs[name].(map[string]any)
			return identity(target, name, depth+1)
		}
		properties, _ := node["properties"].(map[string]any)
		if data, ok := properties["data"].(map[string]any); ok {
			return identity(data, "", depth+1)
		}
		if items, ok := node["items"].(map[string]any); ok {
			return identity(items, "", depth+1)
		}
		if name != "" {
			field := mcpSchemaIDField(name)
			if _, ok, _ := outputSchemaPath(node, defs, []string{field}, 0); ok {
				return field
			}
		}
		// Response variants such as APIKeyWithSecret compose the canonical resource.
		for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
			branches, _ := node[keyword].([]any)
			for _, branch := range branches {
				child, _ := branch.(map[string]any)
				if field := identity(child, "", depth+1); field != "" {
					return field
				}
			}
		}
		return ""
	}
	return identity(schema, cmd.OutputSchema, 0)
}

func mcpSchemaIDField(name string) string {
	// Buyer views share the merchant resource's identifier. These two resource
	// names also differ from their published ID names.
	name = strings.TrimPrefix(name, "Buyer")
	switch name {
	case "ProductVariant":
		name = "Variant"
	case "FlintWalletStoreSetup":
		name = "StoreSetup"
	}
	chars := []rune(name)
	var out strings.Builder
	for i, ch := range chars {
		if unicode.IsUpper(ch) && i > 0 && (unicode.IsLower(chars[i-1]) || i+1 < len(chars) && unicode.IsLower(chars[i+1])) {
			out.WriteByte('_')
		}
		out.WriteRune(unicode.ToLower(ch))
	}
	return strings.ReplaceAll(out.String(), "o_auth", "oauth") + "_id"
}

// A committed write retains its identity and reports absent optional fields as warnings.
func mcpProjectResult(ctx context.Context, cmd *Command, value any, opts Options) (any, string, *CLIError) {
	if opts.DryRunClient() {
		opts.Select = nil
		if opts.Field == "" && opts.JQ == "" {
			return value, "", nil
		}
		projected, e := applyCommandOutputTransforms(ctx, value, nil, opts)
		return projected, "", e
	}
	write := cmd != nil && (cmd.Mutation || cmd.Destructive || cmd.CanonicalName == "signup" || cmd.CanonicalName == "trigger" || cmd.CanonicalName == "listen")
	if !write || len(opts.Select) == 0 || opts.Field != "" || opts.JQ != "" {
		projected, e := applyCommandOutputTransforms(ctx, value, cmd, opts)
		if write && e != nil && (e.Code == "UNKNOWN_OUTPUT_FIELD" || e.Code == "OUTPUT_FIELD_ROOT") {
			return value, "Warning: the mutation succeeded, but output selection failed: " + e.Message + " Returning the complete successful result.", nil
		}
		return projected, "", e
	}
	fields := append([]string(nil), opts.Select...)
	// Internal callers retain the completed-write safeguard even if they bypass dispatch.
	if e := validateOutputPaths(cmd, func() []outputPath {
		p := []outputPath{}
		for _, f := range fields {
			p = append(p, outputPath{f, "select"})
		}
		return p
	}()); e != nil {
		return value, "Warning: the mutation succeeded, but output selection failed: " + e.Message + " Returning the complete successful result.", nil
	}
	for _, field := range []string{mcpOwnIDField(cmd), "status"} {
		if field != "" && !containsString(fields, field) {
			fields = append(fields, field)
		}
	}
	warnings := []string{}
	warned := map[string]bool{}
	project := func(resource any) (any, bool) {
		object, ok := resource.(map[string]any)
		if !ok {
			return nil, false
		}
		out := map[string]any{}
		for _, field := range fields {
			item, present := lookupPath(object, field)
			if !present {
				if containsString(opts.Select, field) && !warned[field] {
					warned[field] = true
					warnings = append(warnings, "Warning: select path "+field+" is not in this result.")
				}
				continue
			}
			if err := setBodyPath(out, field, item); err != nil {
				return nil, false
			}
		}
		return out, true
	}
	envelope, ok := value.(map[string]any)
	if !ok {
		return value, "Warning: the mutation succeeded, but output selection failed. Returning the complete successful result.", nil
	}
	out := maps.Clone(envelope)
	root, hasData := envelope["data"]
	if !hasData {
		root = value
	}
	switch resources := root.(type) {
	case []any:
		list := make([]any, 0, len(resources))
		for _, item := range resources {
			selected, ok := project(item)
			if !ok {
				return value, "Warning: the mutation succeeded, but output selection failed. Returning the complete successful result.", nil
			}
			list = append(list, selected)
		}
		out["data"] = list
	default:
		selected, ok := project(root)
		if !ok {
			return value, "Warning: the mutation succeeded, but output selection failed. Returning the complete successful result.", nil
		}
		if !hasData {
			return selected, strings.Join(warnings, "\n"), nil
		}
		out["data"] = selected
	}
	return out, strings.Join(warnings, "\n"), nil
}
