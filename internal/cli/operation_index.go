package cli

import (
	"crypto/sha256"
	"encoding/json"
	"github.com/flint-pay/flint-cli/internal/opindex"
	apispec "github.com/flint-pay/flint-cli/internal/spec"
	"sync"
)

// Indexes contain only immutable contract and registry metadata. Credentials and
// server exposure filters remain per caller.
var operationIndexes sync.Map

// operationIndex is the registry boundary shared by discovery and offline search.
func (a *App) operationIndex() (*opindex.Index, error) {
	a.Registry.ensureAPICommands()
	commands := make([]opindex.Command, 0, len(a.Registry.Commands))
	for _, cmd := range a.Registry.Commands {
		// Trigger is hidden by the legacy MCP server; the approved discovery index exposes it.
		commands = append(commands, opindex.Command{Name: cmd.Name, CanonicalName: cmd.CanonicalName, Title: cmd.Description, Method: cmd.Method, Path: cmd.APIPath, OperationID: cmd.OperationID, Aliases: cmd.Aliases, Local: cmd.Local, MCPHidden: cmd.MCPHidden && cmd.Name != "trigger", Mutation: cmd.Mutation, Get: cmd.Get, Stream: cmd.Stream, Paginated: cmd.Supports.Pagination})
	}
	metadata, err := json.Marshal(commands)
	if err != nil {
		return nil, err
	}
	fingerprint := sha256.Sum256(metadata)
	build, _ := operationIndexes.LoadOrStore(fingerprint, sync.OnceValues(func() (*opindex.Index, error) {
		return opindex.New(commands, apispec.OpenAPI)
	}))
	return build.(func() (*opindex.Index, error))()
}

// Keep one credential adapter for future CLI and MCP callers.
func credentialSchemes(source, token string) []string {
	return opindex.CredentialSchemes(source, token)
}

// The terminal shares classifications with MCP, with less ceremony for
// reversible sandbox writes. MCP keeps its own agent confirmation policy.
func terminalConfirmationRequired(entry opindex.Entry, environment string) bool {
	if entry.Kind != "write" {
		return false
	}
	if normalizeEnvironment(environment) == "sandbox" {
		return entry.Irreversible
	}
	return entry.ConfirmRequired()
}
