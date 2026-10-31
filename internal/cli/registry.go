package cli

import (
	"sort"
	"strings"
	"sync"
)

type Registry struct {
	tree      *CommandTree
	treeBuild *sync.Once
	Commands  []*Command
	localOnly bool
	byName    map[string]*Command
	byPath    map[string]*Command
}

func (r *Registry) commandsUnder(prefix []string) []*Command {
	r.ensureAPICommands()
	var commands []*Command
	for _, command := range r.Commands {
		if len(command.Path) <= len(prefix) {
			continue
		}
		matches := true
		for i, segment := range prefix {
			if command.Path[i] != segment {
				matches = false
				break
			}
		}
		if matches {
			commands = append(commands, command)
		}
	}
	return commands
}

func NewRegistry() *Registry { return newRegistry(true) }

func newLocalRegistry() *Registry { return newRegistry(false) }

func (r *Registry) ensureAPICommands() {
	if r.localOnly {
		*r = *NewRegistry()
	}
}

func newRegistryFromSnapshot(includeAPI bool) *Registry {
	r := &Registry{treeBuild: &sync.Once{}, localOnly: !includeAPI, byName: map[string]*Command{}, byPath: map[string]*Command{}}
	definitions := append(curatedCommandDefinitions(), triggerCommand(), completionCommand())
	if includeAPI {
		definitions = append(definitions, publicAPICommands()...)
	}
	for _, c := range definitions {
		if !includeAPI && !c.Local {
			continue
		}
		if c.CanonicalName == "" {
			c.CanonicalName = c.Name
		}
		if len(c.CanonicalPath) == 0 {
			c.CanonicalPath = append([]string(nil), c.Path...)
		}
		if !c.Local {
			c.AuthRequired = true
			if c.EnvironmentAffinity == "" {
				c.EnvironmentAffinity = EnvironmentAffinityMerchant
			}
		}
		switch strings.Split(strings.TrimPrefix(c.APIPath, "/v1/"), "/")[0] {
		case "organizations", "api-keys", "sandboxes":
			c.EnvironmentAffinity = EnvironmentAffinityAccount
		}
		if c.OperationID == "getAuthContext" {
			c.EnvironmentAffinity = EnvironmentAffinityAccount
		}

		if operation, ok := registryOperation(c); ok && operation.FlintRouteClass != "" {
			// Help metadata describes the contract's sensitive operations. The
			// confirmation gate reads the shared operation index directly.
			c.Sensitive = operation.FlintRouteClass == "sensitive_write" || operation.FlintRouteClass == "external_provider_action" || operation.FlintRouteClass == "payment"
		}
		c.Supports.JSONOutput = true
		c.Supports.NoInput = true
		transforms := !c.Stream && c.CanonicalName != "mcp.serve" && c.CanonicalName != "auth.token"
		c.Supports.Field = transforms
		c.Supports.Select = transforms
		c.Supports.JQ = transforms
		if c.Mutation {
			c.Supports.DryRunClient = true
			c.Supports.IdempotencyKey = true
		}
		if c.Get && c.CanonicalName != "auth.status" {
			c.Supports.WaitFor = true
		}
		var parameters map[string]map[string]any
		if c.OperationID != "" {
			parameters = operationParameterSchemas(c.OperationID)
		}
		for i := range c.Arguments {
			arg := &c.Arguments[i]
			if schema := parameters["query:"+arg.Query]; arg.Query != "" && schema != nil {
				applyQueryArgumentContract(arg, schema)
			}
		}
		for _, argument := range c.Arguments {
			if argument.Name == "page_size" {
				c.Supports.Pagination = true
			}
		}
		if c.Stream || c.Supports.Pagination || c.CanonicalName == "trigger" {
			c.Supports.NDJSON = true
		}
		switch c.CanonicalName {
		case "help.search":
			c.Render = "help_search"
		case "support.open":
			c.Render = "support_open"
		}
		if !c.Local {
			applyPublicOperationMetadata(c)
		}
		applyArgumentValues(c)
		if commandIdempotencyClass(c) == "required" {
			for i, example := range c.Examples {
				if !strings.Contains(example, "--idempotency-key") {
					c.Examples[i] = example + " --idempotency-key saved-action-1"
				}
			}
		}
		r.add(c)
	}
	sort.Slice(r.Commands, func(i, j int) bool { return r.Commands[i].Name < r.Commands[j].Name })
	if includeAPI {
		index, err := (&App{Registry: r}).operationIndex()
		if err != nil {
			panic(err)
		}
		for _, c := range r.Commands {
			entry, _ := index.Lookup(c.CanonicalName)
			if entry.Kind != "write" {
				continue
			}
			for i, example := range c.Examples {
				environment := "sandbox"
				if strings.Contains(example, "--live") {
					environment = "live"
				}
				if !terminalConfirmationRequired(entry, environment) {
					continue
				}
				if !strings.Contains(example, "--confirm") && !strings.Contains(example, "--preview") && !strings.Contains(example, "--dry-run") {
					c.Examples[i] = example + " --confirm"
				}
			}
		}
	}
	// Populate command metadata before MCP readers can copy the registry.
	r.treeSnapshot()
	return r
}

func (r *Registry) add(c *Command) {
	if r.treeBuild == nil {
		r.treeBuild = &sync.Once{}
	}
	r.Commands = append(r.Commands, c)
	r.byName[c.Name] = c
	r.byPath[strings.Join(c.Path, " ")] = c
	for i, alias := range c.Aliases {
		r.byName[alias] = c
		if i < len(c.AliasPaths) {
			r.byPath[strings.Join(c.AliasPaths[i], " ")] = c
		}
	}
}

func (r *Registry) ByName(name string) (*Command, bool) {
	c, ok := r.byName[name]
	if !ok && r.localOnly {
		r.ensureAPICommands()
		c, ok = r.byName[name]
	}
	return c, ok
}
func (r *Registry) ByPath(path []string) (*Command, bool) {
	c, ok := r.byPath[strings.Join(path, " ")]
	if !ok && r.localOnly {
		localPrefix := false
		for n := len(path) - 1; n > 1; n-- {
			if _, exists := r.byPath[strings.Join(path[:n], " ")]; exists {
				localPrefix = true
				break
			}
		}
		if !localPrefix {
			r.ensureAPICommands()
			c, ok = r.byPath[strings.Join(path, " ")]
		}
	}
	return c, ok
}

func pathWithFlint(parts ...string) []string { return append([]string{"flint"}, parts...) }
func ex(lines ...string) []string            { return lines }
func pos(name, prefix, description string) Arg {
	return Arg{Name: name, Type: "string", Required: true, Positional: 1, IDPrefix: prefix, AcceptsHistoryRef: true, Description: description}
}
func flag(name, typ, body, query, description string, required bool) Arg {
	return Arg{Name: name, Flag: "--" + strings.ReplaceAll(name, "_", "-"), Type: typ, BodyPath: body, Query: query, Required: required, Description: description}
}
func idFlag(name, prefix, body, query, description string, required bool) Arg {
	argument := flag(name, "string", body, query, description, required)
	argument.IDPrefix = prefix
	argument.AcceptsHistoryRef = true
	return argument
}

func apiCommand(name, description, method, path, operation, inSchema, outSchema string, args []Arg, mutation, sensitive, destructive, get bool, examples ...string) *Command {
	parts := strings.Split(name, ".")
	return &Command{Name: name, CanonicalName: name, Path: pathWithFlint(parts...), Description: description, Method: method, APIPath: path, OperationID: operation, InputSchema: inSchema, OutputSchema: outSchema, Arguments: args, Mutation: mutation, Sensitive: sensitive, Destructive: destructive, Get: get, Examples: examples}
}

func alias(c *Command, names ...string) *Command {
	for _, name := range names {
		c.Aliases = append(c.Aliases, name)
		c.AliasPaths = append(c.AliasPaths, pathWithFlint(strings.Split(name, ".")...))
	}
	return c
}

func commandDefinitions() []*Command {
	commands := curatedCommandDefinitions()
	return append(commands, publicAPICommands()...)
}

func registryOperation(c *Command) (openAPIOperation, bool) {
	if c.OperationID == "" {
		return openAPIOperation{}, false
	}
	return openAPIOperationByID(c.OperationID)
}

func curatedCommandDefinitions() []*Command {
	page := []Arg{flag("page_size", "integer", "", "page_size", "Number of resources per page", false), flag("page_token", "string", "", "page_token", "Pagination token", false)}
	input := flag("input", "string", "", "", "JSON body: FILE.json, - for stdin, or inline JSON", false)
	commands := []*Command{
		{Name: "init", Mutation: true, Path: pathWithFlint("init"), Description: "Write or validate .flint/config.json project guards; commit this file with your project", Local: true, Arguments: []Arg{{Name: "force", Flag: "--force", Type: "boolean", Description: "Replace the current project config with guards from the active credential"}}, Examples: ex("flint init", "flint init --force")},
		alias(&Command{Name: "auth.login", Path: pathWithFlint("auth", "login"), Description: "Sign in through the Flint website (recommended for local development)", Local: true, MCPHidden: true, Arguments: []Arg{{Name: "no_wait", Flag: "--no-wait", Type: "boolean", Description: "Save pending approval and exit; run the returned next_command to resume"}, {Name: "new_session", Flag: "--new-session", Type: "boolean", Description: "Replace the saved session after browser approval"}, {Name: "scope", Flag: "--scope", Type: "string", Repeat: true, Description: "Request an additional public API scope"}, {Name: "no_open", Flag: "--no-open", Type: "boolean", Description: "Print the sign-in link without opening a browser"}}, Examples: ex("flint login", "flint auth login", "flint auth login --no-open", "flint auth login --new-session --scope commerce.subscriptions.read", "flint auth login --live --profile production")}, "login"),
		alias(&Command{Name: "auth.reauth", Path: pathWithFlint("auth", "reauth"), Description: "Change authorized contexts or add scopes in the browser", Local: true, MCPHidden: true, Arguments: []Arg{{Name: "scope", Flag: "--scope", Type: "string", Repeat: true, Description: "Add a public API scope to the active context"}, {Name: "no_open", Flag: "--no-open", Type: "boolean"}}, Examples: ex("flint reauth", "flint reauth --scope commerce.subscriptions.read")}, "reauth"),
		{Name: "context.list", Path: pathWithFlint("context", "list"), Description: "List authorized contexts and the active selection", Local: true, MCPHidden: true, Examples: ex("flint context list")},
		{Name: "context.switch", Path: pathWithFlint("context", "switch"), Description: "Choose the active merchant and environment", Local: true, MCPHidden: true, Arguments: []Arg{{Name: "context_id", Type: "string", Positional: 1}}, Examples: ex("flint context switch", "flint context switch ctx_123", "flint context switch ctx_live --live")},
		{Name: "auth.token", Path: pathWithFlint("auth", "token"), Description: "Export an OAuth or workload identity access token as JSON", Local: true, MCPHidden: true, Examples: ex("flint auth token --output json")},
		{Name: "auth.import", Path: pathWithFlint("auth", "import"), Description: "Manually import and validate a Flint API key", Local: true, MCPHidden: true, Arguments: []Arg{{Name: "stdin", Flag: "--stdin", Type: "boolean", Description: "Read the key from stdin"}}, Examples: ex("flint auth import --stdin < token.txt")},
		alias(apiCommand("auth.status", "Fetch the current credential context from the API without using cached metadata", "GET", "/v1/auth-context", "getAuthContext", "", "AuthContextResponse", nil, false, false, false, true, "flint auth status --output json"), "whoami"),
		alias(&Command{Name: "auth.logout", Path: pathWithFlint("auth", "logout"), Description: "Revoke the active OAuth session and delete the active profile", Local: true, Destructive: true, Examples: ex("flint auth logout --confirm")}, "logout"),
		{Name: "config.get", Path: pathWithFlint("config", "get"), Description: "Print resolved Flint CLI configuration and saved profiles", Local: true, Examples: ex("flint config get --output json")},
		{Name: "config.set", Path: pathWithFlint("config", "set"), Description: "Set profile or merchant guard configuration", Local: true, Arguments: []Arg{{Name: "key", Type: "string", Required: true, Positional: 1}, {Name: "value", Type: "string", Required: true, Positional: 2}}, Examples: ex("flint config set profile default", "flint config set merchant mer_123")},
		{Name: "config.unset", Path: pathWithFlint("config", "unset"), Description: "Clear a saved profile setting", Local: true, Arguments: []Arg{{Name: "key", Type: "string", Required: true, Positional: 1}}, Examples: ex("flint config unset merchant", "flint config unset profile")},
		{Name: "config.validate", Path: pathWithFlint("config", "validate"), Description: "Validate global and project Flint configuration", Local: true, Examples: ex("flint config validate --output json")},
		{Name: "doctor", Path: pathWithFlint("doctor"), Description: "Check credential, connectivity, scopes, and API compatibility; explain scopes that need a live key", Local: true, Arguments: []Arg{{Name: "fix", Flag: "--fix", Type: "boolean", Description: "Repair a stale default profile setting"}}, Examples: ex("flint doctor")},
		{Name: "history", Path: pathWithFlint("history"), Description: "List or clear recent Flint resource IDs", Local: true, Arguments: []Arg{{Name: "clear", Flag: "--clear", Type: "boolean", Description: "Delete the saved history; requires --confirm"}}, Examples: ex("flint history", "flint history --clear --confirm")},
		{Name: "upgrade", Path: pathWithFlint("upgrade"), Description: "Upgrade Flint CLI to the latest stable release", Local: true, MCPHidden: true, Arguments: []Arg{{Name: "check", Flag: "--check", Type: "boolean", Description: "Report the current and latest stable versions without installing anything"}}, Examples: ex("flint upgrade", "flint upgrade --check", "flint upgrade --check --output json", "flint upgrade --output json")},
		{Name: "version", Path: pathWithFlint("version"), Description: "Print CLI and public API schema version metadata", Local: true, Examples: ex("flint version --output json")},
	}
	commands = append(commands, additionalCommands(page, input)...)
	return commands
}
