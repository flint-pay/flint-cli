package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/flint-pay/flint-cli/internal/opindex"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

func mcpToolName(canonical string) string { return opindex.ToolName(canonical) }

// Kept for legacy schema helpers. Operation dispatch resolves the index's name table.
func mcpCanonicalToolName(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "_", ".")
}

// Discovery helpers are available in both modes; operations obey the tool scope.
func mcpAlwaysEnabled(name string) bool {
	return name == "flint_search" || name == "flint_describe" || name == "flint_help"
}

type mcpPolicy struct {
	allowLive        bool
	allowDestructive bool
	readOnly         bool
	allTools         bool
	toolPrefixes     []string
	toolsFlag        []string
	credential       opindex.Credential
	index            *opindex.Index
}

func mcpBoolOption(opts Options, name string) bool {
	values := opts.Raw[name]
	return len(values) > 0 && values[len(values)-1] == "true"
}

func mcpPolicyFromOptions(opts Options) mcpPolicy {
	policy := mcpPolicy{allowLive: mcpBoolOption(opts, "allow-live"), allowDestructive: mcpBoolOption(opts, "allow-destructive"), readOnly: mcpBoolOption(opts, "read-only")}
	for _, raw := range opts.Raw["tools"] {
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			policy.toolsFlag = append(policy.toolsFlag, entry)
			if entry == "all" {
				policy.allTools = true
			} else {
				policy.toolPrefixes = append(policy.toolPrefixes, strings.TrimRight(strings.Join(strings.Fields(entry), "."), "."))
			}
		}
	}
	return policy
}

// Classification is offline: opening a server never introspects a credential to build tools/list.
func (a *App) mcpPolicy(opts Options) mcpPolicy {
	policy := mcpPolicyFromOptions(opts)
	_, policy.credential = activeCredentialClass()
	if a.mcpAuth != nil {
		policy.index = a.mcpAuth.index
	}
	if policy.index == nil {
		policy.index, _ = a.operationIndex()
	}
	return policy
}

func mcpPrefixMatches(prefix, name string) bool {
	return name == prefix || strings.HasPrefix(name, prefix+".") || mcpToolName(name) == prefix || strings.HasPrefix(mcpToolName(name), prefix+"_")
}

// defaultToolSet is discovery mode: --tools selects typed mode.
func (p mcpPolicy) defaultToolSet() bool { return len(p.toolsFlag) == 0 }

func (p mcpPolicy) includes(name string) bool {
	if p.allTools || p.defaultToolSet() || mcpAlwaysEnabled(name) {
		return true
	}
	for _, prefix := range p.toolPrefixes {
		if mcpPrefixMatches(prefix, name) {
			return true
		}
	}
	return false
}

func (p mcpPolicy) exposureReason(entry opindex.Entry) string {
	if !entry.MCP {
		return "credential_type"
	}
	if entry.Type != "operation" {
		return ""
	}
	if !p.credential.Allows(entry) {
		return "credential_type"
	}
	if entry.Type == "operation" {
		if !p.includes(entry.ID) {
			return "outside_tool_scope"
		}
		if p.readOnly && entry.Kind == "write" {
			return "read_only_server"
		}
	}
	return ""
}

func (p mcpPolicy) toolSetDescription() string {
	if p.allTools || p.defaultToolSet() {
		return "every operation available to this credential"
	}
	return "the operations selected with --tools " + strings.Join(p.toolsFlag, ",")
}

func (a *App) validateMCPServeOptions(opts Options) *CLIError {
	policy := mcpPolicyFromOptions(opts)
	if len(opts.Raw["tools"]) > 0 && len(policy.toolsFlag) == 0 {
		return usageError("INVALID_FLAG_VALUE", "--tools must name all, an operation, or a resource group such as orders.", "tools")
	}
	index, err := a.operationIndex()
	if err != nil {
		return cliError(ExitUsage, errorTypeInternal, "SCHEMA_SNAPSHOT_INVALID", err.Error())
	}
	var names []string
	var suggestions []string
	seen := map[string]bool{}
	for _, entry := range index.Entries {
		if entry.Type == "operation" && entry.MCP {
			names = append(names, entry.ID)
			// --tools accepts every dotted resource prefix as well as full
			// operation IDs, so typo suggestions must include both.
			for candidate := entry.ID; candidate != ""; {
				if !seen[candidate] {
					seen[candidate] = true
					suggestions = append(suggestions, candidate)
				}
				i := strings.LastIndexByte(candidate, '.')
				if i < 0 {
					break
				}
				candidate = candidate[:i]
			}
		}
	}
	for _, prefix := range policy.toolPrefixes {
		matched := false
		for _, name := range names {
			if mcpPrefixMatches(prefix, name) {
				matched = true
				break
			}
		}
		if !matched {
			e := usageError("UNKNOWN_TOOL_PREFIX", "--tools "+prefix+" matches no MCP operation. Use all, an operation ID such as refunds.create, or a group such as orders.", "tools")
			e.Details = suggestionDetails(prefix, mcpNameSuggestions(prefix, suggestions))
			return e
		}
	}
	if opts.Live && !policy.allowLive {
		return usageError("LIVE_REQUIRES_ALLOW_LIVE", "flint mcp serve --live requires --allow-live, which lets tools act on live (production) data.", "live")
	}
	return nil
}

// mcpSession carries per-connection state that tool calls need: the client's
// declared capabilities and server-to-client requests over the shared stream.
const (
	mcpMessageBlank mcpMessageKind = iota
	mcpMessageInvalid
	mcpMessageRequest
	mcpMessageNotification
	mcpMessageClientResponse
)

func mcpInstructions(policy mcpPolicy) string {
	mode := "Find operations with flint_search, check one with flint_describe, then run it with flint_read (reads) or flint_write (changes). Operation IDs follow Flint CLI commands, such as refunds.create."
	marked := "flint_describe marks confirm"
	if !policy.defaultToolSet() {
		mode = "Each tool runs one Flint operation and is named after its CLI command: refunds_create runs refunds.create. flint_describe shows nested and result fields, and flint_search finds operations among this server's tools."
		marked = "a tool's description says needs confirm"
	}
	writes := "Give every write an idempotency_key and reuse it when you retry the same request after a timeout or network error; a new key can run the write twice. Operations that " + marked + " need confirm: true. In a sandbox, set it yourself after checking the effect with dry_run: true. In live mode the user must also approve each one; if they can't be asked, the call returns the command for them to run."
	if policy.allowDestructive {
		writes = "Give every write an idempotency_key and reuse it when you retry the same request after a timeout or network error; a new key can run the write twice. Operations that " + marked + " need confirm: true. Set it yourself after checking the effect with dry_run: true, in sandbox and live mode."
	}
	if policy.readOnly {
		writes = "This server is read-only: it has no write tool."
	}
	live := "Live (production) credentials are refused; only the user can allow them, by restarting the server with --allow-live."
	if policy.allowLive {
		live = "Live (production) access is on: pass live: true on each call that uses a live credential."
	}
	return strings.Join([]string{"Flint Pay server for the Flint API, using the credential the user started it with.", mode, "Money amounts are integer minor units: 2500 is 25.00 USD. Lists return one page, 10 resources unless you set page_size, and a next_page_token to pass as page_token. Use select to return only the fields you need.", writes, live, "A failed call returns {error, cli}. error is the Flint API error object. Follow error.remediation.retry and cli.next_actions; when a next action is for the user, stop and tell the user."}, " ")
}

// mcpHiddenLocalTools are local commands that change the human's own setup.
// Agents must not rewrite the default profile or merchant guard (config set/unset)
// or remove the server's credential (auth logout); the human runs these.
var mcpHiddenLocalTools = map[string]bool{"config.set": true, "config.unset": true, "auth.logout": true}

func mcpCommandExposed(cmd *Command) bool {
	return cmd != nil && cmd.CanonicalName == cmd.Name && cmd.CanonicalName != "mcp.serve" && (!cmd.MCPHidden || cmd.CanonicalName == "trigger") && !mcpHiddenLocalTools[cmd.CanonicalName]
}

// mcpAllTools lists every tool the server can expose. History is split so
// listing stays read-only while clearing is a separate destructive tool, and
// doctor loses --fix, which rewrites the global default profile unasked.
func (a *App) mcpAllTools() []*Command {
	a.Registry.ensureAPICommands()
	tools := []*Command{}
	for _, cmd := range a.Registry.Commands {
		if !mcpCommandExposed(cmd) {
			continue
		}
		switch cmd.CanonicalName {
		case "history":
			list, clear := mcpHistoryTools(cmd)
			tools = append(tools, list, clear)
		case "doctor":
			doctor := *cmd
			doctor.Arguments = nil
			for _, arg := range cmd.Arguments {
				if arg.Name != "fix" {
					doctor.Arguments = append(doctor.Arguments, arg)
				}
			}
			tools = append(tools, &doctor)
		default:
			tools = append(tools, cmd)
		}
	}
	return tools
}

func mcpHistoryTools(history *Command) (*Command, *Command) {
	list := *history
	list.Arguments = nil
	for _, arg := range history.Arguments {
		if arg.Name != "clear" {
			list.Arguments = append(list.Arguments, arg)
		}
	}
	list.Description = "List recent Flint resource IDs recorded by this CLI"
	clear := list
	clear.Name, clear.CanonicalName = "history.clear", "history.clear"
	clear.Description = "Clear all recent Flint resource IDs recorded by this CLI"
	clear.Destructive = true
	clear.Supports = Supports{JSONOutput: history.Supports.JSONOutput, NoInput: history.Supports.NoInput}
	return &list, &clear
}

func (a *App) mcpTools(policy mcpPolicy) []*Command {
	index, err := a.mcpOperationIndex(policy)
	if err != nil {
		return nil
	}
	tools := []*Command{}
	for _, tool := range a.mcpAllTools() {
		if entry, ok := index.Lookup(tool.CanonicalName); ok && policy.exposureReason(entry) == "" {
			tools = append(tools, tool)
		}
	}
	return tools
}

// Typed tool names are exact. Dotted and space forms are operation argument values only.
func (a *App) mcpTool(name string, policy mcpPolicy) (*Command, bool) {
	for _, tool := range a.mcpTools(policy) {
		if mcpToolName(tool.CanonicalName) == name {
			return tool, true
		}
	}
	return nil, false
}

func mcpToolNames(canonical []string) []string {
	names := make([]string, 0, len(canonical))
	for _, name := range canonical {
		names = append(names, mcpToolName(name))
	}
	return names
}

func (a *App) mcpUnknownToolError(id any, name string, policy mcpPolicy) map[string]any {
	canonical := mcpCanonicalToolName(name)
	for _, tool := range a.mcpAllTools() {
		if tool.CanonicalName == canonical {
			group, _, _ := strings.Cut(canonical, ".")
			enable := "with " + group + " added to --tools"
			if policy.defaultToolSet() {
				enable = "with --tools " + group
			}
			return rpcError(id, -32602, "Unknown tool: "+mcpToolName(canonical)+" is not enabled on this server, which exposes "+policy.toolSetDescription()+". Do not retry; the user can enable it by restarting the server "+enable+" (or --tools all).")
		}
	}
	var names []string
	for _, tool := range a.mcpTools(policy) {
		names = append(names, tool.CanonicalName)
	}
	var suggestions []string
	if alias, ok := a.Registry.ByName(canonical); ok && alias.CanonicalName != canonical && policy.includes(alias.CanonicalName) {
		suggestions = append(suggestions, alias.CanonicalName)
	}
	for _, suggestion := range mcpNameSuggestions(canonical, names) {
		if len(suggestions) < 3 && !containsString(suggestions, suggestion) {
			suggestions = append(suggestions, suggestion)
		}
	}
	suggestions = mcpToolNames(suggestions)
	message := "Unknown tool: " + name + "."
	if len(suggestions) > 0 {
		message += " Did you mean " + strings.Join(suggestions, ", ") + "?"
	}
	response := rpcError(id, -32602, message)
	response["error"].(map[string]any)["data"] = map[string]any{"suggestions": suggestions}
	return response
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func mcpToolGroups(tools []*Command) []string {
	seen := map[string]bool{}
	var groups []string
	for _, tool := range tools {
		group, _, _ := strings.Cut(tool.CanonicalName, ".")
		if !seen[group] {
			seen[group] = true
			groups = append(groups, group)
		}
	}
	return groups
}

// mcpSingular folds each dotted segment to a rough singular, so customer and
// customers.list match customers and customers.lists.
func mcpSingular(name string) string {
	segments := strings.Split(name, ".")
	for i, segment := range segments {
		switch {
		case strings.HasSuffix(segment, "ies"):
			segment = strings.TrimSuffix(segment, "ies") + "y"
		case strings.HasSuffix(segment, "sses"), strings.HasSuffix(segment, "xes"):
			segment = segment[:len(segment)-2]
		case strings.HasSuffix(segment, "s") && !strings.HasSuffix(segment, "ss"):
			segment = strings.TrimSuffix(segment, "s")
		}
		segments[i] = segment
	}
	return strings.Join(segments, ".")
}

// mcpNameSuggestions ranks canonical names: singular and plural forms of the
// input first, then names that extend it, then the closest by edit distance.
func mcpNameSuggestions(input string, names []string) []string {
	input = strings.ToLower(mcpCanonicalToolName(input))
	type candidate struct {
		name         string
		rank, length int
	}
	var candidates []candidate
	limit := max(2, len(input)/3)
	singular := mcpSingular(input)
	for _, name := range names {
		distance := levenshteinDistance(input, name)
		rank := 2
		switch {
		case mcpSingular(name) == singular:
			rank = 0
		case strings.HasPrefix(name, input) || strings.HasPrefix(input, name+".") || strings.HasPrefix(mcpSingular(name), singular):
			rank = 1
		case distance > limit:
			continue
		}
		candidates = append(candidates, candidate{name, rank, distance})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank < candidates[j].rank
		}
		if candidates[i].length != candidates[j].length {
			return candidates[i].length < candidates[j].length
		}
		return candidates[i].name < candidates[j].name
	})
	var out []string
	for _, c := range candidates {
		if len(out) == 3 {
			break
		}
		out = append(out, c.name)
	}
	return out
}

func levenshteinDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(b)]
}

// mcpGateViolation rejects controls the human withheld from agents.
var mcpSelectionSources = map[string]string{"flag": "a command-line flag", "environment": "the environment", "project": "the project's .flint/config.json", "profile": "the profile's saved settings"}

// mcpSelectionViolation keeps an agent's _flint.profile and _flint.merchant
// within the human's choices. An agent may add a merchant guard, or pick a
// profile when the server pins none, but may not replace the profile,
// merchant guard, or context the server resolves (from flags, environment,
// project, or profile configuration). Repeating the server's value is fine.
func mcpQueryArguments(cmd *Command, arguments map[string]any) map[string]any {
	out := copyMap(arguments)
	for _, arg := range cmd.Arguments {
		if arg.Query != "" && arg.Repeat {
			if value, ok := out[arg.Name].(string); ok {
				out[arg.Name] = []any{value}
			}
		}
	}
	return out
}

func validateMCPArguments(cmd *Command, arguments map[string]any) error {
	return validateMCPToolArguments(cmd, arguments, mcpPolicy{allowLive: true, allowDestructive: true})
}

func validateMCPToolArguments(cmd *Command, arguments map[string]any, policy mcpPolicy) error {
	schema, schemaErr := schemaForMCPTool(cmd, policy)
	if schemaErr != nil {
		return fmt.Errorf("generate input schema: %s", schemaErr.Message)
	}
	rawSchema, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("encode input schema: %w", err)
	}
	var normalizedSchema any
	if err := json.Unmarshal(rawSchema, &normalizedSchema); err != nil {
		return fmt.Errorf("normalize input schema: %w", err)
	}
	const resourceURL = "https://cli.withflintpay.com/mcp/tool-input.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resourceURL, normalizedSchema); err != nil {
		return fmt.Errorf("load input schema: %w", err)
	}
	compiled, err := compiler.Compile(resourceURL)
	if err != nil {
		return fmt.Errorf("compile input schema: %w", err)
	}
	if err := compiled.Validate(mcpQueryArguments(cmd, arguments)); err != nil {
		return err
	}
	// Advertised schemas are deliberately shallow. Execution also validates
	// against the original request schema, including nested and union rules.
	entry := opindex.Entry{ID: cmd.CanonicalName, Kind: "read", Idempotency: commandIdempotencyClass(cmd), Paginated: cmd.Supports.Pagination, Get: cmd.Get}
	if cmd.Mutation || cmd.Destructive || cmd.CanonicalName == "listen" || cmd.CanonicalName == "signup" || cmd.CanonicalName == "trigger" {
		entry.Kind = "write"
	}
	args, controls := maps.Clone(arguments), map[string]any{}
	names := mcpReadControls
	if entry.Kind == "write" {
		names = mcpWriteControls
	}
	for _, name := range names {
		if value, ok := args[name]; ok {
			controls[name] = value
			delete(args, name)
		}
	}
	_, inputErr := mcpValidateInputs(cmd, entry, args, controls, policy)
	if inputErr != nil {
		return inputErr
	}
	return nil
}

// schemaForMCPTool advertises the public controls for each operation.
func schemaForMCPTool(cmd *Command, policy mcpPolicy) (map[string]any, *CLIError) {
	public := *cmd
	if public.CanonicalName == "listen" || public.CanonicalName == "auth.status" {
		public.Local = true
	}
	reader, original, e := commandDiscoverySchema(&public, true)
	if e != nil {
		return nil, e
	}
	schema, err := reader.Flatten(original, 2, 600)
	if err != nil {
		return nil, mcpDiscoverySnapshotError(err)
	}
	delete(schema, "description")
	properties, _ := schema["properties"].(map[string]any)
	if public.Local {
		delete(properties, "for")
		if public.Stream || public.CanonicalName == "trigger" {
			maximum := 120
			if public.CanonicalName == "trigger" {
				maximum = 60
			}
			properties["max_seconds"] = map[string]any{"type": "integer", "minimum": 1, "maximum": maximum, "default": maximum}
		}
		if public.Stream {
			properties["max_events"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 100}
		}
	}
	kind, idempotency, paginated, get := "read", commandIdempotencyClass(cmd), cmd.Supports.Pagination, cmd.Get
	gated := cmd.Destructive || cmd.Sensitive
	if cmd.Mutation || cmd.Destructive || cmd.CanonicalName == "listen" || cmd.CanonicalName == "signup" || cmd.CanonicalName == "trigger" {
		kind = "write"
	}
	if policy.index != nil {
		if entry, ok := policy.index.Lookup(cmd.CanonicalName); ok {
			kind, idempotency, paginated, get = entry.Kind, entry.Idempotency, entry.Paginated, entry.Get
			gated = entry.ConfirmRequired()
		}
	}
	// Typed schemas use the same public controls as discovery. The command's
	// full validator remains authoritative for unions and nested arguments.
	controlPolicy := policy
	controlPolicy.toolsFlag = nil
	controlPolicy.readOnly = false
	toolName := "flint_" + kind
	for _, tool := range mcpDiscoveryTools(controlPolicy, latestMCPProtocolVersion) {
		if tool["name"] != toolName {
			continue
		}
		controls := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for name, control := range controls {
			if name == "operation" || name == "arguments" || name == "all" && !paginated || (name == "wait_for" || name == "wait_seconds") && !get || name == "confirm" && !gated {
				continue
			}
			compact := maps.Clone(control.(map[string]any))
			delete(compact, "description")
			properties[name] = compact
		}
	}
	if paginated {
		if pageSize, ok := properties["page_size"].(map[string]any); ok {
			pageSize["default"] = 10
			pageSize["description"] = "Results per page, 1 to 100; default 10."
		}
	}
	if kind == "write" && idempotency == "required" {
		required := mcpStrings(schema["required"])
		schema["required"] = append(required, "idempotency_key")
	}
	return schema, nil
}

// mcpCompactSchema drops annotations that cost tokens without informing an
// agent: enum descriptions that only restate their value ("Goods." for goods)
// and Flint's enum bookkeeping keywords.
func mcpCompactSchema(schema map[string]any) {
	restates := func(value any, description any) bool {
		normalize := func(text string) string {
			return strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, strings.ToLower(text))
		}
		text, _ := description.(string)
		return normalize(text) == normalize(fmt.Sprint(value))
	}
	walkJSONSchemas(schema, func(node map[string]any) {
		delete(node, "x-flint-enum-kind")
		delete(node, "x-flint-enum-vocabulary")
		descriptions, ok := node["x-enum-descriptions"].([]any)
		if !ok {
			return
		}
		values, _ := node["enum"].([]any)
		informative := false
		kept := make([]any, len(descriptions))
		for i, description := range descriptions {
			if i < len(values) && restates(values[i], description) {
				kept[i] = ""
				continue
			}
			kept[i] = description
			informative = true
		}
		if informative {
			node["x-enum-descriptions"] = kept
		} else {
			delete(node, "x-enum-descriptions")
		}
	})
}

func schemaForMCPCommand(cmd *Command) (map[string]any, *CLIError) {
	schema, schemaErr := schemaForCommand(cmd, true)
	if schemaErr != nil {
		return schema, schemaErr
	}
	// MCP requires an object root even when the public schema is a union.
	schema["type"] = "object"
	if cmd == nil || !cmd.Stream {
		return schema, nil
	}
	properties, _ := schema["properties"].(map[string]any)
	controlsProperty, _ := properties["_flint"].(map[string]any)
	controls, _ := controlsProperty["properties"].(map[string]any)
	controls["max_events"] = map[string]any{
		"type":        "integer",
		"minimum":     1,
		"maximum":     mcpMaxStreamEvents,
		"description": fmt.Sprintf("Stop after at most %d stream records.", mcpMaxStreamEvents),
	}
	controls["for"] = map[string]any{
		"type":        "string",
		"description": "Positive Go duration no longer than 2m.",
	}
	controlsProperty["description"] = "Set max_events (1-100) or for (up to 2m)."
	bound := map[string]any{
		"required": []string{"_flint"},
		"properties": map[string]any{
			"_flint": map[string]any{
				"anyOf": []any{
					map[string]any{"required": []string{"max_events"}},
					map[string]any{"required": []string{"for"}},
				},
			},
		},
	}
	constraint := bound
	if cmd.CanonicalName == "listen" {
		controlsProperty["description"] = "Set max_events (1-100) or for (up to 2m); or print_secret."
		constraint = map[string]any{"anyOf": []any{bound, map[string]any{
			"required":   []string{"print_secret"},
			"properties": map[string]any{"print_secret": map[string]any{"const": true}},
		}}}
	}
	schema["allOf"] = appendSchemaConstraint(schema["allOf"], constraint)
	return schema, nil
}

func appendSchemaConstraint(existing any, constraint map[string]any) []any {
	switch values := existing.(type) {
	case []any:
		return append(values, constraint)
	case nil:
		return []any{constraint}
	default:
		return []any{values, constraint}
	}
}

func validateMCPStreamBounds(cmd *Command, arguments map[string]any) error {
	if cmd == nil || !cmd.Stream {
		return nil
	}
	controls, _ := arguments["_flint"].(map[string]any)
	if raw, ok := controls["for"]; ok {
		duration, err := time.ParseDuration(fmt.Sprint(raw))
		if err != nil || duration <= 0 {
			return fmt.Errorf("_flint.for must be a positive Go duration")
		}
		if duration > mcpMaxStreamDuration {
			return fmt.Errorf("_flint.for must not exceed %s", mcpMaxStreamDuration)
		}
	}
	return nil
}

func mcpToolAnnotations(cmd *Command) map[string]any {
	readOnly := !cmd.Mutation && !cmd.Destructive && !cmd.Sensitive
	// Sensitive operations need confirmation in live mode (refunds, secret
	// rotation, session revocation), so clients should treat them as destructive.
	destructive := cmd.Destructive || cmd.Sensitive
	// Replacing (PUT) or deleting a resource again leaves it in the same state.
	idempotent := readOnly || cmd.Method == "PUT" || cmd.Method == "DELETE"
	openWorld := !cmd.Local
	switch cmd.CanonicalName {
	case "api":
		// The raw tool's method is selected at call time and can be DELETE.
		readOnly = false
		destructive = true
		idempotent = false
	case "history":
		readOnly, destructive, idempotent = true, false, true
	case "history.clear":
		readOnly, destructive, idempotent = false, true, true
	case "listen":
		// Forwards each event with a POST to the local endpoint.
		readOnly, destructive, idempotent, openWorld = false, false, false, true
	case "signup":
		readOnly, idempotent, openWorld = false, false, true
	case "doctor", "config.validate", "init", "help.search":
		// MCP's doctor has no --fix, so these only read.
		openWorld = true
	}
	return map[string]any{
		"readOnlyHint":    readOnly,
		"destructiveHint": destructive,
		"idempotentHint":  idempotent,
		"openWorldHint":   openWorld,
	}
}

// mcpLocalTools describes local tools in MCP terms: their CLI descriptions
// mention flags and terminal behavior that do not apply to agents.
var mcpLocalTools = map[string]struct{ title, description string }{
	"help":            {"Flint MCP help", "Explain one tool (arguments, required fields, controls), list a group's tools, or show a topic: controls, events, test-cards, exit-codes. Omit topic for an index of enabled tools."},
	"help.search":     {"Search Flint Help", "Search Flint Help community answers and support threads with a question."},
	"schema.input":    {"Tool input schema", "JSON Schema of a tool's arguments, the same schema tools/call validates against, including tools this server does not enable."},
	"schema.output":   {"Tool result schema", "JSON Schema of a tool's successful result (structuredContent). Some are large; use _flint.jq to extract parts."},
	"schema.command":  {"Describe a command", "Describe the Flint CLI command behind a tool: API method and path, operation ID, and safety classification."},
	"schema.commands": {"List commands", "List Flint CLI commands, optionally only those in a group such as customers, with their API routes."},
	"schema.errors":   {"Error catalog", "Flint exit codes and error types."},
	"schema.events":   {"Webhook event catalog", "List webhook event types with one-line descriptions. Pass event_type (an exact type, or a prefix such as payment_intent) for their payload schemas."},
	"trigger":         {"Run a sandbox recipe", "Create sandbox fixtures and run the selected payment or webhook recipe, waiting at most 60 seconds."},
	"history":         {"Recent resource IDs", "List Flint resource IDs this CLI recently created or changed with the active credential."},
	"history.clear":   {"Clear resource history", "Delete this CLI's saved list of recent resource IDs. Needs confirm: true."},
	"version":         {"CLI version", "Flint CLI version and build, and the public API version and schema hash it targets."},
	"doctor":          {"Check the Flint setup", "Check this server's Flint credential, connectivity, scopes, and API version compatibility."},
	"init":            {"Initialize project guards", "Write or validate .flint/config.json guards for the active merchant and sandbox. force replaces an existing project config. Without a credential, report sign-in steps."},
	"config.get":      {"Resolved configuration", "Show the Flint CLI configuration this server resolves: profile, merchant guard, context, and their sources."},
	"config.validate": {"Validate configuration", "Validate the global and project Flint configuration and the credential they select."},
	"signup":          {"Sign up for Flint", "Create a Flint account and sandbox API key in two calls: first email, first_name, and last_name (Flint emails a code), then verification_code. Only for a profile without a saved credential."},
	"api":             {"Call a Flint API route", "Call any documented Flint public API route by method and /v1 path, with a JSON body for writes. Prefer a dedicated tool when one exists."},
	"listen":          {"Stream webhook events", "Print Flint webhook events or forward them locally with the HMAC listen secret, regardless of endpoint signing methods. JWT handlers need an HMAC path or a local test bypass. Set max_events or max_seconds (up to 100 events or 120 seconds). print_secret returns the local secret without connecting."},
}

// mcpToolTitle prefers the description's first sentence when it is short and
// free of CLI flags; otherwise it spells out the canonical name.
func mcpToolTitle(cmd *Command) string {
	if local, ok := mcpLocalTools[cmd.CanonicalName]; ok {
		return local.title
	}
	sentence := strings.TrimSpace(cmd.Description)
	if index := strings.IndexAny(sentence, ";\n"); index >= 0 {
		sentence = sentence[:index]
	}
	if index := strings.Index(sentence, ". "); index >= 0 {
		sentence = sentence[:index]
	}
	sentence = strings.TrimSuffix(strings.TrimSpace(sentence), ".")
	if sentence != "" && len(sentence) <= 64 && !strings.Contains(sentence, "--") {
		return sentence
	}
	var words []string
	for _, part := range strings.FieldsFunc(cmd.CanonicalName, func(r rune) bool { return r == '.' || r == '-' }) {
		words = append(words, strings.ToUpper(part[:1])+part[1:])
	}
	return strings.Join(words, " ")
}

// mcpToolDescription is the compact tools/list description: what the tool
// does, then the facts agents otherwise guess at: requirements stated only in
// schema combinators, pagination, and money units.
func mcpToolDescription(cmd *Command, schema map[string]any) string {
	parts := []string{mcpBaseDescription(cmd)}
	// Flattening removes conditional requirements from the advertised schema.
	// Keep those requirements visible in the description using the full body.
	full, e := requestBodySchema(cmd)
	if e == nil {
		schema = full
	}
	if summary := mcpRequirementSummary(schema); summary != "" {
		parts = append(parts, summary)
	}
	if cmd.Supports.Pagination && !cmd.Stream {
		parts = append(parts, "Returns one page: pass next_page_token as page_token for the next (empty on the last page; there is no has_more), or set all: true.")
	}
	if mcpUsesMoney(cmd, schema) {
		parts = append(parts, "Money amounts are integer minor units (2500 is 25.00 USD).")
	}
	return strings.Join(parts, " ")
}

// mcpBaseDescription uses the public API's operation description, trimmed to
// its first sentences, when it says more than the CLI's one-line summary.
func mcpBaseDescription(cmd *Command) string {
	switch cmd.OperationID {
	case "createWebhookEndpoint":
		return "Create a webhook endpoint. signing_methods defaults to [jwt]; verify Authorization: Bearer <token> with Flint's JWKS. A shared secret is returned only when hmac is included; save it if present."
	case "updateWebhookEndpoint":
		return "Update a webhook endpoint. signing_methods replaces the set; omit to leave unchanged. Adding hmac without an issued secret returns 409 ACTION_NOT_ALLOWED with reason secret_not_issued; use rotate-secret to issue one and enable hmac. Removing hmac deletes shared secrets."
	case "rotateWebhookEndpointSecret":
		return "Rotate a webhook endpoint secret, or issue the first one and enable hmac. Save the returned secret."
	case "createDeliveryRateCallback":
		return "Create a delivery rate callback. signing_methods defaults to [jwt]; verify Authorization: Bearer <token> with Flint's JWKS and aud set to the delivery_rate_callback_id. A shared secret and key_id are returned only when hmac is included; save them if present."
	case "updateDeliveryRateCallback":
		return "Update a delivery rate callback. signing_methods replaces the set; omit to keep it. Adding hmac without a secret returns 409 ACTION_NOT_ALLOWED with reason secret_not_issued; rotate-secret issues one and enables hmac. Removing hmac deletes shared secrets."
	case "rotateDeliveryRateCallbackSigningKey":
		return "Rotate a delivery rate callback secret, or issue the first one and enable hmac. Save the returned secret and key_id."
	}
	if local, ok := mcpLocalTools[cmd.CanonicalName]; ok {
		return local.description
	}
	description := strings.TrimSpace(cmd.Description)
	if before, after, found := strings.Cut(description, ";"); found && strings.Contains(after, "--") {
		description = strings.TrimSpace(before)
	}
	if operation, ok := openAPIOperationByID(cmd.OperationID); ok {
		if text := mcpLeadingSentences(operation.Description, 200); len(text) > len(description)+1 {
			description = text
		}
	}
	if description != "" && !strings.HasSuffix(description, ".") {
		description += "."
	}
	return description
}

// mcpLeadingSentences returns up to two leading sentences within limit bytes,
// shortening a first sentence that alone is too long.
func mcpLeadingSentences(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	var out string
	for sentences := 0; text != "" && sentences < 2; sentences++ {
		end := len(text)
		for i := 0; i+1 < len(text); i++ {
			if text[i] != '.' || text[i+1] != ' ' {
				continue
			}
			word := text[strings.LastIndexByte(text[:i], ' ')+1 : i]
			if word == "e.g" || word == "i.e" || word == "etc" || word == "vs" {
				continue
			}
			end = i + 1
			break
		}
		sentence := strings.TrimSpace(text[:end])
		if out != "" && len(out)+1+len(sentence) > limit {
			break
		}
		if out == "" && len(sentence) > limit {
			cut := strings.LastIndexByte(sentence[:limit], ' ')
			if cut <= 0 {
				for cut = limit; cut > 0 && !utf8.RuneStart(sentence[cut]); cut-- {
				}
			}
			return strings.TrimRight(sentence[:cut], " ,;:") + "..."
		}
		out = strings.TrimSpace(out + " " + sentence)
		text = strings.TrimSpace(text[end:])
	}
	return out
}

func mcpStrings(value any) []string {
	var out []string
	switch values := value.(type) {
	case []string:
		out = append(out, values...)
	case []any:
		for _, item := range values {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
	}
	return out
}

// mcpRequirementSummary spells out requirements a schema states only through
// oneOf, anyOf, allOf, if/then, not, or dependentRequired, which clients and
// models often ignore. It returns "" when there are none.
func mcpRequirementSummary(schema map[string]any) string {
	defs, _ := schema["$defs"].(map[string]any)
	resolve := func(node any) map[string]any {
		m, _ := node.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			m, _ = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		}
		return m
	}
	rootRequired := map[string]bool{}
	for _, name := range mcpStrings(schema["required"]) {
		rootRequired[name] = true
	}
	var sentences []string
	needs := map[string][]string{} // "expected_version" -> fields that need it
	var needOrder []string
	need := func(triggers, needed []string) {
		key := strings.Join(needed, " and ")
		if _, seen := needs[key]; !seen {
			needOrder = append(needOrder, key)
		}
		needs[key] = append(needs[key], triggers...)
	}
	// conditional recognizes {anyOf: [{not: {required: X}}, {required: Y}]}
	// and {if: {required: X}, then: {required: Y}}: sending X needs Y.
	conditional := func(node map[string]any) bool {
		if branches, ok := node["anyOf"].([]any); ok && len(branches) == 2 {
			first, second := resolve(branches[0]), resolve(branches[1])
			not, _ := first["not"].(map[string]any)
			triggers := mcpStrings(not["required"])
			var needed []string
			for _, name := range mcpStrings(second["required"]) {
				if !containsString(triggers, name) {
					needed = append(needed, name)
				}
			}
			if len(first) == 1 && len(not) == 1 && len(triggers) > 0 && len(needed) > 0 {
				need(triggers, needed)
				return true
			}
		}
		condition, _ := node["if"].(map[string]any)
		then, _ := node["then"].(map[string]any)
		if triggers, needed := mcpStrings(condition["required"]), mcpStrings(then["required"]); len(triggers) > 0 && len(needed) > 0 {
			need(triggers, needed)
			return true
		}
		return false
	}
	alternatives := func(branches []any, exclusive bool) string {
		type variant struct {
			required []string
			values   map[string][]string // property -> const or enum values
		}
		var variants []variant
		for _, raw := range branches {
			branch := resolve(raw)
			v := variant{values: map[string][]string{}}
			members := []map[string]any{branch}
			for _, member := range mcpAnySlice(branch["allOf"]) {
				members = append(members, resolve(member))
			}
			for _, member := range members {
				for _, name := range mcpStrings(member["required"]) {
					if !rootRequired[name] && !containsString(v.required, name) {
						v.required = append(v.required, name)
					}
				}
			}
			properties, _ := branch["properties"].(map[string]any)
			for name, rawProperty := range properties {
				property, _ := rawProperty.(map[string]any)
				if value, ok := property["const"]; ok {
					v.values[name] = []string{fmt.Sprint(value)}
				} else if enum, ok := property["enum"].([]any); ok && len(enum) > 0 {
					for _, value := range enum {
						v.values[name] = append(v.values[name], fmt.Sprint(value))
					}
				}
			}
			variants = append(variants, v)
		}
		if len(variants) < 2 {
			return ""
		}
		// A property every variant fixes to its own values selects the variant.
		var discriminators []string
		for name := range variants[0].values {
			shared := true
			for _, v := range variants[1:] {
				if _, ok := v.values[name]; !ok {
					shared = false
				}
			}
			// The values must tell the variants apart.
			seen := map[string]bool{}
			for _, v := range variants {
				for _, value := range v.values[name] {
					if seen[value] {
						shared = false
					}
					seen[value] = true
				}
			}
			if shared {
				discriminators = append(discriminators, name)
			}
		}
		sort.Strings(discriminators)
		if len(discriminators) > 0 {
			name := discriminators[0]
			var values, details []string
			for _, v := range variants {
				values = append(values, v.values[name]...)
				var rest []string
				for _, field := range v.required {
					if field != name {
						rest = append(rest, field)
					}
				}
				if len(rest) > 0 {
					details = append(details, name+" "+strings.Join(v.values[name], "/")+", send "+strings.Join(rest, ", "))
				}
			}
			sentence := "Set " + name + " to one of: " + strings.Join(values, ", ") + "."
			if len(details) > 0 && len(variants) <= 4 {
				sentence += " With " + strings.Join(details, "; with ") + "."
			} else if len(details) > 0 {
				sentence += " Each has its own required fields; see help."
			}
			return sentence
		}
		var groups []string
		empty := 0
		// Variants that one required set cannot tell apart (equal or nested
		// sets) differ in ways a one-line summary cannot state.
		for i, v := range variants {
			for j, other := range variants {
				if i == j || len(v.required) == 0 {
					continue
				}
				nested := true
				for _, field := range v.required {
					if !containsString(other.required, field) {
						nested = false
					}
				}
				if nested {
					return fmt.Sprintf("Arguments must match one of %d variants; see flint_describe.", len(variants))
				}
			}
		}
		for _, v := range variants {
			switch len(v.required) {
			case 0:
				empty++
			case 1:
				groups = append(groups, v.required[0])
			default:
				groups = append(groups, "{"+strings.Join(v.required, ", ")+"}")
			}
		}
		switch {
		case empty == 0 && exclusive:
			return "Provide exactly one of: " + strings.Join(groups, "; ") + "."
		case empty == 0:
			return "Provide at least one of: " + strings.Join(groups, "; ") + "."
		case empty == 1 && exclusive && len(groups) > 0:
			return "Provide at most one of: " + strings.Join(groups, "; ") + "."
		case exclusive:
			return fmt.Sprintf("Arguments must match one of %d variants; see flint_describe.", len(variants))
		}
		return ""
	}
	if branches, ok := schema["oneOf"].([]any); ok {
		if sentence := alternatives(branches, true); sentence != "" {
			sentences = append(sentences, sentence)
		}
	}
	if branches, ok := schema["anyOf"].([]any); ok && !conditional(schema) {
		if sentence := alternatives(branches, false); sentence != "" {
			sentences = append(sentences, sentence)
		}
	}
	conditional(map[string]any{"if": schema["if"], "then": schema["then"]})
	for _, raw := range mcpAnySlice(schema["allOf"]) {
		member := resolve(raw)
		if conditional(member) {
			continue
		}
		// Streaming tools require one of several _flint bounds.
		properties, _ := member["properties"].(map[string]any)
		for name, rawProperty := range properties {
			property, _ := rawProperty.(map[string]any)
			var options []string
			for _, branch := range mcpAnySlice(property["anyOf"]) {
				if required := mcpStrings(resolve(branch)["required"]); len(required) == 1 {
					options = append(options, name+"."+required[0])
				}
			}
			if len(options) > 1 && containsString(mcpStrings(member["required"]), name) {
				sentences = append(sentences, "Set "+strings.Join(options, " or ")+".")
			}
		}
	}
	if dependent, ok := schema["dependentRequired"].(map[string]any); ok {
		var triggers []string
		for trigger := range dependent {
			triggers = append(triggers, trigger)
		}
		sort.Strings(triggers)
		for _, trigger := range triggers {
			need([]string{trigger}, mcpStrings(dependent[trigger]))
		}
	}
	for _, key := range needOrder {
		triggers := needs[key]
		list := strings.Join(triggers, ", ")
		if len(triggers) > 1 {
			list = strings.Join(triggers[:len(triggers)-1], ", ") + ", or " + triggers[len(triggers)-1]
		}
		verb := " is "
		if strings.Contains(key, " and ") {
			verb = " are "
		}
		sentences = append(sentences, key+verb+"required when sending "+list+".")
	}
	// {not: {required: [S], anyOf: [...]}} means S must be sent alone.
	if not, ok := schema["not"].(map[string]any); ok {
		if alone := mcpStrings(not["required"]); len(alone) == 1 {
			if _, combined := not["anyOf"]; combined {
				sentences = append(sentences, "Send "+alone[0]+" without other fields.")
			} else {
				sentences = append(sentences, "Do not send "+alone[0]+".")
			}
		}
	}
	summary := strings.Join(sentences, " ")
	if len(summary) > 200 {
		cut := strings.LastIndex(summary[:190], ", ")
		if cut <= 0 {
			cut = 190
		}
		summary = summary[:cut] + "... (see help)"
	}
	return summary
}

func mcpAnySlice(value any) []any {
	values, _ := value.([]any)
	return values
}

var (
	mcpComponentGraphOnce sync.Once
	mcpComponentGraph     map[string]map[string]bool
)

// mcpComponentReferences maps each public schema component to the components
// it references directly.
func mcpComponentReferences() map[string]map[string]bool {
	mcpComponentGraphOnce.Do(func() {
		mcpComponentGraph = map[string]map[string]bool{}
		doc, err := loadOpenAPI()
		if err != nil {
			return
		}
		for name, schema := range doc.Components.Schemas {
			mcpComponentGraph[name] = map[string]bool{}
			collectComponentRefs(schema, mcpComponentGraph[name])
		}
	})
	return mcpComponentGraph
}

// mcpReachesMoney reports whether Money is within depth references of the
// named components. A shallow bound keeps the money note to tools whose own
// fields are amounts, not ones that merely link to a priced resource.
func mcpReachesMoney(names []string, depth int) bool {
	graph := mcpComponentReferences()
	seen := map[string]bool{}
	for level := 0; level <= depth && len(names) > 0; level++ {
		var next []string
		for _, name := range names {
			if name == "Money" {
				return true
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			for target := range graph[name] {
				next = append(next, target)
			}
		}
		names = next
	}
	return false
}

// mcpUsesMoney reports whether a tool's arguments or result carry Money
// amounts within two levels of nesting.
func mcpUsesMoney(cmd *Command, schema map[string]any) bool {
	root := map[string]any{}
	for key, value := range schema {
		if key != "$defs" {
			root[key] = value
		}
	}
	var direct []string
	walkJSONSchemas(root, func(node map[string]any) {
		if ref, ok := node["$ref"].(string); ok {
			direct = append(direct, strings.TrimPrefix(ref, "#/$defs/"))
		}
	})
	if mcpReachesMoney(direct, 1) {
		return true
	}
	return cmd.OutputSchema != "" && mcpReachesMoney([]string{cmd.OutputSchema}, 2)
}

func mcpHelpIndex(all []*Command, policy mcpPolicy) map[string]any {
	groups := map[string]int{}
	var disabled []string
	for _, tool := range all {
		group, _, _ := strings.Cut(tool.CanonicalName, ".")
		if policy.includes(tool.CanonicalName) {
			groups[group]++
		}
	}
	for _, group := range mcpToolGroups(all) {
		if groups[group] == 0 {
			disabled = append(disabled, group)
		}
	}
	findingTools := []string{
		"Call help with topic set to a tool name for its arguments and required fields, or to a group such as customers for its tools.",
		"Call schema_input or schema_output with command set to a tool name for its full JSON input or result schema.",
	}
	if policy.includes("help.search") {
		findingTools = append(findingTools, "Call help_search with a question to search Flint Help community answers.")
	}
	findingTools = append(findingTools, "tools/list returns every enabled tool. Tools in other_groups need the user to restart the server with --tools all, or --tools default,<group>.")
	data := map[string]any{
		"summary":    "Index of Flint MCP help. Tools are named <resource>_<action>; resource_groups lists each enabled group with its number of tools.",
		"tool_set":   policy.toolSetDescription(),
		"tool_count": len(mcpFilterTools(all, policy)),
		"topics": []map[string]string{
			{"topic": "<tool name>", "description": "Arguments, required fields, and controls for one tool, for example customers_create."},
			{"topic": "<group>", "description": "The tools in one group, for example customers."},
			{"topic": "controls", "description": "What each _flint execution control does."},
			{"topic": "events", "description": "Webhook event types with one-line descriptions; schema_events with event_type returns payload schemas."},
			{"topic": "test-cards", "description": "Sandbox payment method tokens and card numbers for test scenarios."},
			{"topic": "exit-codes", "description": "Exit codes and error types returned by Flint commands."},
			{"topic": "errors", "description": "Same catalog as exit-codes."},
		},
		"finding_tools":   findingTools,
		"resource_groups": groups,
	}
	if len(disabled) > 0 {
		data["other_groups"] = disabled
	}
	return map[string]any{"data": data}
}

func mcpFilterTools(all []*Command, policy mcpPolicy) []*Command {
	var tools []*Command
	for _, tool := range all {
		if policy.includes(tool.CanonicalName) {
			tools = append(tools, tool)
		}
	}
	return tools
}

// mcpSchemaType is a short type label for a property in help output.
func mcpSchemaType(property map[string]any) string {
	switch value := property["type"].(type) {
	case string:
		if items, ok := property["items"].(map[string]any); ok && value == "array" {
			return "array of " + mcpSchemaType(items)
		}
		return value
	case []any:
		return strings.Join(mcpStrings(value), " or ")
	}
	if ref, ok := property["$ref"].(string); ok {
		return strings.TrimPrefix(ref, "#/$defs/")
	}
	if _, ok := property["const"]; ok {
		return "const"
	}
	for _, keyword := range []string{"anyOf", "oneOf", "allOf"} {
		var labels []string
		for _, branch := range mcpAnySlice(property[keyword]) {
			if branchMap, ok := branch.(map[string]any); ok {
				if label := mcpSchemaType(branchMap); label != "any" && !containsString(labels, label) {
					labels = append(labels, label)
				}
			}
		}
		if len(labels) > 0 {
			return strings.Join(labels, " or ")
		}
	}
	return "any"
}

// mcpToolHelp is help for one tool in MCP terms, consistent with its input
// schema: argument names, types, and required fields come from that schema.
func mcpToolHelp(cmd *Command, policy mcpPolicy) (map[string]any, *CLIError) {
	schema, e := schemaForMCPTool(cmd, policy)
	if e != nil {
		return nil, e
	}
	name := mcpToolName(cmd.CanonicalName)
	properties, _ := schema["properties"].(map[string]any)
	required := mcpStrings(schema["required"])
	var names []string
	for property := range properties {
		if property != "_flint" {
			names = append(names, property)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := containsString(required, names[i]), containsString(required, names[j])
		if left != right {
			return left
		}
		return names[i] < names[j]
	})
	arguments := []map[string]any{}
	for _, property := range names {
		definition, _ := properties[property].(map[string]any)
		entry := map[string]any{"name": property, "type": mcpSchemaType(definition), "required": containsString(required, property)}
		for _, keyword := range []string{"description", "enum", "default"} {
			if value, ok := definition[keyword]; ok {
				entry[keyword] = value
			}
		}
		arguments = append(arguments, entry)
	}
	controls := []map[string]any{}
	controlSchemas, _ := lookupPath(schema, "properties._flint.properties")
	controlMap, _ := controlSchemas.(map[string]any)
	var controlNames []string
	for control := range controlMap {
		controlNames = append(controlNames, control)
	}
	sort.Strings(controlNames)
	for _, control := range controlNames {
		definition, _ := controlMap[control].(map[string]any)
		controls = append(controls, map[string]any{"name": "_flint." + control, "description": definition["description"]})
	}
	data := map[string]any{
		"name":           name,
		"canonical_name": cmd.CanonicalName,
		"title":          mcpToolTitle(cmd),
		"description":    mcpToolDescription(cmd, schema),
		"enabled":        policy.includes(cmd.CanonicalName),
		"annotations":    mcpToolAnnotations(cmd),
		"required":       required,
		"arguments":      arguments,
		"controls":       controls,
		"schemas":        "Call schema_input with command " + name + " for the full input JSON Schema, including nested fields, and schema_output for the result schema.",
	}
	if data["required"] == nil {
		data["required"] = []string{}
	}
	if summary := mcpRequirementSummary(schema); summary != "" {
		data["requirements"] = summary
	}
	if !policy.includes(cmd.CanonicalName) {
		group, _, _ := strings.Cut(cmd.CanonicalName, ".")
		data["note"] = "This tool is not enabled on this server. The user can enable it by restarting the server with --tools all, or with " + group + " added to --tools."
	}
	return map[string]any{"data": data}, nil
}

// mcpControlHelp documents each _flint control in full; tool schemas carry
// only a one-line description.
var mcpControlHelp = []map[string]string{
	{"name": "_flint.dry_run", "applies_to": "writes", "description": "\"client\" validates the arguments and returns the prepared request (method, path, headers, body) without sending it. Nothing changes in Flint."},
	{"name": "_flint.preview", "applies_to": "destructive writes", "description": "Returns the request and its effect (permission check, reversibility, affected resources) without making the change."},
	{"name": "_flint.idempotency_key", "applies_to": "writes", "description": "Sent as the Idempotency-Key header. Flint returns the original result when a key is reused, so reuse the same key when retrying a write that timed out or failed on the network; a new key could run it twice. The CLI generates a key only when the route makes it optional. Required routes need a key saved before the first call; operations that take no key send none. The API never generates keys. Every error from a keyed write includes its key."},
	{"name": "_flint.confirm", "applies_to": "sensitive or irreversible operations", "description": "Required in every environment. In sandbox the agent confirms. Live and unverified environments also require user approval through elicitation, or return a terminal command; --allow-destructive permits agent confirmation there."},
	{"name": "_flint.live", "applies_to": "API tools, only when the server was started with --allow-live", "description": "Acknowledges a live (production) credential or context. Without --allow-live the server refuses live credentials."},
	{"name": "_flint.merchant", "applies_to": "API tools", "description": "Fails the call unless the credential belongs to this mer_ merchant. An agent may add a guard but cannot replace one the server already uses."},
	{"name": "_flint.profile", "applies_to": "API tools", "description": "Selects a saved Flint CLI profile, unless the server pins one with --profile, FLINT_PROFILE, or project configuration, or the profile would change the server's merchant guard or context."},
	{"name": "_flint.timeout", "applies_to": "tools that call Flint", "description": "Total request budget including retries, as a Go duration. Default: 150s."},
	{"name": "_flint.all", "applies_to": "paginated lists", "description": "Fetches every page and returns them merged as one list with an empty next_page_token. Large lists can exceed the result limit; combine with _flint.select."},
	{"name": "_flint.wait_for", "applies_to": "get tools", "description": "Polls until every FIELD=VALUE condition holds, for example status=succeeded, bounded by _flint.for (default 2m)."},
	{"name": "_flint.for", "applies_to": "waits and streams", "description": "Maximum duration of a wait or stream as a Go duration; streams allow at most 2m."},
	{"name": "_flint.max_events", "applies_to": "streams", "description": "Stops a stream after this many records (at most 100)."},
	{"name": "_flint.expand", "applies_to": "tools with documented expansions", "description": "Includes related resources inline; the schema lists the allowed values."},
	{"name": "_flint.field", "applies_to": "most tools", "description": "Returns only the value at a resource path such as customer_id, as {\"value\": ...}."},
	{"name": "_flint.select", "applies_to": "most tools", "description": "Keeps only the listed fields of each resource, keeping the response envelope."},
	{"name": "_flint.jq", "applies_to": "most tools", "description": "Applies a jq expression to the result, returned as {\"value\": ...}. Cannot be combined with _flint.field."},
}

// mcpEventList is the compact webhook catalog: every event type with its
// one-line summary, plus how deliveries are signed and retried.
func mcpEventList() (map[string]any, *CLIError) {
	summaries, err := eventTypeSummaries()
	doc, docErr := loadOpenAPI()
	if err != nil || docErr != nil {
		return nil, cliError(ExitUsage, errorTypeInternal, "SCHEMA_SNAPSHOT_INVALID", "The embedded webhook catalog could not be read.")
	}
	data := map[string]any{
		"event_types": summaries,
		"count":       len(summaries),
		"next":        "Call schema_events with event_type set to an event type, or a prefix such as payment_intent, for payload schemas.",
	}
	for _, key := range []string{"signing", "retry_policy", "secret_rotation"} {
		if value, ok := doc.WebhookMetadata[key]; ok {
			data[key] = value
		}
	}
	return map[string]any{"data": data}, nil
}

// mcpEventSchemas is the webhook catalog for one event type or prefix.
func mcpEventSchemas(filter string) (any, *CLIError) {
	filter = strings.TrimSuffix(strings.TrimSpace(filter), ".")
	match := func(eventType string) bool { return eventType == filter || strings.HasPrefix(eventType, filter+".") }
	catalog, err := eventCatalogFor(match)
	if err != nil {
		return nil, cliError(ExitUsage, errorTypeInternal, "SCHEMA_SNAPSHOT_INVALID", err.Error())
	}
	if webhooks, _ := catalog["webhooks"].(map[string]any); len(webhooks) > 0 {
		return catalog, nil
	}
	summaries, _ := eventTypeSummaries()
	type candidate struct {
		name     string
		distance int
	}
	var candidates []candidate
	for _, summary := range summaries {
		name := summary["event_type"]
		group, _, _ := strings.Cut(name, ".")
		distance := min(levenshteinDistance(filter, name), levenshteinDistance(filter, group))
		if strings.Contains(name, filter) {
			distance = 0
		}
		if distance <= max(2, len(filter)/3) {
			candidates = append(candidates, candidate{name, distance})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].distance < candidates[j].distance })
	var suggestions []string
	for _, c := range candidates[:min(3, len(candidates))] {
		suggestions = append(suggestions, c.name)
	}
	message := "No webhook event type matches " + filter + "."
	if len(suggestions) > 0 {
		message += " Did you mean " + strings.Join(suggestions, ", ") + "?"
	}
	e := usageError("UNKNOWN_EVENT_TYPE", message+" Call schema_events without event_type to list event types.", "event_type")
	e.Details = []map[string]any{{"input": filter, "suggestions": suggestions}}
	return nil, e
}

// mcpCanonicalArguments maps tool names given to help and the schema tools,
// such as customers_create, to the command names the CLI resolves.
func mcpCanonicalArguments(cmd *Command, arguments map[string]any) map[string]any {
	key := map[string]string{"help": "topic", "schema.input": "command", "schema.output": "command", "schema.command": "command", "schema.commands": "prefix"}[cmd.CanonicalName]
	value, ok := arguments[key].(string)
	if !ok || !strings.Contains(value, "_") {
		return arguments
	}
	out := copyMap(arguments)
	out[key] = mcpCanonicalToolName(value)
	return out
}

// mcpTopicTool resolves a help or schema topic to an MCP tool.
func (a *App) mcpTopicTool(topic string) (*Command, bool) {
	canonical := strings.TrimSpace(topic)
	if alias, ok := a.Registry.ByName(canonical); ok {
		canonical = alias.CanonicalName
	}
	for _, tool := range a.mcpAllTools() {
		if tool.CanonicalName == canonical {
			return tool, true
		}
	}
	return nil, false
}

// mcpLocalToolResult answers the help and schema tools in MCP terms where the
// CLI's answer would describe flags and files, and gates signup. It reports
// false when the CLI command should run as usual.
func (a *App) mcpLocalToolResult(ctx context.Context, cmd *Command, arguments map[string]any, opts Options, policy mcpPolicy, structured bool) (map[string]any, bool) {
	finish := func(value any, e *CLIError) (map[string]any, bool) {
		if e != nil {
			return mcpCommandError(cmd, mcpCallResult{value: mcpCLIErrorEnvelope(e), exit: e.ExitCode}, policy, ""), true
		}
		transformOptions, transformed, e := mcpOutputTransformOptions(cmd, arguments)
		if e == nil && transformed {
			// Transforms run on decoded JSON, as they do for CLI output.
			raw, _ := json.Marshal(value)
			var decoded any
			_ = decodeJSONNumbers(raw, &decoded)
			transformOptions.outputLimit = mcpMaxOutputBytes
			value, e = applyOutputTransforms(ctx, decoded, transformOptions)
		}
		if e != nil {
			return mcpCommandError(cmd, mcpCallResult{value: mcpCLIErrorEnvelope(e), exit: e.ExitCode}, policy, ""), true
		}
		if refused := mcpResultTooLarge(cmd, arguments, value); refused != nil {
			return refused, true
		}
		return mcpToolResult(value, structured, transformed, ""), true
	}
	switch cmd.CanonicalName {
	case "help":
		topic, _ := arguments["topic"].(string)
		topic = strings.TrimSpace(topic)
		all := a.mcpAllTools()
		switch topic {
		case "":
			return finish(mcpHelpIndex(all, policy), nil)
		case "controls":
			return finish(map[string]any{"data": map[string]any{"controls": mcpControlHelp, "usage": "Pass controls inside the _flint object argument, for example {\"_flint\": {\"dry_run\": \"client\"}}. Each tool's schema lists the controls it accepts."}}, nil)
		case "events":
			return finish(mcpEventList())
		}
		if tool, ok := a.mcpTopicTool(topic); ok {
			return finish(mcpToolHelp(tool, policy))
		}
		var group []map[string]any
		for _, tool := range all {
			if strings.HasPrefix(tool.CanonicalName, topic+".") {
				group = append(group, map[string]any{"name": mcpToolName(tool.CanonicalName), "title": mcpToolTitle(tool), "enabled": policy.includes(tool.CanonicalName)})
			}
		}
		if len(group) > 0 {
			return finish(map[string]any{"data": map[string]any{"group": mcpToolName(topic), "tools": group, "next": "Call help with a tool name for its arguments."}}, nil)
		}
	case "schema.input":
		command, _ := arguments["command"].(string)
		if tool, ok := a.mcpTopicTool(command); ok {
			schema, e := schemaForMCPTool(tool, policy)
			return finish(schema, e)
		}
	case "schema.events":
		if filter, ok := arguments["event_type"].(string); ok {
			return finish(mcpEventSchemas(filter))
		}
		return finish(mcpEventList())
	case "signup":
		if message := a.mcpSignupViolation(opts); message != "" {
			return mcpToolError(message, nil), true
		}
	}
	return nil, false
}

// mcpSignupViolation refuses signup when it would replace the credential the
// human already saved for the server's profile.
func (a *App) mcpSignupViolation(opts Options) string {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return "" // Signup reports the configuration problem itself.
	}
	existing, err := a.loadSavedCredential(resolved.ProfileName)
	if err != nil || strings.TrimSpace(existing) == "" {
		return ""
	}
	return "signup is not available here: profile " + resolved.ProfileName + " already has a saved Flint credential, and signup would replace it with a new account's key. Do not retry. The user already has a Flint login; if they want a new account, they can run flint signup in a terminal."
}

var errMCPLiveBlocked = errors.New("live access is disabled on this MCP server")

var mcpCredentialMessages = map[string]string{
	"API_KEY_REQUIRED":         "No Flint credential is configured for this MCP server.",
	"CREDENTIAL_LOOKUP_FAILED": "This MCP server could not read its configured Flint credential store.",
}

const mcpCredentialRemedy = " Do not retry. Tell the user to set FLINT_API_KEY (a sandbox flint_test_ key from " + dashboardAPIKeysURL + ") or FLINT_PROFILE (a profile saved with flint login, using --credential-store file when the OS keyring is unavailable) in this server's env in their MCP client configuration, then restart the server."

// mcpTranslateDetails rewrites CLI flag advice anywhere in error details,
// leaving commands meant for the human and identifiers unchanged.
