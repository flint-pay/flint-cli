package cli

import "strings"

func additionalCommands(page []Arg, input Arg) []*Command {
	return []*Command{

		{Name: "listen", CanonicalName: "listen", Path: pathWithFlint("listen"), Description: "Forward Flint webhook events to a local endpoint using the local HMAC listen secret, regardless of endpoint signing methods. Delivery is at-least-once and uses the caller's API version. To test JWT, use a tunnel and a registered sandbox endpoint", Method: "GET", APIPath: "/v1/webhook-events/stream", OperationID: "streamWebhookEvents", Arguments: []Arg{flag("forward_to", "string", "", "", "Local HTTP or HTTPS webhook URL on localhost, a loopback IP, or host.docker.internal; forwarded requests use local HMAC signing; omit to print events instead", false), flag("event_type", "string", "", "event_type", "Event types to receive, repeatable or comma-separated; omit for all. Newer types pass through to the server. See flint help events", false), idFlag("webhook_endpoint_id", "whep_", "", "", "Flint-Webhook-Endpoint-Id sent to the local handler; defaults to whep_cli_listen", false), idFlag("cursor", "whev_", "", "after_event_id", "Resume after this webhook event ID, such as a checkpoint cursor", false), flag("webhook_secret", "string", "", "", "Local HMAC signing secret (whsec_...) for forwarded events; defaults to FLINT_WEBHOOK_SECRET, then a saved secret reused across runs", false), {Name: "print_secret", Flag: "--print-secret", Type: "boolean", Description: "Print the signing secret and exit without connecting"}}, Stream: true, AuthRequired: true, Examples: ex("flint listen --forward-to http://localhost:8080/webhooks/flint --event-type payment_intent.succeeded", "flint listen --forward-to http://127.0.0.1:8080/webhooks/flint --for 30s --output ndjson", "flint listen --print-secret", "flint listen --event-type customer.created --max-events 1 --for 2m --output json")},
		{Name: "api", CanonicalName: "api", Path: pathWithFlint("api"), Description: "Call a Flint public API route", AuthRequired: true, Arguments: []Arg{{Name: "method", Type: "string", Required: true, Positional: 1, Description: "HTTP method: get, post, put, patch, or delete"}, {Name: "path", Type: "string", Required: true, Positional: 2, Description: "Canonical relative /v1/ path with IDs filled in; unknown routes warn and writes run once"}, input}, Examples: ex("flint api get /v1/payment-intents/pi_123", "flint api post /v1/payment-intents --input payment-intent.json")},
		{Name: "schema.commands", CanonicalName: "schema.commands", Path: pathWithFlint("schema", "commands"), Description: "List command schemas, optionally only those under a command group", Local: true, Arguments: []Arg{{Name: "prefix", Type: "string", Positional: 1, Variadic: true, Description: "Command group, dotted or space-separated, such as customers or customers addresses"}}, Examples: ex("flint schema commands customers --output json", "flint schema commands --output json")},
		{Name: "schema.command", CanonicalName: "schema.command", Path: pathWithFlint("schema", "command"), Description: "Describe one command from the embedded API snapshot, without a request", Local: true, Arguments: []Arg{{Name: "command", Type: "string", Required: true, Positional: 1, Variadic: true, Description: "Command name, dotted or space-separated, such as customers.create or customers create"}}, Examples: ex("flint schema command payment-intents.create --output json")},
		{Name: "schema.input", CanonicalName: "schema.input", Path: pathWithFlint("schema", "input"), Description: "Print the JSON Schema of a command's --input body, or its documented body with --example", Local: true, Arguments: []Arg{{Name: "command", Type: "string", Required: true, Positional: 1, Variadic: true, Description: "Command name, dotted or space-separated, such as customers.create or customers create"}, flag("example", "boolean", "", "", "Print a documented request body as JSON, ready for --input; omit to print the schema", false)}, Examples: ex("flint schema input payment-intents.create --output json", "flint schema input customers addresses create --output json", "flint schema input refunds.create --example > refund.json")},
		{Name: "schema.output", CanonicalName: "schema.output", Path: pathWithFlint("schema", "output"), Description: "Print a command output JSON Schema", Local: true, Arguments: []Arg{{Name: "command", Type: "string", Required: true, Positional: 1, Variadic: true, Description: "Command name, dotted or space-separated, such as customers.create or customers create"}}, Examples: ex("flint schema output payment-intents.create --output json")},
		{Name: "schema.errors", CanonicalName: "schema.errors", Path: pathWithFlint("schema", "errors"), Description: "List CLI and public API error types and exit codes", Local: true, Examples: ex("flint schema errors --output json")},
		{Name: "schema.events", CanonicalName: "schema.events", Path: pathWithFlint("schema", "events"), Description: "List webhook event types, or print payload schemas for a type or prefix", Local: true, Arguments: []Arg{{Name: "event_type", Type: "string", Positional: 1, Description: "Exact event type or prefix, such as payment_intent.succeeded or payment_intent"}}, Examples: ex("flint schema events --output json", "flint schema events payment_intent --output json", "flint schema events payment_intent.succeeded --output json")},
		{
			Name: "mcp.serve", CanonicalName: "mcp.serve", Path: pathWithFlint("mcp", "serve"),
			Description: "Discover and run Flint operations with five MCP tools over stdio (UTF-8 JSON, string or safe integer request IDs). Uses the CLI credential: FLINT_API_KEY, the FLINT_PROFILE (or --profile) profile, or a saved flint login",
			Local:       true,
			Arguments: []Arg{
				{Name: "read_only", Flag: "--read-only", Type: "boolean", Description: "Expose read operations only and refuse every write"},
				{Name: "tools", Flag: "--tools", Type: "string", Repeat: true, Description: "Opt into typed tools for operation IDs or dotted resource prefixes, comma-separated or repeated; all selects every available operation. Names use underscores for dots and hyphens. Search, describe, and help use the same scope; no generic read/write tools"},
				{Name: "allow_live", Flag: "--allow-live", Type: "boolean", Description: "Let tools act on live (production) credentials and contexts; each live call must pass live: true"},
				{Name: "allow_destructive", Flag: "--allow-destructive", Type: "boolean", Description: "Let agents confirm gated live operations with confirm: true"},
			},
			Examples: ex(
				"flint mcp serve",
				"flint mcp serve --tools all",
				"flint mcp serve --tools invoices,subscriptions",
				"flint mcp serve --read-only",
				"flint mcp serve --profile staging --tools customers",
				"flint mcp serve --allow-live",
				"flint mcp serve --allow-live --allow-destructive",
			),
		},
		{Name: "help", CanonicalName: "help", Path: pathWithFlint("help"), Description: "Show command help or an offline reference topic", Local: true, Arguments: []Arg{{Name: "topic", Type: "string", Positional: 1, Variadic: true, Description: "A command such as customers.create, or a topic: " + strings.Join(helpTopicNames, ", ")}}, Examples: ex("flint help test-cards", "flint help exit-codes")},
		{
			Name: "help.search", CanonicalName: "help.search", Path: pathWithFlint("help", "search"),
			Description: "Find commands, webhook events, and reference topics offline",
			Local:       true,
			Arguments: []Arg{{
				Name: "query", Type: "string", Required: true, Positional: 1, Variadic: true,
				Description: "Words to search for; the rest of the line is the query",
			}},
			Examples: ex(
				"flint help search forward webhooks to localhost",
				"flint help search refund an order --output json",
			),
		},
		{
			Name: "support.open", CanonicalName: "support.open", Path: pathWithFlint("support", "open"),
			Description: "Start a Flint Help thread with your terminal context prefilled. Opens the composer in your browser; posting happens there.",
			Local:       true,
			// An agent cannot see or use a browser window, so this stays out of MCP.
			MCPHidden: true,
			Arguments: []Arg{
				flag("title", "string", "", "", "Thread title to prefill", false),
				flag("body", "string", "", "", "Thread body to prefill", false),
				{Name: "ai_agent", Flag: "--ai-agent", Type: "boolean", Description: "Identify the reporter as an AI agent (self-reported; defaults to false)"},
				flag("request_id", "string", "", "", "Flint request ID from the failing call", false),
				flag("resource", "string", "", "", "Flint resource ID the question is about", false),
				flag("area", "string", "", "", "Product area: "+strings.Join(supportProductAreas, ", "), false),
				{Name: "private", Flag: "--private", Type: "boolean", Description: "Ask for a private thread instead of a public one"},
				{Name: "no_open", Flag: "--no-open", Type: "boolean", Description: "Print the composer link without opening a browser"},
			},
			Examples: ex(
				"flint support open --request-id req_123",
				"flint support open --title Webhook-failure --body Endpoint-returned-500 --area webhooks --private",
				"flint support open --area webhooks --resource whep_123 --private",
			),
		},
		{
			Name: "signup", CanonicalName: "signup", Path: pathWithFlint("signup"),
			Description: "Create an account and sandbox key. Without a code, email a verification code and save progress; finish with --verification-code. MCP and --no-input use two calls.",
			Local:       true,
			Arguments: []Arg{
				flag("email", "string", "", "", "Email address", false),
				flag("first_name", "string", "", "", "First name", false),
				flag("last_name", "string", "", "", "Last name", false),
				flag("verification_code", "string", "", "", "Email verification code; resumes signup saved in this profile", false),
				flag("country", "string", "", "", "ISO 3166-1 alpha-2 business country", false),
				flag("website_url", "string", "", "", "Public business website URL", false),
				flag("support_email", "string", "", "", "Customer support email", false),
				flag("support_phone", "string", "", "", "Customer support phone", false),
				flag("support_url", "string", "", "", "Customer support URL", false),
				{Name: "requested_capability", Flag: "--requested-capability", Type: "string", Repeat: true, Description: "Requested onboarding capability; repeat for multiple capabilities"},
			},
			Examples: ex(
				"flint signup",
				"flint signup --email dev@example.com --first-name Ada --last-name Lovelace --no-input --output json",
				"flint signup --verification-code 482193 --no-input --output json",
			),
		},
	}
}

// API key commands share these flags. --scope without --access selects
// restricted access, matching the Dashboard (see inferAPIKeyAccess).
var (
	apiKeyAccessFlag = flag("access", "string", "access", "", "Key access: full, read_only, or restricted; defaults to restricted with --scope", false)
	apiKeyScopeFlag  = Arg{Name: "scope", Flag: "--scope", Type: "string", BodyPath: "scopes", Repeat: true, Description: "Scope to grant; implies --access restricted and is not allowed with full or read_only"}
)

func withEnvironmentAffinity(command *Command, affinity EnvironmentAffinity) *Command {
	command.EnvironmentAffinity = affinity
	return command
}
