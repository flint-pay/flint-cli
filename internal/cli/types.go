package cli

import (
	"context"
	"io"
	"net/http"
	"time"
)

const (
	ExitOK           = 0
	ExitAPI          = 1
	ExitUsage        = 2
	ExitAuth         = 3
	ExitConfirmation = 4
	ExitNetwork      = 5
	ExitWait         = 6
	ExitSoftware     = 70
)

type BuildInfo struct {
	Version    string `json:"cli_version"`
	Commit     string `json:"git_commit"`
	BuildDate  string `json:"build_date"`
	APIVersion string `json:"api_version"`
	SchemaHash string `json:"schema_hash"`
}

type Arg struct {
	bodyGenerated     bool
	Discriminator     string   `json:"discriminator,omitempty"`
	InferenceHint     string   `json:"inference_hint,omitempty"`
	Nullable          bool     `json:"nullable,omitempty"`
	Variants          []string `json:"variants,omitempty"`
	RequiredFor       []string `json:"required_for,omitempty"`
	Name              string   `json:"name"`
	Flag              string   `json:"flag,omitempty"`
	Type              string   `json:"type"`
	Enum              []string `json:"enum,omitempty"`
	Required          bool     `json:"required"`
	Description       string   `json:"description,omitempty"`
	Positional        int      `json:"positional,omitempty"`
	BodyPath          string   `json:"body_path,omitempty"`
	Query             string   `json:"query,omitempty"`
	Style             string   `json:"style,omitempty"`
	Explode           *bool    `json:"explode,omitempty"`
	Repeat            bool     `json:"repeatable,omitempty"`
	Variadic          bool     `json:"variadic,omitempty"`
	IDPrefix          string   `json:"id_prefix,omitempty"`
	AcceptsHistoryRef bool     `json:"accepts_history_ref,omitempty"`
}

type Command struct {
	Section             string                `json:"section"`
	Audience            string                `json:"audience"`
	Kind                string                `json:"kind,omitempty"`
	Group               bool                  `json:"-"`
	Name                string                `json:"name"`
	CanonicalName       string                `json:"canonical_name"`
	Path                []string              `json:"command"`
	CanonicalPath       []string              `json:"canonical_command,omitempty"`
	Aliases             []string              `json:"aliases,omitempty"`
	AliasPaths          [][]string            `json:"alias_commands,omitempty"`
	Description         string                `json:"description"`
	Examples            []string              `json:"examples"`
	Method              string                `json:"method,omitempty"`
	APIPath             string                `json:"api_path,omitempty"`
	Security            []map[string][]string `json:"security,omitempty"`
	ResponseMediaType   string                `json:"response_media_type,omitempty"`
	OperationID         string                `json:"operation_id,omitempty"`
	InputSchema         string                `json:"input_schema,omitempty"`
	OutputSchema        string                `json:"output_schema,omitempty"`
	Arguments           []Arg                 `json:"arguments"`
	Mutation            bool                  `json:"mutation"`
	Sensitive           bool                  `json:"sensitive"`
	Destructive         bool                  `json:"destructive"`
	Stream              bool                  `json:"stream"`
	Get                 bool                  `json:"get"`
	Local               bool                  `json:"local"`
	AuthRequired        bool                  `json:"auth_required"`
	Supports            Supports              `json:"supports"`
	Render              string                `json:"-"`
	MCPHidden           bool                  `json:"-"`
	EnvironmentAffinity EnvironmentAffinity   `json:"environment_affinity,omitempty"`
}

type EnvironmentAffinity string

const (
	EnvironmentAffinityMerchant EnvironmentAffinity = "merchant_environment"
	EnvironmentAffinityAccount  EnvironmentAffinity = "account_control_plane"
	EnvironmentAffinityNeutral  EnvironmentAffinity = "environment_neutral"
)

type Supports struct {
	DryRunClient   bool `json:"dry_run_client"`
	IdempotencyKey bool `json:"idempotency_key"`
	JSONOutput     bool `json:"json_output"`
	NoInput        bool `json:"no_input"`
	Pagination     bool `json:"pagination,omitempty"`
	WaitFor        bool `json:"wait_for,omitempty"`
	NDJSON         bool `json:"ndjson,omitempty"`
	Open           bool `json:"open,omitempty"`
	Field          bool `json:"field"`
	Select         bool `json:"select"`
	JQ             bool `json:"jq"`
}

type Options struct {
	Output          string
	Quiet           bool
	Debug           bool
	NoInput         bool
	Live            bool
	Merchant        string
	ContextID       string
	CredentialStore string
	Profile         string
	Color           string
	Confirm         bool
	DryRun          string
	IdempotencyKey  string
	JQ              string
	Select          []string
	Field           string
	Expand          []string
	Open            bool
	Limit           int
	PageSize        int
	PageToken       string
	All             bool
	Timeout         time.Duration
	MaxEvents       int
	For             time.Duration
	WaitFor         []string
	Progress        string
	Input           string
	Paginate        bool
	Preview         bool
	Clear           bool
	Stdin           bool
	Fix             bool
	Raw             map[string][]string
	Positionals     []string

	booleanFlags          map[string]bool
	allowMissingProfile   bool
	streamCommand         bool
	outputFromEnvironment bool
	outputLimit           int // MCP transform output budget; zero leaves CLI output unbounded.
}

type App struct {
	mcpAuth                   *mcpAuthState
	mcpSkipScopeCheck         bool // MCP forwards scope failures from the API unchanged.
	mcpErrorObserver          func(*CLIError)
	mcpGate                   *mcpCallGate
	credentialStore           string
	Info                      BuildInfo
	Stdout                    io.Writer
	Stderr                    io.Writer
	Stdin                     io.Reader
	IsTTY                     func() bool
	Now                       func() time.Time
	OpenBrowser               func(string) error
	HTTPClient                *http.Client
	ForwardHTTPClient         *http.Client
	Context                   context.Context
	BaseURL                   string
	ConfigDir                 string
	WorkingDir                string
	Registry                  *Registry
	LoadCredential            func(string) (string, error)
	StoreCredential           func(string, string) error
	DeleteCredential          func(string) error
	upgradeReleaseLatestURL   string
	upgradeReleaseAPIURL      string
	upgradeReleaseDownloadURL string
	executablePath            func() (string, error)
	runCommand                func(context.Context, string, ...string) ([]byte, error)
	runtimeGOOS               string
	runtimeGOARCH             string
}

type CLIError struct {
	apiError       map[string]any // Immutable decoded API error, separate from terminal diagnostics.
	idempotencyKey string
	requestSent    bool // The request reached, or may have reached, the transport.
	retryable      *bool
	RetryAfterMs   *int64
	HTTPStatus     int
	ExitCode       int
	Type           string
	ErrorSource    string
	Origin         string
	Code           string
	Message        string
	Param          string
	Details        any
	RequestID      string
	Cause          error
}

func (e *CLIError) Error() string { return e.Message }

func cliError(exit int, typ, code, message string) *CLIError {
	return &CLIError{ExitCode: exit, Type: typ, Code: code, Message: message}
}

// Decoded API errors must not share mutable maps or arrays with terminal
// diagnostics, which add idempotency and retry advice of their own.
func cloneErrorJSON(value any) any {
	switch raw := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(raw))
		for key, item := range raw {
			out[key] = cloneErrorJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(raw))
		for index, item := range raw {
			out[index] = cloneErrorJSON(item)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(raw))
		for index, item := range raw {
			out[index] = cloneErrorJSON(item).(map[string]any)
		}
		return out
	case []string:
		return append([]string(nil), raw...)
	default:
		return value
	}
}
