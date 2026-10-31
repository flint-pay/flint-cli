package opindex

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type operation struct {
	ID            string                `json:"operationId"`
	Summary       string                `json:"summary"`
	Description   string                `json:"description"`
	Security      []map[string][]string `json:"security"`
	Destructive   bool                  `json:"x-flint-destructive"`
	RouteClass    string                `json:"x-flint-route-class"`
	Audience      string                `json:"x-flint-audience"`
	PartnerAccess string                `json:"x-flint-partner-access"`
	Scopes        []string              `json:"x-flint-required-scopes"`
	ScopeMode     string                `json:"x-flint-required-scopes-mode"`
	Parameters    []struct {
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
	} `json:"parameters"`
}

type operationRoute struct {
	operation
	Method, Path string
}

var localReads = map[string]bool{"version": true, "doctor": true, "config.get": true, "config.validate": true, "history": true, "auth.status": true, "help": true, "help.search": true, "context.list": true, "support.open": true, "completion": true}
var localExposed = map[string]bool{"version": true, "doctor": true, "init": true, "config.get": true, "config.validate": true, "history": true, "history.clear": true, "signup": true, "listen": true, "auth.status": true, "trigger": true}
var hidden = map[string]bool{"mcp.serve": true, "config.set": true, "config.unset": true, "auth.logout": true, "api": true, "help": true, "help.search": true}

// New builds metadata without credentials or IO. Registry names are authoritative.
func New(commands []Command, document []byte) (*Index, error) {
	var doc struct {
		Paths    map[string]map[string]operation `json:"paths"`
		Webhooks map[string]struct {
			Post operation `json:"post"`
		} `json:"webhooks"`
		Scopes []struct {
			Name      string   `json:"name"`
			ImpliedBy []string `json:"implied_by"`
		} `json:"x-flint-scopes"`
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		return nil, fmt.Errorf("read operation contract: %w", err)
	}
	routes := map[string]operationRoute{}
	for path, methods := range doc.Paths {
		for method, op := range methods {
			if op.ID != "" {
				routes[op.ID] = operationRoute{op, strings.ToUpper(method), path}
			}
		}
	}
	ix := &Index{searchOnce: &sync.Once{}, byID: map[string]Entry{}, names: map[string]string{}, impliedBy: map[string][]string{}, credentialPolicies: map[string]Entry{}}
	for id, route := range routes {
		schemes := securitySchemes(route.Security)
		kind := "write"
		if route.Method == "GET" && id != "authorizePartnerInstall" {
			kind = "read"
		}
		ix.credentialPolicies[id] = Entry{OperationID: id, Method: route.Method, Path: route.Path, Kind: kind, Schemes: schemes, Audience: audience(schemes), PartnerAccess: route.PartnerAccess == "install_scoped",
			Sensitive:    route.RouteClass == "payment" || route.RouteClass == "sensitive_write" || route.RouteClass == "external_provider_action",
			Irreversible: route.Destructive}
	}
	for id, route := range routes {
		if route.Audience == "flint_hosted" {
			policy := ix.credentialPolicies[id]
			policy.Audience = "flint_hosted"
			ix.credentialPolicies[id] = policy
		}
	}
	for _, scope := range doc.Scopes {
		ix.impliedBy[scope.Name] = append([]string(nil), scope.ImpliedBy...)
	}
	seen := map[string]bool{}
	for _, cmd := range commands {
		if cmd.CanonicalName == "" {
			cmd.CanonicalName = cmd.Name
		}
		if cmd.Name != cmd.CanonicalName || cmd.Name == "__complete" || seen[cmd.Name] {
			continue
		}
		seen[cmd.Name] = true
		entry := Entry{ID: cmd.Name, Type: "operation", Title: shortText(cmd.Title, 1, 300), OperationID: cmd.OperationID, Aliases: append([]string(nil), cmd.Aliases...), Get: cmd.Get, Stream: cmd.Stream, Paginated: cmd.Paginated, Idempotency: "none"}
		route, hasRoute := routes[cmd.OperationID]
		local := cmd.Local || cmd.Name == "auth.status" || cmd.Name == "listen" || !hasRoute
		if local {
			entry.Audience = "local"
			entry.Kind = "write"
			if localReads[cmd.Name] || strings.HasPrefix(cmd.Name, "schema.") {
				entry.Kind = "read"
			}
			entry.Irreversible = cmd.Name == "history.clear"
		} else {
			entry.Method, entry.Path = route.Method, route.Path
			entry.Description = shortText(route.Description, 2, 300)
			entry.Kind = "write"
			if route.Method == "GET" && !cmd.Mutation && route.ID != "authorizePartnerInstall" {
				entry.Kind = "read"
			}
			policy := ix.credentialPolicies[cmd.OperationID]
			entry.Schemes, entry.Audience, entry.PartnerAccess = policy.Schemes, policy.Audience, policy.PartnerAccess
			entry.Overwrites = route.Method == "PUT" || route.Method == "PATCH"
			entry.Sensitive = policy.Sensitive
			entry.Irreversible = policy.Irreversible
			for _, parameter := range route.Parameters {
				if parameter.In == "header" && parameter.Name == "Idempotency-Key" {
					entry.Idempotency = "optional"
					if parameter.Required {
						entry.Idempotency = "required"
					}
				}
			}
			if len(route.Scopes) > 0 {
				mode := route.ScopeMode
				if mode == "" {
					mode = "all"
				}
				if mode != "all" && mode != "any" {
					return nil, fmt.Errorf("%s has invalid scope mode %q", entry.ID, mode)
				}
				entry.ScopeRequirement = &ScopeRequirement{mode, append([]string(nil), route.Scopes...)}
			}
		}
		entry.MCP = !cmd.MCPHidden && !hidden[entry.ID] && !strings.HasPrefix(entry.ID, "schema.") && entry.Audience != "keyless" && entry.Audience != "flint_hosted" && (!local || localExposed[entry.ID])
		ix.Entries = append(ix.Entries, entry)
	}
	if seen["history"] && !seen["history.clear"] {
		ix.Entries = append(ix.Entries, Entry{ID: "history.clear", Type: "operation", Title: "Clear recent Flint resource IDs", Kind: "write", Audience: "local", Irreversible: true, Idempotency: "none", MCP: true})
	}
	for event, webhook := range doc.Webhooks {
		ix.Entries = append(ix.Entries, Entry{ID: event, Type: "event", Title: shortText(webhook.Post.Summary, 1, 300), Description: shortText(webhook.Post.Description, 2, 300), MCP: true})
	}
	for _, topic := range []struct{ ID, Title string }{{"safety", "Live access, confirm, dry runs, and idempotency keys"}, {"errors", "Error shape, retry values, and next actions"}, {"events", "Webhook event types"}, {"test-cards", "Sandbox test cards and tokens"}, {"exit-codes", "CLI exit codes and their meanings"}} {
		ix.Entries = append(ix.Entries, Entry{ID: topic.ID, Type: "topic", Title: topic.Title, MCP: topic.ID != "exit-codes"})
	}
	for i, entry := range ix.Entries {
		if entry.Type != "operation" {
			continue
		}
		if entry.Kind == "write" {
			for _, read := range ix.Entries {
				if read.Type != "operation" || read.Method != "GET" || read.Path != entry.Path {
					continue
				}
				for _, parameter := range routes[read.OperationID].Parameters {
					if parameter.In == "query" && parameter.Name == "idempotency_key" {
						ix.Entries[i].RecoveryRead = read.ID
					}
				}
			}
		}
	}
	sort.Slice(ix.Entries, func(i, j int) bool {
		if ix.Entries[i].Type != ix.Entries[j].Type {
			return ix.Entries[i].Type < ix.Entries[j].Type
		}
		return ix.Entries[i].ID < ix.Entries[j].ID
	})
	typed := map[string]string{}
	validName := regexp.MustCompile(`^[a-z0-9_]{1,52}$`)
	for _, entry := range ix.Entries {
		if entry.Type != "operation" {
			continue
		}
		ix.byID[entry.ID] = entry
		ix.names[entry.ID] = entry.ID
		for _, alias := range entry.Aliases {
			ix.names[alias] = entry.ID
		}
		name := ToolName(entry.ID)
		if !validName.MatchString(name) {
			return nil, fmt.Errorf("invalid typed name for %s: %s", entry.ID, name)
		}
		if old, exists := typed[name]; exists && old != entry.ID {
			return nil, fmt.Errorf("typed name collision: %s and %s", old, entry.ID)
		}
		typed[name] = entry.ID
		ix.names[name] = entry.ID
	}
	return ix, nil
}

func ToolName(id string) string {
	return strings.NewReplacer(".", "_", "-", "_").Replace(strings.ToLower(id))
}

func (ix *Index) Lookup(name string) (Entry, bool) {
	name = strings.Join(strings.Fields(strings.TrimSpace(name)), ".")
	entry, ok := ix.byID[ix.names[name]]
	return entry, ok
}

func securitySchemes(security []map[string][]string) []string {
	names := map[string]bool{}
	for _, requirement := range security {
		for name := range requirement {
			names[name] = true
		}
	}
	result := []string{}
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func audience(schemes []string) string {
	if len(schemes) == 0 {
		return "keyless"
	}
	for _, scheme := range schemes {
		if scheme != "CustomerSessionBearer" && scheme != "InvoiceAccessTokenBearer" && scheme != "CheckoutSessionBearer" {
			return "merchant"
		}
	}
	return "buyer"
}

func shortText(value string, sentences, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	end := 0
	for range sentences {
		next := strings.Index(value[end:], ". ")
		if next < 0 {
			end = len(value)
			break
		}
		end += next + 2
	}
	value = strings.TrimSpace(value[:end])
	runes := []rune(value)
	if len(runes) > limit {
		value = strings.TrimSpace(string(runes[:limit-3])) + "..."
	}
	return value
}
