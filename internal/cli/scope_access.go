package cli

import (
	"sort"
	"strings"
	"sync"

	"github.com/flint-pay/flint-cli/internal/opindex"
	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

func credentialScopesKnown(auth AuthContext) bool {
	return auth.AuthType == "api_key" || auth.isCLILogin() || auth.AuthType == "workload_identity" || auth.WorkloadIdentityID != ""
}

var scopeRequirementIndex = sync.OnceValues(func() (*opindex.Index, error) {
	return opindex.New(nil, apispec.Startup().Metadata)
})

func commandScopeRequirement(cmd *Command) *opindex.ScopeRequirement {
	if cmd == nil || cmd.OperationID == "" {
		return nil
	}
	operation, ok := openAPIOperationByID(cmd.OperationID)
	if !ok || len(operation.FlintRequiredScopes) == 0 {
		return nil
	}
	mode := operation.FlintScopesMode
	if mode == "" {
		mode = "all"
	}
	return &opindex.ScopeRequirement{Mode: mode, Scopes: append([]string(nil), operation.FlintRequiredScopes...)}
}

type scopeAccessGroup struct {
	Group           string   `json:"group"`
	ScopedCommands  int      `json:"scoped_commands"`
	BlockedCommands []string `json:"blocked_commands"`
}

type scopeAccessSummary struct {
	Status                string             `json:"status"`
	ScopedCommands        int                `json:"scoped_commands"`
	AllowedCommands       int                `json:"allowed_commands"`
	BlockedCommands       []string           `json:"blocked_commands"`
	BlockedResourceGroups []scopeAccessGroup `json:"blocked_resource_groups"`
}

// Scope access describes only scoped commands accepted by this credential
// class. Counts retain partial group access rather than calling a group denied.
func (a *App) scopeAccess(auth AuthContext) scopeAccessSummary {
	result := scopeAccessSummary{Status: "unknown", BlockedCommands: []string{}, BlockedResourceGroups: []scopeAccessGroup{}}
	if !credentialScopesKnown(auth) {
		return result
	}
	index, err := a.operationIndex()
	if err != nil {
		return result
	}
	result.Status = "known"
	credential := opindex.ClassifyCredential(opindex.CredentialAPIKey, "")
	groups := map[string]*scopeAccessGroup{}
	for _, entry := range index.Entries {
		if entry.Type != "operation" || entry.Audience == "local" || entry.ScopeRequirement == nil || !credential.Allows(entry) {
			continue
		}
		result.ScopedCommands++
		groupName := strings.SplitN(entry.ID, ".", 2)[0]
		group := groups[groupName]
		if group == nil {
			group = &scopeAccessGroup{Group: groupName, BlockedCommands: []string{}}
			groups[groupName] = group
		}
		group.ScopedCommands++
		if len(index.MissingScopes(entry, auth.Scopes, true)) == 0 {
			result.AllowedCommands++
			continue
		}
		result.BlockedCommands = append(result.BlockedCommands, entry.ID)
		group.BlockedCommands = append(group.BlockedCommands, entry.ID)
	}
	sort.Strings(result.BlockedCommands)
	for _, group := range groups {
		if len(group.BlockedCommands) > 0 {
			sort.Strings(group.BlockedCommands)
			result.BlockedResourceGroups = append(result.BlockedResourceGroups, *group)
		}
	}
	sort.Slice(result.BlockedResourceGroups, func(i, j int) bool {
		return result.BlockedResourceGroups[i].Group < result.BlockedResourceGroups[j].Group
	})
	return result
}

func scopeAccessOutputSchema() map[string]any {
	stringsSchema := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return map[string]any{"type": "object", "properties": map[string]any{
		"status":          map[string]any{"type": "string", "enum": []string{"known", "unknown"}},
		"scoped_commands": map[string]any{"type": "integer"}, "allowed_commands": map[string]any{"type": "integer"},
		"blocked_commands": stringsSchema,
		"blocked_resource_groups": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
			"group": map[string]any{"type": "string"}, "scoped_commands": map[string]any{"type": "integer"}, "blocked_commands": stringsSchema,
		}}},
	}}
}
