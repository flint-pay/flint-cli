package cli

import "strings"

// operationOverlay carries behavior by public operation ID, never a generated name.
// Top-level body flags are generated; nested shortcuts remain CLI conveniences.
type operationOverlay struct {
	Arguments      []Arg
	Examples       []string
	Render         string
	Open           bool
	Destructive    bool
	ReadOnly       bool
	TerminalStates map[string]bool
}

func operationOverlays() map[string]operationOverlay {
	overlays := map[string]operationOverlay{
		"getMerchant":                          {Examples: ex("flint merchant get")},
		"updateMerchant":                       {Examples: ex("flint merchant update --input merchant.json")},
		"createOrganization":                   {Arguments: []Arg{flag("name", "string", "name", "", "Organization name", true), idFlag("parent_organization", "org_", "parent_organization_id", "", "Organization to create this one under", true)}, Examples: ex("flint organizations create --name Acme --parent-organization org_123")},
		"listOrganizations":                    {Examples: ex("flint organizations list --page-size 25")},
		"getOrganization":                      {Examples: ex("flint organizations get org_123")},
		"updateOrganization":                   {Examples: ex("flint organizations update org_123 --input organization.json")},
		"createCustomer":                       {Arguments: []Arg{flag("email", "string", "email", "", "Customer email", false), flag("name", "string", "name", "", "Customer name", false)}, Examples: ex("flint customers create --email jane@example.com")},
		"getCustomer":                          {Examples: ex("flint customers get cus_123")},
		"listCustomers":                        {Examples: ex("flint customers list --page-size 25")},
		"updateCustomer":                       {Examples: ex("flint customers update cus_123 --input customer.json")},
		"createOrder":                          {Examples: ex("flint orders create --input order.json")},
		"getOrder":                             {Examples: ex("flint orders get ord_123"), TerminalStates: map[string]bool{"closed": true}},
		"listOrders":                           {Examples: ex("flint orders list --page-size 25")},
		"payOrder":                             {Arguments: []Arg{{Name: "payment_source_token", Flag: "--payment-source-token", Type: "string", Repeat: true, BodyPath: "payment_source_tokens", Description: "Payment intent to sandbox token mapping"}}, Examples: ex("flint orders pay ord_123 --payment-source-token pi_123=pm_card_visa")},
		"closeOrder":                           {Examples: ex("flint orders close ord_123 --confirm"), Destructive: true},
		"createPaymentIntent":                  {Arguments: []Arg{flag("amount", "integer", "amount_money.amount", "", "Amount in the smallest currency unit", false), flag("currency", "string", "amount_money.currency", "", "Uppercase ISO 4217 currency code, such as USD", false), idFlag("customer", "cus_", "customer_id", "", "Customer ID", false), flag("capture_method", "string", "capture_method", "", "automatic or manual", false), {Name: "payment_option", Flag: "--payment-option", Type: "string", Repeat: true, BodyPath: "payment_options", Description: "Standalone payment option; repeat for multiple options"}, flag("transaction_purpose", "string", "transaction_purpose", "", "goods, services, or other; required for ach_debit", false)}, Examples: ex("flint payment-intents create --amount 2500 --currency USD --payment-option card")},
		"getPaymentIntent":                     {Examples: ex("flint payment-intents get pi_123 --wait-for status=succeeded"), TerminalStates: map[string]bool{"succeeded": true, "canceled": true, "expired": true}},
		"listPaymentIntents":                   {Examples: ex("flint payment-intents list --status succeeded")},
		"confirmPaymentIntent":                 {Arguments: []Arg{flag("confirmation_token", "string", "stripe.confirmation_token", "", "Stripe confirmation token created with Stripe.js", false), flag("payment_source_token", "string", "payment_source_token", "", "Documented sandbox payment source token", false), idFlag("payment_method", "pm_", "payment_method_id", "", "Saved payment method ID", false)}, Examples: ex("flint payment-intents confirm pi_123 --confirmation-token ctoken_123")},
		"capturePaymentIntent":                 {Arguments: []Arg{flag("amount", "integer", "amount_money.amount", "", "Optional capture amount", false), flag("currency", "string", "amount_money.currency", "", "Currency for capture amount", false)}, Examples: ex("flint payment-intents capture pi_123")},
		"cancelPaymentIntent":                  {Arguments: []Arg{flag("cancellation_reason", "string", "cancellation_reason", "", "requested_by_customer, duplicate, fraudulent, or abandoned", false)}, Examples: ex("flint payment-intents cancel pi_123 --cancellation-reason abandoned --confirm"), Destructive: true},
		"createCheckoutSession":                {Arguments: []Arg{flag("quick_pay_name", "string", "quick_pay_item.name", "", "Quick-pay item name", false), flag("amount", "integer", "quick_pay_item.amount_money.amount", "", "Amount in the smallest currency unit", false), flag("currency", "string", "quick_pay_item.amount_money.currency", "", "Uppercase ISO 4217 currency code, such as USD", false)}, Examples: ex("flint checkout-sessions create --quick-pay-name T-shirt --amount 2500 --currency USD --open"), Render: "checkout", Open: true},
		"getCheckoutSession":                   {Examples: ex("flint checkout-sessions get cs_123"), TerminalStates: map[string]bool{"paid": true, "expired": true, "closed": true, "invalidated": true}},
		"listCheckoutSessions":                 {Examples: ex("flint checkout-sessions list --page-size 25")},
		"createRefund":                         {Arguments: []Arg{idFlag("payment_intent", "pi_", "payment_intent_id", "", "Payment intent ID", false), idFlag("order", "ord_", "order_id", "", "Order ID", false), flag("amount", "integer", "amount_money.amount", "", "Refund amount in minor units", false), flag("currency", "string", "amount_money.currency", "", "Uppercase ISO 4217 currency code, such as USD", false), flag("refund_type", "string", "refund_type", "", "full, amount, or targets (required)", false), flag("reason", "string", "reason", "", "Optional refund reason", false)}, Examples: ex("flint refunds create --payment-intent pi_123 --refund-type amount --amount 1000 --currency USD --reason requested_by_customer")},
		"getRefund":                            {Examples: ex("flint refunds get ref_123"), TerminalStates: map[string]bool{"failed": true}},
		"listRefunds":                          {Examples: ex("flint refunds list --payment-intent-id pi_123")},
		"sendPaymentIntentReceipt":             {Examples: ex("flint payment-intents send-receipt pi_123")},
		"createCustomerVerification":           {Arguments: []Arg{idFlag("customer", "cus_", "customer_id", "", "Customer ID", false), flag("purpose", "string", "purpose", "", "link_guest_purchases", false)}, Examples: ex("flint customer-verifications create --input verification.json")},
		"confirmCustomerVerification":          {Examples: ex("flint customer-verifications confirm cver_123 --input -")},
		"linkCustomerGuestPurchases":           {Arguments: []Arg{idFlag("customer_verification", "cver_", "customer_verification_id", "", "Confirmed customer verification ID", false)}, Examples: ex("flint customers link-guest-purchases cus_123 --input linking.json")},
		"lookupEmailPreferenceLink":            {Examples: ex("flint email-preference-links lookup --input -"), ReadOnly: true},
		"unsubscribeEmailPreferenceLink":       {Examples: ex("flint email-preference-links unsubscribe --input -")},
		"validateCustomDomain":                 {Examples: ex("flint custom-domains validate cdom_123")},
		"createPaymentLink":                    {Arguments: []Arg{flag("name", "string", "name", "", "Payment link name", true), flag("item_name", "string", "line_items.0.name", "", "Item name", false), flag("amount", "integer", "line_items.0.unit_price_money.amount", "", "Item price in minor units", false), flag("currency", "string", "line_items.0.unit_price_money.currency", "", "Uppercase ISO 4217 currency code, such as USD", false)}, Examples: ex("flint payment-links create --name T-shirt --item-name T-shirt --amount 2500 --currency USD --open"), Render: "checkout", Open: true},
		"listPaymentLinks":                     {Examples: ex("flint payment-links list --page-size 25")},
		"createInvoice":                        {Arguments: []Arg{idFlag("order", "ord_", "order_id", "", "Order ID", false), flag("recipient_email", "string", "recipient_email", "", "Invoice recipient email", false)}, Examples: ex("flint invoices create --order ord_123 --recipient-email jane@example.com")},
		"issueInvoice":                         {Arguments: []Arg{flag("delivery_mode", "string", "delivery_mode", "", "Invoice delivery mode", false)}, Examples: ex("flint invoices issue inv_123 --delivery-mode email")},
		"getInvoice":                           {Examples: ex("flint invoices get inv_123")},
		"createInvoiceLateFee":                 {Arguments: []Arg{idFlag("invoice_schedule_entry_id", "invse_", "invoice_schedule_entry_id", "", "Schedule entry ID for a scheduled invoice", false)}, Examples: ex("flint invoices late-fees create inv_123")},
		"waiveInvoiceLateFee":                  {Arguments: []Arg{flag("reason", "string", "reason_message", "", "Note explaining why the fee is waived", true)}, Examples: ex("flint invoices late-fees waive inv_123 inlf_123 --reason courtesy")},
		"listInvoices":                         {Examples: ex("flint invoices list --customer-id cus_123")},
		"createWebhookEndpoint":                {Arguments: []Arg{flag("url", "string", "url", "", "HTTPS delivery URL", true), {Name: "enabled_event", Flag: "--enabled-event", Type: "string", BodyPath: "enabled_events", Repeat: true, Description: "Event type to deliver"}, {Name: "signing_method", Flag: "--signing-method", Type: "string", BodyPath: "signing_methods", Repeat: true, Description: "Signing method: jwt (default, verify with Flint's public keys) or hmac (shared secret)"}}, Examples: ex("flint webhook-endpoints create --url https://example.com/webhooks/flint --enabled-event payment_intent.succeeded --signing-method jwt")},
		"getWebhookEndpoint":                   {Examples: ex("flint webhook-endpoints get whep_123")},
		"listWebhookEndpoints":                 {Examples: ex("flint webhook-endpoints list --page-size 25")},
		"updateWebhookEndpoint":                {Arguments: []Arg{flag("url", "string", "url", "", "HTTPS delivery URL", false), {Name: "enabled_event", Flag: "--enabled-event", Type: "string", BodyPath: "enabled_events", Repeat: true}, {Name: "signing_method", Flag: "--signing-method", Type: "string", BodyPath: "signing_methods", Repeat: true, Description: "Signing method: jwt (default, verify with Flint's public keys) or hmac (shared secret)"}, flag("expected_version", "integer", "expected_version", "", "Endpoint version last read; required with --enabled-event or --signing-method", false)}, Examples: ex("flint webhook-endpoints update whep_123 --input endpoint.json", "flint webhook-endpoints update whep_123 --enabled-event payment_intent.succeeded --expected-version 3")},
		"createWebhookTestEvent":               {Arguments: []Arg{flag("event_type", "string", "event_type", "", "Webhook event type", true)}, Examples: ex("flint webhook-endpoints test-events create whep_123 --event-type payment_intent.succeeded")},
		"rotateWebhookEndpointSecret":          {Examples: ex("flint webhook-endpoints rotate-secret whep_123 --confirm"), Destructive: true},
		"deleteWebhookEndpoint":                {Examples: ex("flint webhook-endpoints delete whep_123 --confirm")},
		"createDeliveryRateCallback":           {Arguments: []Arg{{Name: "signing_method", Flag: "--signing-method", Type: "string", BodyPath: "signing_methods", Repeat: true, Description: "Signing method: jwt (default, verify with Flint's public keys) or hmac (shared secret)"}}, Examples: ex("flint delivery-rate-callbacks create --input callback.json --signing-method jwt")},
		"updateDeliveryRateCallback":           {Arguments: []Arg{{Name: "signing_method", Flag: "--signing-method", Type: "string", BodyPath: "signing_methods", Repeat: true, Description: "Signing method: jwt (default, verify with Flint's public keys) or hmac (shared secret)"}, flag("expected_version", "integer", "expected_version", "", "Callback version last read; required with --signing-method or a configuration change", false)}, Examples: ex("flint delivery-rate-callbacks update dcb_123 --input callback.json", "flint delivery-rate-callbacks update dcb_123 --signing-method jwt --expected-version 3")},
		"rotateDeliveryRateCallbackSigningKey": {Examples: ex("flint delivery-rate-callbacks rotate-secret dcb_123 --confirm")},
		"getWebhookEvent":                      {Examples: ex("flint webhook-events get whev_123")},
		"listWebhookEvents":                    {Examples: ex("flint webhook-events list --event-type payment_intent.succeeded", "flint webhook-events list --webhook-endpoint-id whep_123 --delivery-status failed")},
		"getWebhookDelivery":                   {Examples: ex("flint webhook-deliveries get wdel_123")},
		"listWebhookDeliveryAttempts":          {Examples: ex("flint webhook-deliveries attempts list wdel_123")},
		"resendWebhookDelivery":                {Examples: ex("flint webhook-deliveries resend wdel_123")},
		"getAPIRequestLog":                     {Examples: ex("flint api-request-logs get rlog_123"), Render: "request_log"},
		"listAPIRequestLogs":                   {Examples: ex("flint api-request-logs list --status-bucket server_error"), Render: "request_log"},
		"getResourceTimeline":                  {Examples: ex("flint resource-timelines get pi_123", "flint resource-timelines get @last.pi")},
		"listSandboxes":                        {Examples: ex("flint sandboxes list --live")},
		"createSandbox":                        {Arguments: []Arg{flag("name", "string", "name", "", "Sandbox name", true), apiKeyAccessFlag, apiKeyScopeFlag, flag("test_key_name", "string", "test_key_name", "", "Name for the sandbox's test key; with --access or --scope, the sandbox issues one", false)}, Examples: ex("flint sandboxes create --name qa-scenarios --live", "flint sandboxes create --name qa-scenarios --issue-test-key true --access restricted --scope customers.read --live")},
		"resetSandbox":                         {Examples: ex("flint sandboxes reset test_123 --live --confirm"), Destructive: true},
		"createSandboxAPIKey":                  {Arguments: []Arg{flag("name", "string", "name", "", "Test key name", false), apiKeyAccessFlag, apiKeyScopeFlag}, Examples: ex("flint sandboxes api-keys create test_123 --name qa --scope customers.read --live", "flint sandboxes api-keys create test_123 --name qa --access read_only --live")},
		"createAPIKey":                         {Arguments: []Arg{flag("name", "string", "name", "", "API key name", true), apiKeyAccessFlag, apiKeyScopeFlag}, Examples: ex("flint api-keys create --name local-dev --scope payments.payment_intents.read", "flint api-keys create --name ci-readonly --access read_only")},
		"updateAPIKey":                         {Arguments: []Arg{flag("name", "string", "name", "", "API key name", false), apiKeyAccessFlag, apiKeyScopeFlag, flag("expected_version", "integer", "expected_version", "", "Update only if the key is still at this version; required with --access or --scope", false)}, Examples: ex("flint api-keys update key_123 --name ci", "flint api-keys update key_123 --access restricted --scope customers.read --expected-version 3", "flint api-keys update key_123 --access read_only --expected-version 3")},
		"listAPIKeys":                          {Examples: ex("flint api-keys list")},
		"revokeAPIKey":                         {Arguments: []Arg{flag("expected_version", "integer", "expected_version", "", "Revoke only if the key is still at this version", false)}, Examples: ex("flint api-keys revoke key_123 --preview", "flint api-keys revoke key_123 --confirm"), Destructive: true},
	}
	for id, overlay := range overlays {
		var shortcuts []Arg
		for _, arg := range overlay.Arguments {
			if strings.Contains(arg.BodyPath, ".") || id == "payOrder" && arg.Name == "payment_source_token" {
				switch id {
				case "createPaymentLink":
					arg.ShortcutTagPath = "line_items.0.type"
					arg.ShortcutTagValue = "custom_fixed"
				case "createCheckoutSession":
					arg.ShortcutTagPath = "type"
					arg.ShortcutTagValue = "quick_pay"
				case "confirmPaymentIntent":
					arg.ShortcutTagPath = "credential_type"
					arg.ShortcutTagValue = "stripe_confirmation_token"
				}
				shortcuts = append(shortcuts, arg)
			}
		}
		overlay.Arguments = shortcuts
		for i, example := range overlay.Examples {
			for _, rename := range [][2]string{{"--parent-organization", "--parent-organization-id"}, {"--customer", "--customer-id"}, {"--payment-option", "--payment-options"}, {"--payment-method", "--payment-method-id"}, {"--payment-intent", "--payment-intent-id"}, {"--order", "--order-id"}, {"--customer-verification", "--customer-verification-id"}, {"--enabled-event", "--enabled-events"}, {"--signing-method", "--signing-methods"}, {"--scope", "--scopes"}} {
				example = strings.ReplaceAll(example, rename[0]+" ", rename[1]+" ")
			}
			if id == "waiveInvoiceLateFee" {
				example = strings.ReplaceAll(example, "--reason ", "--reason-message ")
			}
			overlay.Examples[i] = example
		}
		overlays[id] = overlay
	}
	return overlays
}

var publicOperationOverlays = operationOverlays()
