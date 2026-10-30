package cli

import (
	"github.com/flint-pay/flint-cli/internal/opindex"
	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

// operationIndex is the registry boundary shared by discovery and offline search.
func (a *App) operationIndex() (*opindex.Index, error) {
	a.Registry.ensureAPICommands()
	commands := make([]opindex.Command, 0, len(a.Registry.Commands))
	for _, cmd := range a.Registry.Commands {
		commands = append(commands, opindex.Command{Name: cmd.Name, CanonicalName: cmd.CanonicalName, Title: cmd.Description, Method: cmd.Method, Path: cmd.APIPath, OperationID: cmd.OperationID, Aliases: cmd.Aliases, Local: cmd.Local, MCPHidden: cmd.MCPHidden, Mutation: cmd.Mutation, Get: cmd.Get, Stream: cmd.Stream, Paginated: cmd.Supports.Pagination})
	}
	return opindex.New(commands, apispec.OpenAPI)
}

// Keep one credential adapter for future CLI and MCP callers.
func credentialSchemes(source, token string) []string {
	return opindex.CredentialSchemes(source, token)
}
