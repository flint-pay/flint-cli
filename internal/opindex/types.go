// Package opindex derives shared discovery metadata from canonical CLI commands
// and the embedded public API contract. It performs no network requests.
package opindex

// Command is the registry boundary; the index never assigns command names.
type Command struct {
	Name, CanonicalName, Title, Method, Path, OperationID string
	Aliases                                               []string
	Local, MCPHidden, Mutation, Get, Stream, Paginated    bool
}

type ScopeRequirement struct {
	Mode   string   `json:"mode"`
	Scopes []string `json:"scopes"`
}

type Entry struct {
	ID               string            `json:"id"`
	Type             string            `json:"type"`
	Title            string            `json:"title"`
	Description      string            `json:"description,omitempty"`
	Kind             string            `json:"kind,omitempty"`
	Method           string            `json:"method,omitempty"`
	Path             string            `json:"path,omitempty"`
	OperationID      string            `json:"operation_id,omitempty"`
	Audience         string            `json:"audience,omitempty"`
	Schemes          []string          `json:"schemes,omitempty"`
	PartnerAccess    bool              `json:"partner_access,omitempty"`
	Irreversible     bool              `json:"irreversible,omitempty"`
	Overwrites       bool              `json:"overwrites,omitempty"`
	Sensitive        bool              `json:"sensitive,omitempty"`
	Idempotency      string            `json:"idempotency,omitempty"`
	Paginated        bool              `json:"paginated,omitempty"`
	Get              bool              `json:"get,omitempty"`
	Stream           bool              `json:"stream,omitempty"`
	ScopeRequirement *ScopeRequirement `json:"scope_requirement,omitempty"`
	Aliases          []string          `json:"aliases,omitempty"`
	MCP              bool              `json:"mcp"`
	RecoveryRead     string            `json:"recovery_read,omitempty"`
}

func (e Entry) ConfirmRequired() bool { return e.Irreversible || e.Sensitive }

type Index struct {
	Entries   []Entry
	byID      map[string]Entry
	names     map[string]string
	impliedBy map[string][]string
	// The bridge test trips when the embedded spec has taken over every derived mark.
	DerivedDestructive, PublishedDestructive int
}

type Filter struct {
	Kind    string
	Exposed func(Entry) bool
	Limit   int
}

type Hit struct {
	Entry
	Score float64 `json:"-"`
}
