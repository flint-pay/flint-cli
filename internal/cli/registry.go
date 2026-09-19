package cli

import (
	"sort"
	"strings"
)

type Registry struct {
	Commands []*Command
	byName   map[string]*Command
	byPath   map[string]*Command
}

func (r *Registry) commandsUnder(prefix []string) []*Command {
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

func NewRegistry() *Registry {
	r := &Registry{byName: map[string]*Command{}, byPath: map[string]*Command{}}
	for _, c := range commandDefinitions() {
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
		if strings.HasPrefix(c.CanonicalName, "organizations.") ||
			strings.HasPrefix(c.CanonicalName, "api-keys.") ||
			strings.HasPrefix(c.CanonicalName, "sandboxes.") ||
			c.CanonicalName == "auth.status" {
			c.EnvironmentAffinity = EnvironmentAffinityAccount
		}
		if operation, ok := openAPIOperationByID(c.OperationID); ok && operation.FlintRouteClass != "" {
			// Safety classification belongs to the public API contract. Keeping the
			// CLI confirmation gate derived from it prevents a new sensitive route
			// from silently behaving like an ordinary write.
			c.Sensitive = operation.FlintRouteClass == "sensitive_write" || operation.FlintRouteClass == "external_provider_action"
		}
		c.Supports.JSONOutput = true
		c.Supports.NoInput = true
		transforms := !c.Stream && c.CanonicalName != "mcp.serve"
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
		for _, argument := range c.Arguments {
			if argument.Name == "page_size" {
				c.Supports.Pagination = true
			}
		}
		if c.Stream {
			c.Supports.NDJSON = true
		}
		switch c.CanonicalName {
		case "checkout-sessions.create":
			c.Render = "checkout"
			c.Supports.Open = true
		case "request-logs.get", "request-logs.list":
			c.Render = "request_log"
		case "help.search":
			c.Render = "help_search"
		case "support.open":
			c.Render = "support_open"
		}
		applyPublicOperationMetadata(c)
		r.add(c)
	}
	sort.Slice(r.Commands, func(i, j int) bool { return r.Commands[i].Name < r.Commands[j].Name })
	return r
}

func (r *Registry) add(c *Command) {
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

func (r *Registry) ByName(name string) (*Command, bool) { c, ok := r.byName[name]; return c, ok }
func (r *Registry) ByPath(path []string) (*Command, bool) {
	c, ok := r.byPath[strings.Join(path, " ")]
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
	page := []Arg{flag("page_size", "integer", "", "page_size", "Number of resources per page", false), flag("page_token", "string", "", "page_token", "Pagination token", false)}
	with := func(base []Arg, extra ...Arg) []Arg { out := append([]Arg(nil), base...); return append(out, extra...) }
	input := flag("input", "string", "", "", "JSON request body file, or - for stdin", false)
	commands := []*Command{
		{Name: "init", Path: pathWithFlint("init"), Description: "Initialize Flint CLI context", Local: true, Examples: ex("flint init")},
		alias(&Command{Name: "auth.login", Path: pathWithFlint("auth", "login"), Description: "Sign in through the Flint website (recommended for local development)", Local: true, MCPHidden: true, Arguments: []Arg{{Name: "new_session", Flag: "--new-session", Type: "boolean", Description: "Replace the saved session after browser approval"}, {Name: "no_open", Flag: "--no-open", Type: "boolean", Description: "Print the sign-in link without opening a browser"}}, Examples: ex("flint login", "flint auth login", "flint auth login --no-open", "flint auth login --live --profile production")}, "login"),
		alias(&Command{Name: "auth.reauth", Path: pathWithFlint("auth", "reauth"), Description: "Change the current session's authorized contexts in the browser", Local: true, MCPHidden: true, Arguments: []Arg{{Name: "no_open", Flag: "--no-open", Type: "boolean"}}, Examples: ex("flint reauth")}, "reauth"),
		{Name: "context.list", Path: pathWithFlint("context", "list"), Description: "List authorized contexts and the active selection", Local: true, MCPHidden: true, Examples: ex("flint context list")},
		{Name: "context.switch", Path: pathWithFlint("context", "switch"), Description: "Choose the active merchant and environment", Local: true, MCPHidden: true, Arguments: []Arg{{Name: "context_id", Type: "string", Positional: 1}}, Examples: ex("flint context switch", "flint context switch ctx_123", "flint context switch ctx_live --live")},
		{Name: "auth.import", Path: pathWithFlint("auth", "import"), Description: "Manually import and validate a Flint API key", Local: true, MCPHidden: true, Arguments: []Arg{{Name: "stdin", Flag: "--stdin", Type: "boolean", Description: "Read the key from stdin"}}, Examples: ex("flint auth import --stdin < token.txt")},
		alias(apiCommand("auth.status", "Show the active credential context", "GET", "/v1/developer/auth-context", "getDeveloperAuthContext", "", "DeveloperAuthContextResponse", nil, false, false, false, true, "flint auth status --output json"), "whoami"),
		alias(&Command{Name: "auth.logout", Path: pathWithFlint("auth", "logout"), Description: "Revoke the active OAuth session and remove the profile credential", Local: true, Destructive: true, Examples: ex("flint auth logout --confirm")}, "logout"),
		{Name: "config.get", Path: pathWithFlint("config", "get"), Description: "Print resolved Flint CLI configuration", Local: true, Examples: ex("flint config get --output json")},
		{Name: "config.set", Path: pathWithFlint("config", "set"), Description: "Set profile or merchant guard configuration", Local: true, Arguments: []Arg{{Name: "key", Type: "string", Required: true, Positional: 1}, {Name: "value", Type: "string", Required: true, Positional: 2}}, Examples: ex("flint config set profile default", "flint config set merchant mer_123")},
		{Name: "config.validate", Path: pathWithFlint("config", "validate"), Description: "Validate global and project Flint configuration", Local: true, Examples: ex("flint config validate --output json")},
		{Name: "doctor", Path: pathWithFlint("doctor"), Description: "Check credential, connectivity, scopes, and API compatibility", Local: true, Arguments: []Arg{{Name: "fix", Flag: "--fix", Type: "boolean"}}, Examples: ex("flint doctor")},
		{Name: "history", Path: pathWithFlint("history"), Description: "List or clear recent Flint resource IDs", Local: true, Arguments: []Arg{{Name: "clear", Flag: "--clear", Type: "boolean"}}, Examples: ex("flint history", "flint history --clear --confirm")},
		alias(&Command{Name: "upgrade", Path: pathWithFlint("upgrade"), Description: "Upgrade Flint CLI to the latest stable release", Local: true, MCPHidden: true, Examples: ex("flint upgrade", "flint update", "flint upgrade --output json")}, "update"),
		{Name: "version", Path: pathWithFlint("version"), Description: "Print CLI and public API schema version metadata", Local: true, Examples: ex("flint version --output json")},
		apiCommand("merchants.get", "Get the credential-bound merchant", "GET", "/v1/merchants/{merchant_id}", "getMerchant", "", "MerchantResponse", []Arg{pos("merchant_id", "mer_", "Flint merchant ID")}, false, false, false, true, "flint merchants get mer_123"),
		apiCommand("merchants.update", "Update the credential-bound merchant", "PATCH", "/v1/merchants/{merchant_id}", "updateMerchant", "UpdateMerchantRequest", "MerchantResponse", []Arg{pos("merchant_id", "mer_", "Flint merchant ID"), input}, true, false, false, false, "flint merchants update mer_123 --input merchant.json"),
		apiCommand("organizations.create", "Create an organization", "POST", "/v1/organizations", "createOrganization", "CreateOrganizationRequest", "OrganizationResponse", []Arg{flag("name", "string", "name", "", "Organization name", true), input}, true, false, false, false, "flint organizations create --name Acme"),
		apiCommand("organizations.list", "List organizations", "GET", "/v1/organizations", "listOrganizations", "", "OrganizationListResponse", page, false, false, false, false, "flint organizations list --page-size 25"),
		apiCommand("organizations.get", "Get an organization", "GET", "/v1/organizations/{organization_id}", "getOrganization", "", "OrganizationResponse", []Arg{pos("organization_id", "org_", "Flint organization ID")}, false, false, false, true, "flint organizations get org_123"),
		apiCommand("organizations.update", "Update an organization", "PATCH", "/v1/organizations/{organization_id}", "updateOrganization", "UpdateOrganizationRequest", "OrganizationResponse", []Arg{pos("organization_id", "org_", "Flint organization ID"), input}, true, false, false, false, "flint organizations update org_123 --input organization.json"),
		apiCommand("customers.create", "Create a customer", "POST", "/v1/customers", "createCustomer", "CreateCustomerRequest", "CustomerResponse", []Arg{flag("email", "string", "email", "", "Customer email", false), flag("name", "string", "name", "", "Customer name", false), input}, true, false, false, false, "flint customers create --email jane@example.com"),
		apiCommand("customers.get", "Get a customer", "GET", "/v1/customers/{customer_id}", "getCustomer", "", "CustomerResponse", []Arg{pos("customer_id", "cus_", "Flint customer ID")}, false, false, false, true, "flint customers get cus_123"),
		apiCommand("customers.list", "List customers", "GET", "/v1/customers", "listCustomers", "", "CustomerListResponse", with(page, flag("email", "string", "", "email", "Filter by customer email", false)), false, false, false, false, "flint customers list --page-size 25"),
		apiCommand("customers.update", "Update a customer", "PATCH", "/v1/customers/{customer_id}", "updateCustomer", "UpdateCustomerRequest", "CustomerResponse", []Arg{pos("customer_id", "cus_", "Flint customer ID"), input}, true, false, false, false, "flint customers update cus_123 --input customer.json"),
		apiCommand("orders.create", "Create an order", "POST", "/v1/orders", "createOrder", "CreateOrderRequest", "OrderResponse", []Arg{input}, true, false, false, false, "flint orders create --input order.json"),
		apiCommand("orders.get", "Get an order", "GET", "/v1/orders/{order_id}", "getOrder", "", "OrderResponse", []Arg{pos("order_id", "ord_", "Flint order ID")}, false, false, false, true, "flint orders get ord_123"),
		apiCommand("orders.list", "List orders", "GET", "/v1/orders", "listOrders", "", "OrderListResponse", with(page, idFlag("customer", "cus_", "", "customer_id", "Filter by customer ID", false), flag("status", "string", "", "status", "Filter by order status", false), flag("created_after", "time", "", "created_after", "Creation lower bound", false), flag("created_before", "time", "", "created_before", "Creation upper bound", false)), false, false, false, false, "flint orders list --page-size 25"),
		apiCommand("orders.pay", "Pay, set up, or resume an order payment", "POST", "/v1/orders/{order_id}/pay", "payOrder", "PayOrderRequest", "PayOrderResponse", []Arg{pos("order_id", "ord_", "Flint order ID"), {Name: "payment_source_token", Flag: "--payment-source-token", Type: "string", Repeat: true, BodyPath: "payment_source_tokens", Description: "Payment intent to sandbox token mapping"}, input}, true, true, false, false, "flint orders pay ord_123 --payment-source-token pi_123=pm_card_visa"),
		apiCommand("orders.close", "Close an order", "POST", "/v1/orders/{order_id}/close", "closeOrder", "CloseOrderRequest", "OrderResponse", []Arg{pos("order_id", "ord_", "Flint order ID"), input}, true, true, true, false, "flint orders close ord_123 --confirm"),
		alias(apiCommand("payment-intents.create", "Create a standalone or order-linked payment intent", "POST", "/v1/payment-intents", "createPaymentIntent", "CreatePaymentIntentRequest", "CreatePaymentIntentResponse", []Arg{flag("amount", "integer", "amount_money.amount", "", "Amount in the smallest currency unit", false), flag("currency", "string", "amount_money.currency", "", "ISO currency code", false), idFlag("customer", "cus_", "customer_id", "", "Customer ID", false), idFlag("order", "ord_", "order_id", "", "Order ID; amount derives from the order balance", false), flag("capture_method", "string", "capture_method", "", "automatic or manual", false), {Name: "payment_option", Flag: "--payment-option", Type: "string", Repeat: true, BodyPath: "payment_options", Description: "Standalone payment option; repeat for multiple options"}, flag("transaction_purpose", "string", "transaction_purpose", "", "goods, services, or other; required for ach_debit", false), input}, true, true, false, false, "flint payment-intents create --amount 2500 --currency USD --payment-option card", "flint payment-intents create --order ord_123"), "payment.create"),
		apiCommand("payment-intents.get", "Get a payment intent", "GET", "/v1/payment-intents/{payment_intent_id}", "getPaymentIntent", "", "GetPaymentIntentResponse", []Arg{pos("payment_intent_id", "pi_", "Flint payment intent ID")}, false, false, false, true, "flint payment-intents get pi_123 --wait-for status=succeeded"),
		apiCommand("payment-intents.list", "List payment intents", "GET", "/v1/payment-intents", "listPaymentIntents", "", "PaymentIntentListResponse", with(page, flag("status", "string", "", "status", "Filter by status", false), idFlag("customer", "cus_", "", "customer_id", "Filter by customer ID", false), flag("created_after", "time", "", "created_after", "Creation lower bound", false), flag("created_before", "time", "", "created_before", "Creation upper bound", false)), false, false, false, false, "flint payment-intents list --status succeeded"),
		apiCommand("payment-intents.confirm", "Confirm a payment intent", "POST", "/v1/payment-intents/{payment_intent_id}/confirm", "confirmPaymentIntent", "ConfirmPaymentIntentRequest", "PaymentIntentResponse", []Arg{pos("payment_intent_id", "pi_", "Flint payment intent ID"), flag("confirmation_token", "string", "confirmation_token", "", "Client-created confirmation token", false), flag("payment_source_token", "string", "payment_source_token", "", "Documented sandbox payment source token", false), idFlag("payment_method", "pm_", "payment_method_id", "", "Saved payment method ID", false), input}, true, true, false, false, "flint payment-intents confirm pi_123 --confirmation-token ctoken_123"),
		apiCommand("payment-intents.capture", "Capture a manual-capture payment intent", "POST", "/v1/payment-intents/{payment_intent_id}/capture", "capturePaymentIntent", "CapturePaymentIntentRequest", "PaymentIntentResponse", []Arg{pos("payment_intent_id", "pi_", "Flint payment intent ID"), flag("amount", "integer", "amount_money.amount", "", "Optional capture amount", false), flag("currency", "string", "amount_money.currency", "", "Currency for capture amount", false), input}, true, true, false, false, "flint payment-intents capture pi_123"),
		apiCommand("payment-intents.cancel", "Cancel a payment intent", "POST", "/v1/payment-intents/{payment_intent_id}/cancel", "cancelPaymentIntent", "CancelPaymentIntentRequest", "PaymentIntentResponse", []Arg{pos("payment_intent_id", "pi_", "Flint payment intent ID"), flag("cancellation_reason", "string", "cancellation_reason", "", "requested_by_customer, duplicate, fraudulent, or abandoned", false), input}, true, true, true, false, "flint payment-intents cancel pi_123 --cancellation-reason abandoned --confirm"),
		alias(apiCommand("checkout-sessions.create", "Create a hosted checkout session", "POST", "/v1/checkout-sessions", "createCheckoutSession", "CreateCheckoutSessionRequest", "CheckoutSessionLaunchResponse", []Arg{flag("quick_pay_name", "string", "quick_pay_item.name", "", "Quick-pay item name", false), flag("amount", "integer", "quick_pay_item.amount_money.amount", "", "Amount in the smallest currency unit", false), flag("currency", "string", "quick_pay_item.amount_money.currency", "", "ISO currency code", false), input}, true, true, false, false, "flint checkout create --quick-pay-name T-shirt --amount 2500 --currency USD --open"), "checkout.create"),
		alias(apiCommand("checkout-sessions.get", "Get a checkout session", "GET", "/v1/checkout-sessions/{checkout_session_id}", "getCheckoutSession", "", "CheckoutSessionResponse", []Arg{pos("checkout_session_id", "cs_", "Flint checkout session ID")}, false, false, false, true, "flint checkout get cs_123"), "checkout.get"),
		alias(apiCommand("checkout-sessions.list", "List checkout sessions", "GET", "/v1/checkout-sessions", "listCheckoutSessions", "", "CheckoutSessionListResponse", page, false, false, false, false, "flint checkout list --page-size 25"), "checkout.list"),
		apiCommand("refunds.create", "Create a refund", "POST", "/v1/refunds", "createRefund", "CreateRefundRequest", "RefundResponse", []Arg{idFlag("payment_intent", "pi_", "payment_intent_id", "", "Payment intent ID", false), idFlag("order", "ord_", "order_id", "", "Order ID", false), flag("amount", "integer", "amount_money.amount", "", "Refund amount in minor units", false), flag("currency", "string", "amount_money.currency", "", "ISO currency code", false), flag("reason", "string", "reason", "", "Refund reason", true), input}, true, true, false, false, "flint refunds create --payment-intent pi_123 --amount 1000 --currency USD --reason requested_by_customer"),
		apiCommand("refunds.get", "Get a refund", "GET", "/v1/refunds/{refund_id}", "getRefund", "", "RefundResponse", []Arg{pos("refund_id", "ref_", "Flint refund ID")}, false, false, false, true, "flint refunds get ref_123"),
		apiCommand("refunds.list", "List refunds", "GET", "/v1/refunds", "listRefunds", "", "RefundListResponse", with(page, idFlag("payment_intent", "pi_", "", "payment_intent_id", "Filter by payment intent", false), idFlag("order", "ord_", "", "order_id", "Filter by order", false), flag("status", "string", "", "status", "Filter by status", false)), false, false, false, false, "flint refunds list --payment-intent pi_123"),
	}
	commands = append(commands, additionalCommands(page, input)...)
	commands = append(commands, publicAPICommands(commands)...)
	return commands
}
