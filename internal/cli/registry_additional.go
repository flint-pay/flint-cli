package cli

import "strings"

func additionalCommands(page []Arg, input Arg) []*Command {
	with := func(base []Arg, extra ...Arg) []Arg { out := append([]Arg(nil), base...); return append(out, extra...) }
	return []*Command{
		apiCommand("payment-links.create", "Create a payment link", "POST", "/v1/payment-links", "createPaymentLink", "CreatePaymentLinkRequest", "PaymentLinkResponse", []Arg{flag("name", "string", "name", "", "Payment link name", true), flag("item_name", "string", "line_items.0.name", "", "Item name", false), flag("amount", "integer", "line_items.0.unit_price_money.amount", "", "Item price in minor units", false), flag("currency", "string", "line_items.0.unit_price_money.currency", "", "ISO currency code", false), input}, true, false, false, false, "flint payment-links create --name T-shirt --item-name T-shirt --amount 2500 --currency USD"),
		apiCommand("payment-links.list", "List payment links", "GET", "/v1/payment-links", "listPaymentLinks", "", "PaymentLinkListResponse", page, false, false, false, false, "flint payment-links list --page-size 25"),
		apiCommand("invoices.create", "Create an invoice", "POST", "/v1/invoices", "createInvoice", "CreateInvoiceRequest", "InvoiceResponse", []Arg{idFlag("order", "ord_", "order_id", "", "Order ID", false), flag("recipient_email", "string", "recipient_email", "", "Invoice recipient email", false), input}, true, false, false, false, "flint invoices create --order ord_123 --recipient-email jane@example.com"),
		apiCommand("invoices.issue", "Issue an invoice", "POST", "/v1/invoices/{invoice_id}/issue", "issueInvoice", "IssueInvoiceRequest", "IssueInvoiceResponse", []Arg{pos("invoice_id", "inv_", "Flint invoice ID"), flag("delivery_mode", "string", "delivery_mode", "", "Invoice delivery mode", false), input}, true, true, false, false, "flint invoices issue inv_123 --delivery-mode email"),
		apiCommand("invoices.get", "Get an invoice", "GET", "/v1/invoices/{invoice_id}", "getInvoice", "", "InvoiceResponse", []Arg{pos("invoice_id", "inv_", "Flint invoice ID")}, false, false, false, true, "flint invoices get inv_123"),
		apiCommand("invoices.list", "List invoices", "GET", "/v1/invoices", "listInvoices", "", "InvoiceListResponse", with(page, idFlag("customer", "cus_", "", "customer_id", "Filter by customer ID", false), flag("status", "string", "", "status", "Filter by invoice status", false)), false, false, false, false, "flint invoices list --customer cus_123"),
		apiCommand("webhook-endpoints.create", "Create a webhook endpoint", "POST", "/v1/webhook-endpoints", "createWebhookEndpoint", "CreateWebhookEndpointRequest", "WebhookEndpointResponse", []Arg{flag("url", "string", "url", "", "HTTPS delivery URL", true), {Name: "enabled_event", Flag: "--enabled-event", Type: "string", BodyPath: "enabled_events", Repeat: true, Description: "Event type to deliver"}, input}, true, true, false, false, "flint webhook-endpoints create --url https://example.com/webhooks/flint --enabled-event payment_intent.succeeded"),
		apiCommand("webhook-endpoints.get", "Get a webhook endpoint", "GET", "/v1/webhook-endpoints/{webhook_endpoint_id}", "getWebhookEndpoint", "", "WebhookEndpointResponse", []Arg{pos("webhook_endpoint_id", "whep_", "Flint webhook endpoint ID")}, false, false, false, true, "flint webhook-endpoints get whep_123"),
		apiCommand("webhook-endpoints.list", "List webhook endpoints", "GET", "/v1/webhook-endpoints", "listWebhookEndpoints", "", "WebhookEndpointListResponse", page, false, false, false, false, "flint webhook-endpoints list --page-size 25"),
		apiCommand("webhook-endpoints.update", "Update a webhook endpoint", "PATCH", "/v1/webhook-endpoints/{webhook_endpoint_id}", "updateWebhookEndpoint", "UpdateWebhookEndpointRequest", "WebhookEndpointResponse", []Arg{pos("webhook_endpoint_id", "whep_", "Flint webhook endpoint ID"), flag("url", "string", "url", "", "HTTPS delivery URL", false), {Name: "enabled_event", Flag: "--enabled-event", Type: "string", BodyPath: "enabled_events", Repeat: true}, input}, true, true, false, false, "flint webhook-endpoints update whep_123 --input endpoint.json"),
		apiCommand("webhook-endpoints.test", "Create a synthetic test event for one endpoint", "POST", "/v1/webhook-endpoints/{webhook_endpoint_id}/test-events", "createWebhookTestEvent", "CreateWebhookTestEventRequest", "WebhookDeliveryActionResponse", []Arg{pos("webhook_endpoint_id", "whep_", "Flint webhook endpoint ID"), flag("event_type", "string", "event_type", "", "Webhook event type", true), input}, true, true, false, false, "flint webhook-endpoints test whep_123 --event-type payment_intent.succeeded"),
		apiCommand("webhook-endpoints.rotate-secret", "Rotate a webhook endpoint secret", "POST", "/v1/webhook-endpoints/{webhook_endpoint_id}/rotate-secret", "rotateWebhookSecret", "", "WebhookSecretRotationResponse", []Arg{pos("webhook_endpoint_id", "whep_", "Flint webhook endpoint ID")}, true, true, true, false, "flint webhook-endpoints rotate-secret whep_123 --confirm"),
		apiCommand("webhook-endpoints.delete", "Delete a webhook endpoint", "DELETE", "/v1/webhook-endpoints/{webhook_endpoint_id}", "deleteWebhookEndpoint", "", "ActionResponse", []Arg{pos("webhook_endpoint_id", "whep_", "Flint webhook endpoint ID")}, true, true, true, false, "flint webhook-endpoints delete whep_123 --confirm"),
		apiCommand("webhook-events.get", "Get a webhook event", "GET", "/v1/webhook-events/{webhook_event_id}", "getWebhookEvent", "", "WebhookEventResponse", []Arg{pos("webhook_event_id", "whev_", "Flint webhook event ID")}, false, false, false, true, "flint webhook-events get whev_123"),
		apiCommand("webhook-events.list", "List webhook events", "GET", "/v1/webhook-events", "listWebhookEvents", "", "WebhookEventListResponse", with(page, idFlag("webhook_endpoint_id", "whep_", "", "webhook_endpoint_id", "Filter by webhook endpoint ID", false), flag("delivery_status", "string", "", "delivery_status", "Filter by child delivery status", false), flag("event_type", "string", "", "event_type", "Filter by event type", false), flag("created_after", "time", "", "created_after", "Creation lower bound", false), flag("created_before", "time", "", "created_before", "Creation upper bound", false)), false, false, false, false, "flint webhook-events list --event-type payment_intent.succeeded", "flint webhook-events list --webhook-endpoint-id whep_123 --delivery-status failed"),
		apiCommand("webhook-events.deliveries", "List deliveries for a webhook event", "GET", "/v1/webhook-events/{webhook_event_id}/deliveries", "listWebhookDeliveries", "", "WebhookDeliveryListResponse", with([]Arg{pos("webhook_event_id", "whev_", "Flint webhook event ID")}, page...), false, false, false, false, "flint webhook-events deliveries whev_123"),
		apiCommand("webhook-deliveries.get", "Get a webhook delivery", "GET", "/v1/webhook-deliveries/{webhook_delivery_id}", "getWebhookDelivery", "", "WebhookDeliveryResponse", []Arg{pos("webhook_delivery_id", "wdel_", "Flint webhook delivery ID")}, false, false, false, true, "flint webhook-deliveries get wdel_123"),
		apiCommand("webhook-deliveries.attempts", "List attempts for a webhook delivery", "GET", "/v1/webhook-deliveries/{webhook_delivery_id}/attempts", "listWebhookDeliveryAttempts", "", "WebhookDeliveryAttemptListResponse", with([]Arg{pos("webhook_delivery_id", "wdel_", "Flint webhook delivery ID")}, page...), false, false, false, false, "flint webhook-deliveries attempts wdel_123"),
		apiCommand("webhook-deliveries.resend", "Resend a webhook delivery", "POST", "/v1/webhook-deliveries/{webhook_delivery_id}/resend", "resendWebhookDelivery", "ResendWebhookDeliveryRequest", "WebhookDeliveryActionResponse", []Arg{pos("webhook_delivery_id", "wdel_", "Flint webhook delivery ID"), input}, true, true, false, false, "flint webhook-deliveries resend wdel_123"),
		{Name: "listen", CanonicalName: "listen", Path: pathWithFlint("listen"), Description: "Forward Flint webhook events to a local endpoint", Method: "GET", APIPath: "/v1/webhook-events/stream", OperationID: "streamWebhookEvents", Arguments: []Arg{flag("forward_to", "string", "", "", "Local HTTP or HTTPS webhook URL", true), flag("event_type", "string", "", "event_type", "Filter by event type", false), idFlag("cursor", "whev_", "", "after_event_id", "Resume after a webhook event ID", false)}, Stream: true, AuthRequired: true, Examples: ex("flint listen --forward-to http://localhost:8080/webhooks/flint --event-type payment_intent.succeeded", "flint listen --forward-to http://127.0.0.1:8080/webhooks/flint --for 30s --output ndjson")},
		apiCommand("request-logs.get", "Get redacted request log detail", "GET", "/v1/developer/request-logs/{api_request_log_id}", "getCurrentAPIKeyRequestLog", "", "APIRequestLogDetailResponse", []Arg{pos("api_request_log_id", "rlog_", "Flint API request log ID")}, false, false, false, true, "flint request-logs get rlog_123"),
		apiCommand("request-logs.list", "List request logs for the current API key", "GET", "/v1/developer/request-logs", "listCurrentAPIKeyRequestLogs", "", "APIRequestLogListResponse", with(page, flag("status_bucket", "string", "", "status_bucket", "Filter by status bucket", false), flag("request_id", "string", "", "request_id", "Filter by request ID", false), flag("created_after", "time", "", "created_after", "Creation lower bound", false), flag("created_before", "time", "", "created_before", "Creation upper bound", false)), false, false, false, false, "flint request-logs list --status-bucket server_error"),
		apiCommand("timeline", "Get the timeline for any supported public resource", "GET", "/v1/developer/resource-timelines/{resource_id}", "getResourceTimeline", "", "ResourceTimelineResponse", with([]Arg{{Name: "resource_id", Type: "string", Required: true, Positional: 1, AcceptsHistoryRef: true, Description: "Any supported Flint resource ID"}}, page...), false, false, false, true, "flint timeline pi_123", "flint timeline @last.pi"),
		apiCommand("sandboxes.list", "List developer sandboxes using a live management key", "GET", "/v1/developer/sandboxes", "listDeveloperSandboxes", "", "SandboxListResponse", page, false, false, false, false, "flint sandboxes list --live"),
		apiCommand("sandboxes.create", "Create a developer sandbox using a live management key", "POST", "/v1/developer/sandboxes", "createDeveloperSandbox", "CreateSandboxRequest", "SandboxWithAPIKeyResponse", []Arg{flag("name", "string", "name", "", "Sandbox name", true), input}, true, true, false, false, "flint sandboxes create --name qa-scenarios --live"),
		apiCommand("sandboxes.reset", "Reset a developer sandbox using a live management key", "POST", "/v1/developer/sandboxes/{sandbox_id}/reset", "resetDeveloperSandbox", "", "SandboxResponse", []Arg{pos("sandbox_id", "test_", "Flint sandbox ID")}, true, true, true, false, "flint sandboxes reset test_123 --live --confirm"),
		apiCommand("sandboxes.test-key", "Issue a sandbox-bound test key using a live management key", "POST", "/v1/developer/sandboxes/{sandbox_id}/test-key", "issueDeveloperSandboxTestKey", "IssueSandboxAPIKeyRequest", "CreateAPIKeyResponse", []Arg{pos("sandbox_id", "test_", "Flint sandbox ID"), input}, true, true, false, false, "flint sandboxes test-key test_123 --input key.json --live"),
		apiCommand("api-keys.create", "Create a scoped API key", "POST", "/v1/api-keys", "createAPIKey", "CreateAPIKeyRequest", "CreateAPIKeyResponse", []Arg{flag("name", "string", "name", "", "API key name", true), {Name: "scope", Flag: "--scope", Type: "string", BodyPath: "scopes", Repeat: true, Required: true, Description: "Scope to grant"}, input}, true, true, false, false, "flint api-keys create --name local-dev --scope payments.payment_intents.read"),
		apiCommand("api-keys.list", "List API keys", "GET", "/v1/api-keys", "listAPIKeys", "", "APIKeyListResponse", page, false, false, false, false, "flint api-keys list"),
		apiCommand("api-keys.revoke", "Revoke an API key", "POST", "/v1/api-keys/{api_key_id}/revoke", "revokeAPIKey", "", "APIKeyResponse", []Arg{pos("api_key_id", "key_", "Flint API key ID")}, true, true, true, false, "flint api-keys revoke key_123 --preview", "flint api-keys revoke key_123 --confirm"),
		{Name: "api", CanonicalName: "api", Path: pathWithFlint("api"), Description: "Call a documented Flint public API route", AuthRequired: true, Arguments: []Arg{{Name: "method", Type: "string", Required: true, Positional: 1}, {Name: "path", Type: "string", Required: true, Positional: 2}, input}, Examples: ex("flint api get /v1/payment-intents/pi_123", "flint api post /v1/payment-intents --input payment-intent.json")},
		{Name: "schema.commands", CanonicalName: "schema.commands", Path: pathWithFlint("schema", "commands"), Description: "List command schemas", Local: true, Examples: ex("flint schema commands --output json")},
		{Name: "schema.command", CanonicalName: "schema.command", Path: pathWithFlint("schema", "command"), Description: "Describe one command", Local: true, Arguments: []Arg{{Name: "command", Type: "string", Required: true, Positional: 1}}, Examples: ex("flint schema command payment-intents.create --output json")},
		{Name: "schema.input", CanonicalName: "schema.input", Path: pathWithFlint("schema", "input"), Description: "Print a command input JSON Schema", Local: true, Arguments: []Arg{{Name: "command", Type: "string", Required: true, Positional: 1}}, Examples: ex("flint schema input payment-intents.create --output json")},
		{Name: "schema.output", CanonicalName: "schema.output", Path: pathWithFlint("schema", "output"), Description: "Print a command output JSON Schema", Local: true, Arguments: []Arg{{Name: "command", Type: "string", Required: true, Positional: 1}}, Examples: ex("flint schema output payment-intents.create --output json")},
		{Name: "schema.errors", CanonicalName: "schema.errors", Path: pathWithFlint("schema", "errors"), Description: "Print the public error catalog", Local: true, Examples: ex("flint schema errors --output json")},
		{Name: "schema.events", CanonicalName: "schema.events", Path: pathWithFlint("schema", "events"), Description: "Print the public webhook event catalog", Local: true, Examples: ex("flint schema events --output json")},
		{Name: "mcp.serve", CanonicalName: "mcp.serve", Path: pathWithFlint("mcp", "serve"), Description: "Serve canonical Flint commands as MCP tools over stdio", Local: true, Examples: ex("flint mcp serve")},
		{Name: "help", CanonicalName: "help", Path: pathWithFlint("help"), Description: "Show command help or an offline reference topic", Local: true, Arguments: []Arg{{Name: "topic", Type: "string", Positional: 1}}, Examples: ex("flint help test-cards", "flint help exit-codes")},
		{
			Name: "help.search", CanonicalName: "help.search", Path: pathWithFlint("help", "search"),
			Description: "Search Flint Help for community answers and support threads",
			Local:       true,
			Arguments: []Arg{{
				Name: "query", Type: "string", Required: true, Positional: 1, Variadic: true,
				Description: "Words to search for; the rest of the line is the query",
			}},
			Examples: ex(
				"flint help search webhook signature verification",
				"flint help search payout on hold --output json",
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
			Description: "Create a merchant and initial sandbox key through API-first onboarding",
			Local:       true,
			Arguments: []Arg{
				flag("email", "string", "", "", "Email address", false),
				flag("first_name", "string", "", "", "First name", false),
				flag("last_name", "string", "", "", "Last name", false),
				flag("verification_code", "string", "", "", "Email verification code", false),
				flag("country", "string", "", "", "ISO 3166-1 alpha-2 business country", false),
				flag("website_url", "string", "", "", "Public business website URL", false),
				flag("support_email", "string", "", "", "Customer support email", false),
				flag("support_phone", "string", "", "", "Customer support phone", false),
				flag("support_url", "string", "", "", "Customer support URL", false),
				{Name: "requested_capability", Flag: "--requested-capability", Type: "string", Repeat: true, Description: "Requested onboarding capability; repeat for multiple capabilities"},
			},
			Examples: ex(
				"flint signup",
				"flint signup --email dev@example.com --first-name Ada --last-name Lovelace --verification-code 482193 --no-input --output json",
			),
		},
	}
}

func withEnvironmentAffinity(command *Command, affinity EnvironmentAffinity) *Command {
	command.EnvironmentAffinity = affinity
	return command
}
