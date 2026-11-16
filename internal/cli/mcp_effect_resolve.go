package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

// These operations can omit the amount that Flint will resolve. Never present
// their component prices as the total the user is being asked to approve.
func mcpImplicitAmount(entry opindex.Entry, body map[string]any) bool {
	switch entry.ID {
	case "refunds.create":
		return body["amount_money"] == nil
	case "invoices.create", "invoices.issue", "invoices.collect", "orders.pay", "payouts.cancel":
		return true
	case "payouts.create":
		return body["amount_money"] == nil
	}
	return false
}

func mcpInteger(value any) (*big.Int, bool) {
	n, ok := new(big.Rat).SetString(fmt.Sprint(value))
	if !ok || !n.IsInt() {
		return nil, false
	}
	return new(big.Int).Set(n.Num()), true
}

func mcpMoney(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	currency, _ := m["currency"].(string)
	n, valid := mcpInteger(m["amount"])
	return m, valid && n.Sign() >= 0 && currency != ""
}

func mcpRemaining(total, refunded any) (map[string]any, bool) {
	t, ok := mcpMoney(total)
	r, valid := mcpMoney(refunded)
	if !ok || !valid || t["currency"] != r["currency"] {
		return nil, false
	}
	a, _ := mcpInteger(t["amount"])
	b, _ := mcpInteger(r["amount"])
	a.Sub(a, b)
	if a.Sign() < 0 {
		return nil, false
	}
	return map[string]any{"amount": json.Number(a.String()), "currency": t["currency"]}, true
}

// The result describes a read-time snapshot, not a reservation or a guarantee
// that prices or refundable balances cannot change before the write is accepted.
func (a *App) mcpResolvePreparedEffect(ctx context.Context, entry opindex.Entry, req preparedRequest, opts Options, resolved ResolvedConfig, auth AuthContext, key, baseURL string) map[string]any {
	effect := mcpPreparedEffect(entry, req)
	if !mcpImplicitAmount(entry, req.BodyValue) {
		return effect
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	body := req.BodyValue
	at := func(value any, path string) any { result, _ := lookupPath(value, path); return result }
	amount := map[string]any{"field": "resolved_amount_money", "resolution": "unresolved", "display": "amount resolved by Flint: unavailable before execution"}
	effect["amounts"] = []map[string]any{amount}
	note := "Amounts reflect the current resource; Flint resolves the final amount when executing."
	customer, recipient := "", ""
	resources := effect["resources"].([]string)
	addResource := func(id string) {
		if id != "" && !containsString(resources, id) {
			resources = append(resources, id)
		}
	}
	setAmount := func(value any, field, resolution, prefix string) bool {
		money, ok := mcpMoney(value)
		if !ok {
			return false
		}
		amount = map[string]any{"field": field, "amount": money["amount"], "currency": money["currency"], "display": prefix + mcpMoneyDisplay(money["amount"], fmt.Sprint(money["currency"])), "resolution": resolution}
		effect["amounts"] = []map[string]any{amount}
		return true
	}
	// Dry runs without permitted authenticated reads remain useful, but explicitly
	// state the missing amount. In particular, do not exchange offline workload tokens.
	canRead := key != "" && key != workloadCredential && (normalizeEnvironment(auth.Environment) != "live" || a.mcpGate.policy.allowLive)
	read := func(path string) map[string]any {
		if !canRead {
			return nil
		}
		operation, ok := matchPublicOperation(http.MethodGet, path)
		if !ok {
			return nil
		}
		readCtx := context.WithValue(ctx, responseContractKey{}, &Command{OperationID: operation.OperationID})
		response, e := a.doRequestWithin(readCtx, opts.Timeout, baseURL, key, http.MethodGet, path, nil, "", opts.Debug)
		if e != nil {
			note = "Amount lookup failed (" + e.Code + "); Flint will resolve the final amount when executing."
			return nil
		}
		object, _ := resourceRoot(response.Value).(map[string]any)
		return object
	}
	// Client preparation has not authenticated API keys. Verify the credential and
	// guards before allowing any effect lookup, using the normal credential flow.
	if canRead && opts.DryRunClient() {
		lookupOpts := opts
		lookupOpts.DryRun = ""
		command, _ := a.Registry.ByName("auth.status")
		readKey, readBase, envelope, e := a.authenticateCommand(ctx, command, lookupOpts, resolved)
		if e != nil {
			canRead = false
			note = "Amount lookup could not authenticate (" + e.Code + ")."
		} else {
			lookupOpts.Live = normalizeEnvironment(envelope.Data.Environment) == "live"
			if e := validateResolvedCredentialIntent(envelope.Data, lookupOpts, resolved, ""); e != nil {
				canRead = false
				note = "Amount lookup refused (" + e.Code + "); correct the server's configured guards or credential."
			}
			key, baseURL = readKey, readBase
			if normalizeEnvironment(envelope.Data.Environment) == "live" && !a.mcpGate.policy.allowLive {
				canRead = false
			}
			if a.mcpAuth != nil {
				a.mcpAuth.store(envelope.Data)
			}
		}
	}
	orderID, _ := body["order_id"].(string)
	var resource map[string]any
	switch entry.ID {
	case "refunds.create":
		if id, _ := body["credit_note_id"].(string); id != "" {
			resource = read("/v1/credit-notes/" + url.PathEscape(id))
			setAmount(resource["available_money"], "available_money", "upper_bound", "amount resolved by Flint: up to ")
			if invoiceID, _ := resource["invoice_id"].(string); invoiceID != "" {
				invoice := read("/v1/invoices/" + url.PathEscape(invoiceID))
				orderID, _ = invoice["order_id"].(string)
			}
		} else if id, _ := body["payment_intent_id"].(string); id != "" {
			resource = read("/v1/payment-intents/" + url.PathEscape(id))
			remaining, ok := mcpRemaining(resource["captured_money"], resource["refunded_money"])
			if ok {
				setAmount(remaining, "payment_intent.remaining_money", "upper_bound", "amount resolved by Flint: up to ")
			}
			if orderID == "" {
				orderID, _ = resource["order_id"].(string)
			}
		}
		if orderID != "" {
			order := read("/v1/orders/" + url.PathEscape(orderID))
			addResource(orderID)
			if resource == nil {
				resource = order
			}
			if body["refund_type"] == "targets" {
				if money, exact, ok := mcpRefundTargets(order, body); ok {
					prefix, resolution := "amount resolved by Flint: up to ", "upper_bound"
					if exact {
						prefix, resolution = "", "snapshot"
					}
					setAmount(money, "targets.remaining_money", resolution, prefix)
				} else if amount["resolution"] == "unresolved" {
					setAmount(at(order, "settlement_amounts.net_collected_money"), "order.net_collected_money", "upper_bound", "amount resolved by Flint: up to ")
				}
			} else if body["refund_type"] == "full" && amount["resolution"] == "unresolved" {
				setAmount(at(order, "settlement_amounts.net_collected_money"), "order.net_collected_money", "upper_bound", "amount resolved by Flint: up to ")
			}
			if customer == "" {
				customer, _ = order["customer_id"].(string)
			}
		}
		note += " Refund eligibility, target allocation and any merchant-recorded payment cap are resolved by Flint."
	case "invoices.create":
		if orderID != "" {
			resource = read("/v1/orders/" + url.PathEscape(orderID))
			setAmount(at(resource, "pricing_amounts.total_money"), "order.pricing_amounts.total_money", "snapshot", "")
		} else {
			quick, _ := body["quick_pay"].(map[string]any)
			customer, _ = quick["customer_id"].(string)
			if money, ok := mcpQuickPaySubtotal(quick); ok {
				setAmount(money, "quick_pay.subtotal_money", "subtotal", "subtotal ")
			}
			note = "Final invoice amount resolved by Flint at creation, including catalog prices, discounts, tips and tax; no invoice preview endpoint is available."
		}
	case "invoices.issue", "invoices.collect":
		resource = read(strings.TrimSuffix(strings.TrimSuffix(req.Path, "/issue"), "/collect"))
		field := "pricing_amounts.total_money"
		if entry.ID == "invoices.collect" {
			field = "settlement_amounts.currently_due_money"
		}
		setAmount(at(resource, field), "invoice."+field, "snapshot", "")
		recipient, _ = resource["recipient_email"].(string)
		customer, _ = at(resource, "snapshot.customer_display_name").(string)
		if id, _ := resource["order_id"].(string); id != "" {
			order := read("/v1/orders/" + url.PathEscape(id))
			addResource(id)
			if id, _ := order["customer_id"].(string); id != "" {
				customer = id
			}
		}
	case "orders.pay":
		resource = read(strings.TrimSuffix(req.Path, "/pay"))
		if body["action"] == "setup" {
			effect["amounts"] = []map[string]any{}
			note = "Save a payment method; no money is charged."
		} else {
			setAmount(at(resource, "settlement_amounts.outstanding_money"), "order.settlement_amounts.outstanding_money", "snapshot", "")
			note = "Outstanding balance at lookup; Flint resolves the payment and tender allocation when executing."
		}
	case "payouts.cancel":
		resource = read(strings.TrimSuffix(req.Path, "/cancel"))
		setAmount(resource["amount_money"], "payout.amount_money", "snapshot", "")
		if destination, _ := resource["payout_destination_id"].(string); destination != "" {
			addResource(destination)
		}
	}
	if customer == "" {
		customer, _ = resource["customer_id"].(string)
	}
	if customer != "" {
		effect["customer"] = customer
		amount["customer"] = customer
		addResource(customer)
	}
	if value, exists := body["recipient_email"]; exists {
		recipient, _ = value.(string)
	} else if recipient == "" && entry.ID == "invoices.create" && customer != "" {
		buyer := read("/v1/customers/" + url.PathEscape(customer))
		recipient, _ = buyer["email"].(string)
	}
	if recipient != "" {
		effect["recipient_email"] = recipient
	}
	effect["resources"] = resources
	if amount["resolution"] == "unresolved" && note == "Amounts reflect the current resource; Flint resolves the final amount when executing." {
		note = "The amount could not be read with this server's credential and live-access policy; Flint resolves it when executing."
	}
	if amount["resolution"] != "unresolved" && !strings.Contains(note, "lookup") && entry.ID != "invoices.create" && entry.ID != "orders.pay" {
		note = "Amounts reflect the current resource; Flint resolves the final amount when executing."
	}
	effect["amount_note"] = note
	facts := []string{}
	for _, m := range effect["amounts"].([]map[string]any) {
		facts = append(facts, fmt.Sprint(m["display"]))
	}
	if customer != "" {
		facts = append(facts, "customer "+customer)
	} else if strings.HasPrefix(entry.ID, "payouts.") {
		facts = append(facts, "merchant payout; no customer")
	} else {
		facts = append(facts, "customer unavailable before execution")
	}
	if recipient != "" {
		facts = append(facts, "recipient "+recipient)
	} else if strings.HasPrefix(entry.ID, "invoices.") {
		facts = append(facts, "recipient unset or unavailable before execution")
	}
	facts = append(facts, resources...)
	effect["summary"] = strings.TrimSuffix(entry.Title, ".") + ": " + strings.Join(facts, ", ") + ". " + note
	return effect
}

func mcpRefundTargets(order, body map[string]any) (map[string]any, bool, bool) {
	sum := new(big.Int)
	currency := ""
	exact := true
	count := 0
	for _, kind := range []struct{ list, id string }{{"charges", "order_charge_id"}, {"line_items", "order_line_item_id"}} {
		targets, _ := body[kind.list].([]any)
		rows, _ := order[kind.list].([]any)
		for _, raw := range targets {
			target, _ := raw.(map[string]any)
			var row map[string]any
			for _, raw := range rows {
				candidate, _ := raw.(map[string]any)
				if candidate[kind.id] == target[kind.id] {
					row = candidate
					break
				}
			}
			money, ok := mcpRemaining(row["total_money"], row["refunded_money"])
			if !ok {
				return nil, false, false
			}
			// Partial quantities, explicit tax and adjustments require Flint's allocator.
			if target["amount_money"] != nil || target["quantity"] != nil || target["tax_refund_mode"] != nil || target["refund_adjustments"] != nil || target["adjustment_refunds"] != nil {
				exact = false
			}
			if currency != "" && currency != money["currency"] {
				return nil, false, false
			}
			currency, _ = money["currency"].(string)
			n, _ := mcpInteger(money["amount"])
			sum.Add(sum, n)
			count++
		}
	}
	if taxes, _ := body["tax_breakdown_refunds"].([]any); len(taxes) > 0 {
		return nil, false, false
	}
	if count == 0 {
		return nil, false, false
	}
	return map[string]any{"amount": json.Number(sum.String()), "currency": currency}, exact, true
}

func mcpQuickPaySubtotal(quick map[string]any) (map[string]any, bool) {
	lines, _ := quick["line_items"].([]any)
	sum := new(big.Int)
	currency := ""
	for _, raw := range lines {
		line, _ := raw.(map[string]any)
		money, ok := mcpMoney(line["unit_price_money"])
		if !ok || line["variant_id"] != nil || line["bundle_id"] != nil || line["modifiers"] != nil {
			return nil, false
		}
		qty := big.NewInt(1)
		if value, exists := line["quantity"]; exists {
			var ok bool
			qty, ok = mcpInteger(value)
			if !ok || qty.Sign() < 1 {
				return nil, false
			}
		}
		if currency != "" && currency != money["currency"] {
			return nil, false
		}
		currency, _ = money["currency"].(string)
		n, _ := mcpInteger(money["amount"])
		sum.Add(sum, n.Mul(n, qty))
	}
	return map[string]any{"amount": json.Number(sum.String()), "currency": currency}, len(lines) > 0
}
