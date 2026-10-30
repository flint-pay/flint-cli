package cli

// Recipes bind to operation IDs so route and command renames stay in the spec.
type triggerRecipe struct {
	Resumable bool          `json:"resumable"`
	Name      string        `json:"name"`
	Async     string        `json:"async,omitempty"`
	Pending   []string      `json:"pending,omitempty"`
	Steps     []triggerStep `json:"steps"`
}
type triggerStep struct {
	Name, OperationID string
	Path, Query       map[string]string
	Body              map[string]any
	Outputs, Secrets  map[string]string
	Credential        string
	Until             []string
	Accepted          []string
}
type triggerEntry struct {
	Event   string         `json:"event_type"`
	Recipe  string         `json:"recipe"`
	Through string         `json:"through,omitempty"`
	Set     map[string]any `json:"-"`
	Also    []string       `json:"also_arrive,omitempty"`
}

func triggerRecipes() map[string]triggerRecipe {
	step := func(name, op string, body map[string]any, outputs map[string]string) triggerStep {
		return triggerStep{Name: name, OperationID: op, Body: body, Outputs: outputs}
	}
	obj := func(k string, v any) map[string]any { return map[string]any{k: v} }
	line := map[string]any{"type": "custom", "name": "Flint trigger item", "quantity": 1, "unit_price_money": map[string]any{"amount": 2500, "currency": "USD"}, "fulfillment": obj("requirement", "none")}
	customer := step("customer", "createCustomer", obj("name", "Flint trigger customer"), map[string]string{"customer_id": "data.customer_id"})
	order := step("order", "createOrder", map[string]any{"line_items": []any{line}, "tax": obj("enabled", false)}, map[string]string{"order_id": "data.order_id"})
	pay := step("pay", "payOrder", map[string]any{"action": "pay", "payment_source": map[string]any{"credential_type": "token", "token": "pm_card_visa"}, "expected_outstanding_money": "{order.settlement_amounts.outstanding_money}"}, map[string]string{"order_id": "data.order.order_id", "payment_intent_id": "data.order_payment_attempt.payment_intents.0.payment_intent_id", "order_payment_attempt_id": "data.order_payment_attempt.order_payment_attempt_id"})
	pay.Path = map[string]string{"order_id": "{order.order_id}"}
	refund := step("refund", "createRefund", map[string]any{"refund_type": "full", "order_id": "{order.order_id}", "reason": "requested_by_customer"}, map[string]string{"refund_id": "data.refund_id"})
	session := step("session", "createCheckoutSession", map[string]any{"type": "order", "order_id": "{order.order_id}", "tip": obj("enabled", false)}, map[string]string{"checkout_session_id": "data.checkout_session_id"})
	session.Secrets = map[string]string{"checkout_auth_token": "data.checkout_auth_token"}
	checkoutPay := pay
	checkoutPay.Credential = "{session.checkout_auth_token}"
	dispute := step("dispute", "listDisputes", nil, map[string]string{"dispute_id": "data.0.dispute_id"})
	dispute.Query = map[string]string{"order_id": "{order.order_id}"}
	dispute.Until = []string{"data.0.status=needs_response"}
	dispute.Accepted = []string{"needs_response", "under_review", "won", "lost"}
	disputePay := pay
	disputePay.Body = map[string]any{"action": "pay", "payment_source": map[string]any{"credential_type": "token", "token": "pm_card_createDispute"}, "expected_outstanding_money": "{order.settlement_amounts.outstanding_money}"}
	simulate := step("simulate", "simulateDispute", obj("outcome", "won"), nil)
	simulate.Path = map[string]string{"dispute_id": "{dispute.dispute_id}"}
	intent := step("intent", "createPaymentIntent", map[string]any{"amount_money": map[string]any{"amount": 2500, "currency": "USD"}, "payment_options": []any{"card"}, "customer_id": "{customer.customer_id}"}, map[string]string{"payment_intent_id": "data.payment_intent_id"})
	confirm := step("confirm", "confirmPaymentIntent", map[string]any{"credential_type": "token", "payment_source_token": "pm_card_visa", "save_payment_method": true}, nil)
	confirm.Path = map[string]string{"payment_intent_id": "{intent.payment_intent_id}"}
	method := step("payment_method", "listPaymentMethods", nil, map[string]string{"payment_method_id": "data.0.payment_method_id"})
	method.Query = map[string]string{"customer_id": "{customer.customer_id}", "status": "active"}
	method.Until = []string{"data.0.status=active"}
	method.Accepted = []string{"active"}
	clock := step("clock", "createTestClock", obj("name", "flint trigger"), map[string]string{"test_clock_id": "data.test_clock_id"})
	clockCustomer := customer
	clockCustomer.Body = map[string]any{"name": "Flint trigger customer", "test_clock_id": "{clock.test_clock_id}"}
	plan := step("plan", "createSubscriptionPlan", map[string]any{"name": "Flint trigger plan", "currency": "USD", "billing_interval": "monthly", "billing_interval_count": 1, "line_items": []any{map[string]any{"type": "custom", "name": "Flint trigger item", "quantity": 1, "unit_price_money": map[string]any{"amount": 1000, "currency": "USD"}}}}, map[string]string{"subscription_plan_id": "data.subscription_plan_id"})
	subscription := step("subscription", "createSubscription", map[string]any{"customer_id": "{customer.customer_id}", "payment_method_id": "{payment_method.payment_method_id}", "billing_start": obj("type", "immediate"), "terms": map[string]any{"type": "subscription_plan", "subscription_plan_id": "{plan.subscription_plan_id}"}}, map[string]string{"subscription_id": "data.subscription_id"})
	active := step("active", "getSubscription", nil, nil)
	active.Path = map[string]string{"subscription_id": "{subscription.subscription_id}"}
	active.Until = []string{"status=active"}
	active.Accepted = []string{"active", "paused", "past_due", "canceled"}
	advance := step("advance", "advanceTestClock", obj("frozen_at", triggerClockAdvance{From: "{clock.frozen_at}", Months: plan.Body["billing_interval_count"].(int), MarginSeconds: 60}), nil)
	advance.Path = map[string]string{"test_clock_id": "{clock.test_clock_id}"}
	ready := step("ready", "getTestClock", nil, nil)
	ready.Path = advance.Path
	ready.Until = []string{"status=ready"}
	ready.Accepted = []string{"ready"}
	invoice := step("invoice", "createInvoice", map[string]any{"type": "quick_pay", "quick_pay": order.Body, "collection": obj("mode", "external"), "issue": obj("delivery_mode", "caller_managed")}, map[string]string{"invoice_id": "data.invoice_id"})
	payment := step("payment", "createInvoiceManualPayment", obj("amount_money", "{invoice.outstanding_money}"), map[string]string{"invoice_manual_payment_id": "data.invoice_manual_payment.invoice_manual_payment_id"})
	payment.Path = map[string]string{"invoice_id": "{invoice.invoice_id}"}
	return map[string]triggerRecipe{
		"customer":         {Resumable: true, Name: "customer", Steps: []triggerStep{customer}},
		"order_payment":    {Resumable: true, Name: "order_payment", Steps: []triggerStep{order, pay}},
		"refund":           {Resumable: true, Name: "refund", Steps: []triggerStep{order, pay, refund}},
		"checkout_payment": {Resumable: true, Name: "checkout_payment", Steps: []triggerStep{order, session, checkoutPay}},
		"dispute":          {Resumable: true, Name: "dispute", Async: "stripe_webhook", Steps: []triggerStep{order, disputePay, dispute, simulate}},
		"saved_card":       {Resumable: true, Name: "saved_card", Async: "background_job", Steps: []triggerStep{customer, intent, confirm, method}},
		"subscription":     {Resumable: true, Name: "subscription", Async: "temporal", Steps: []triggerStep{clock, clockCustomer, intent, confirm, method, plan, subscription, active, advance, ready}},
		"invoice":          {Resumable: true, Name: "invoice", Steps: []triggerStep{invoice, payment}},
	}
}

func triggerEntries() []triggerEntry {
	var entries []triggerEntry
	add := func(recipe, through string, events ...string) {
		for _, event := range events {
			entries = append(entries, triggerEntry{Event: event, Recipe: recipe, Through: through, Also: events})
		}
	}
	add("customer", "", "customer.created")
	add("order_payment", "order", "order.created")
	add("order_payment", "", "order.paid", "order.closed", "payment_intent.created", "payment_intent.succeeded")
	entries = append(entries, triggerEntry{Event: "payment_intent.payment_failed", Recipe: "order_payment", Set: map[string]any{"pay.payment_source.token": "pm_card_chargeDeclined"}})
	add("refund", "", "refund.created", "refund.succeeded", "order.refunded")
	entries = append(entries, triggerEntry{Event: "refund.failed", Recipe: "refund", Set: map[string]any{"pay.payment_source.token": "pm_card_refundFail"}})
	add("checkout_payment", "session", "checkout_session.created")
	add("checkout_payment", "", "checkout_session.paid")
	add("dispute", "dispute", "dispute.created", "dispute.needs_response")
	add("dispute", "", "dispute.updated", "dispute.won")
	entries = append(entries, triggerEntry{Event: "dispute.lost", Recipe: "dispute", Set: map[string]any{"simulate.outcome": "lost"}})
	add("saved_card", "", "payment_method.activated")
	add("subscription", "clock", "test_clock.created")
	add("subscription", "active", "subscription.created", "subscription.activated", "subscription.payment_succeeded")
	add("subscription", "advance", "test_clock.advancing")
	add("subscription", "", "test_clock.ready", "subscription.renewal_upcoming")
	add("invoice", "invoice", "invoice.created", "invoice.issued")
	add("invoice", "", "invoice.paid", "invoice_manual_payment.created")
	// Related events are advisory; the requested event remains the primary expectation.
	for i := range entries {
		entry := &entries[i]
		payment := []string{"order.created", "payment_intent.created", "payment_intent.succeeded", "order.paid", "order.closed"}
		switch entry.Recipe {
		case "order_payment":
			if entry.Through == "" {
				entry.Also = payment
			}
			if entry.Event == "payment_intent.payment_failed" {
				entry.Also = []string{"order.created", "payment_intent.created"}
			}
		case "refund":
			entry.Also = append(payment, "refund.created", "refund.succeeded", "order.refunded")
		case "checkout_payment":
			entry.Also = []string{"order.created", "checkout_session.created"}
			if entry.Through == "" {
				entry.Also = append(entry.Also, payment[1:]...)
			}
		case "dispute":
			entry.Also = append(payment, "dispute.created", "dispute.needs_response")
			if entry.Through == "" {
				entry.Also = append(entry.Also, "dispute.updated")
				if entry.Event != "dispute.lost" {
					entry.Also = append(entry.Also, "dispute.won")
				}
			}
		case "saved_card":
			entry.Also = []string{"customer.created", "payment_intent.created", "payment_intent.succeeded", "payment_method.activated", "payment_intent.updated"}
		case "subscription":
			entry.Also = []string{"test_clock.created"}
			if entry.Through != "clock" {
				entry.Also = append(entry.Also, "customer.created", "payment_intent.created", "payment_intent.succeeded", "payment_method.activated", "subscription.created", "subscription.activated", "subscription.payment_succeeded")
			}
			if entry.Through == "advance" || entry.Through == "" {
				entry.Also = append(entry.Also, "test_clock.advancing")
			}
			if entry.Through == "" {
				entry.Also = append(entry.Also, "test_clock.ready", "subscription.renewal_upcoming")
			}
		case "invoice":
			entry.Also = []string{"order.created", "invoice.created", "invoice.issued"}
			if entry.Through == "" {
				entry.Also = append(entry.Also, "invoice_manual_payment.created", "invoice.paid", "order.paid")
			}
		}
		also := []string{}
		for _, event := range entry.Also {
			if event != entry.Event {
				also = append(also, event)
			}
		}
		entry.Also = also
	}
	return entries
}
