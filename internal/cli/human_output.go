package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/currency"
)

// Recognize the contract's Money shape without interpreting other amount fields.
func humanMoney(value any) (string, bool) {
	m, ok := value.(map[string]any)
	if !ok || len(m) != 2 {
		return "", false
	}
	code, ok := m["currency"].(string)
	if !ok || code == "" {
		return "", false
	}
	var amount int64
	switch n := m["amount"].(type) {
	case json.Number:
		var err error
		amount, err = n.Int64()
		if err != nil {
			return "", false
		}
	case int:
		amount = int64(n)
	case int64:
		amount = n
	default:
		return "", false
	}
	digits := strconv.FormatInt(amount, 10)
	unit, err := currency.ParseISO(code)
	if err != nil || unit == currency.XXX {
		return digits + " " + terminalSafe(code) + " (minor units)", true
	}
	scale, _ := currency.Standard.Rounding(unit)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale+1-len(digits)) + digits
		}
		digits = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	return sign + digits + " " + code, true
}

// Use public field names to select a compact summary. Detail commands retain
// every field, and JSON keeps the complete list resources.
func renderResourceTable(w io.Writer, rows []any, cmd *Command, now time.Time) bool {
	if len(rows) == 0 {
		return false
	}
	available := map[string]bool{}
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			return false
		}
		for key := range m {
			available[key] = true
		}
	}
	keys := make([]string, 0, len(available))
	for key := range available {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	columns := []string{}
	addFirst := func(candidates ...string) {
		for _, key := range candidates {
			if available[key] {
				columns = append(columns, key)
				return
			}
		}
	}
	// Resource identifiers generally precede foreign IDs in the alphabet. Prefer
	// the command's own resource name when several identifiers are present.
	if cmd != nil && cmd.OperationID == "listCustomerTaxExemptions" {
		addFirst("customer_tax_exemption_id")
	}
	if cmd != nil && cmd.OperationID == "listShopListings" {
		addFirst("shop_listing_id")
	}
	if cmd != nil && len(columns) == 0 {
		parts := strings.Split(cmd.CanonicalName, ".")
		group := parts[0]
		if len(parts) > 1 {
			group = parts[len(parts)-2]
		}
		group = strings.ReplaceAll(group, "-", "_")
		switch {
		case strings.HasSuffix(group, "ies"):
			group = strings.TrimSuffix(group, "ies") + "y"
		case strings.HasSuffix(group, "sses"):
			group = strings.TrimSuffix(group, "es")
		default:
			group = strings.TrimSuffix(group, "s")
		}
		addFirst(group + "_id")
	}
	if len(columns) == 0 {
		for _, key := range keys {
			if strings.HasSuffix(key, "_id") || key == "id" {
				addFirst(key)
				break
			}
		}
	}
	statuses, money := []string{"status"}, []string{"total_money", "amount_money"}
	for _, key := range keys {
		if strings.HasSuffix(key, "_status") {
			statuses = append(statuses, key)
		}
		if strings.HasSuffix(key, "_money") {
			money = append(money, key)
		}
	}
	if cmd != nil && (cmd.OperationID == "listOrders" || cmd.OperationID == "listMeOrders") {
		for _, key := range []string{"pricing_amounts.total_money", "buyer_contact.email", "customer.email"} {
			for _, row := range rows {
				if value, ok := lookupPath(row, key); ok && value != nil {
					available[key] = true
				}
			}
		}
		addFirst("order_number")
		addFirst("payment_status")
		addFirst("status")
		addFirst("pricing_amounts.total_money", "total_money")
		addFirst("buyer_contact.email", "customer.email", "customer_id")
	} else if cmd != nil && cmd.OperationID == "listCustomerTaxExemptions" {
		for _, key := range []string{"reason", "coverage", "certificate_number", "start_date", "end_date", "status"} {
			addFirst(key)
		}
	} else if cmd != nil && cmd.OperationID == "listShopListings" {
		addFirst("resolved_name")
		addFirst("resolved_price_range")
		addFirst("product_id")
		addFirst("bundle_id")
	} else if cmd != nil && cmd.OperationID == "listTaxRegistrations" {
		for _, key := range []string{"country", "state", "status", "registration_number_last4", "start_date", "end_date"} {
			addFirst(key)
		}
	} else if cmd != nil && cmd.OperationID == "listPayoutDestinations" {
		for _, key := range []string{"bank_name", "last4", "currency", "is_default_for_currency", "status"} {
			addFirst(key)
		}
	} else {
		addFirst(statuses...)
		addFirst(money...)
		addFirst("name", "email")
	}
	addFirst("created_at")
	if len(columns) == 0 {
		return false
	}
	cells := make([][]string, 0, len(rows))
	for _, row := range rows {
		m := row.(map[string]any)
		line := make([]string, len(columns))
		for i, key := range columns {
			v, _ := lookupPath(m, key)
			if key == "buyer_contact.email" || key == "customer.email" || key == "customer_id" && cmd != nil && (cmd.OperationID == "listOrders" || cmd.OperationID == "listMeOrders") {
				v, _ = firstStringAt(m, "buyer_contact.email", "customer.email", "customer_id")
			}
			if key == "coverage" && cmd != nil && cmd.OperationID == "listCustomerTaxExemptions" {
				coverage, _ := v.(map[string]any)
				if coverage["type"] == "all_states" {
					line[i] = "All states"
				} else if states, ok := coverage["states"].([]any); ok {
					values := make([]string, 0, len(states))
					for _, state := range states {
						values = append(values, terminalSafe(fmt.Sprint(state)))
					}
					line[i] = strings.Join(values, ", ")
				}
			} else if key == "resolved_price_range" && v != nil {
				priceRange, _ := v.(map[string]any)
				minimum, _ := humanMoney(priceRange["min_unit_price_money"])
				maximum, _ := humanMoney(priceRange["max_unit_price_money"])
				line[i] = minimum
				if maximum != "" && maximum != minimum {
					line[i] = strings.TrimSpace(minimum + " to " + maximum)
				}
			} else if text, ok := humanMoney(v); ok {
				line[i] = text
			} else if key == "created_at" {
				if stamp, ok := v.(string); ok {
					if t, err := time.Parse(time.RFC3339, stamp); err == nil {
						line[i] = relativeAge(now.Sub(t))
					}
				}
			} else if v != nil {
				switch v.(type) {
				case map[string]any, []any:
					return false
				default:
					line[i] = formatHumanScalar(v, now)
				}
			}
			if line[i] == "" {
				line[i] = "-"
			}
		}
		cells = append(cells, line)
	}
	headers := make([]string, len(columns))
	for i, key := range columns {
		headers[i] = humanLabel(key)
		if cmd != nil && cmd.OperationID == "listPayoutDestinations" {
			switch key {
			case "last4":
				headers[i] = "Last 4"
			case "is_default_for_currency":
				headers[i] = "Default"
			}
		}
		if key == "resolved_price_range" {
			headers[i] = "Price"
		}
		if key == "resolved_name" {
			headers[i] = "Name"
		}
		if key == "pricing_amounts.total_money" {
			headers[i] = "Total"
		}
		if key == "buyer_contact.email" || key == "customer.email" || key == "customer_id" && cmd != nil && (cmd.OperationID == "listOrders" || cmd.OperationID == "listMeOrders") {
			headers[i] = "Buyer"
		}
	}
	renderTextTable(w, headers, cells)
	return true
}

func renderTextTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = utf8.RuneCountInString(terminalSafe(header))
	}
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(terminalSafe(cell)))
		}
	}
	printRow := func(row []string) {
		for i, cell := range row {
			if i > 0 {
				fmt.Fprint(w, "  ")
			}
			text := terminalSafe(cell)
			fmt.Fprint(w, text)
			if i < len(row)-1 {
				fmt.Fprint(w, strings.Repeat(" ", widths[i]-utf8.RuneCountInString(text)))
			}
		}
		fmt.Fprintln(w)
	}
	printRow(headers)
	for _, row := range rows {
		printRow(row)
	}
}

// Lead with connection health and keep the country catalog compact in text output.
func renderTaxConnectionHuman(w io.Writer, data map[string]any, now time.Time) {
	remaining := copyMap(data)
	for _, key := range []string{"status", "status_reason", "supported_actions"} {
		if value, exists := remaining[key]; exists {
			renderHumanValue(w, map[string]any{key: value}, "", now, 0)
			delete(remaining, key)
		}
	}
	if coverage, ok := taxCoverageSummary(remaining["coverage"]); ok {
		fmt.Fprintf(w, "%-20s %s\n", "Coverage", terminalSafe(coverage))
		delete(remaining, "coverage")
	}
	if len(remaining) > 0 {
		renderHumanValue(w, remaining, "", now, 0)
	}
}

func taxCoverageSummary(value any) (string, bool) {
	entries, ok := value.([]any)
	if !ok {
		return "", false
	}
	if len(entries) == 0 {
		return "(none)", true
	}
	groups := map[string][]string{}
	for _, value := range entries {
		entry, _ := value.(map[string]any)
		country, countryOK := entry["country"].(string)
		scope, scopeOK := entry["category_scope"].(string)
		if !countryOK || !scopeOK || country == "" || scope == "" {
			return "", false
		}
		groups[scope] = append(groups[scope], country)
	}
	scopes := make([]string, 0, len(groups))
	for scope := range groups {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	parts := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		countries := groups[scope]
		label := scope
		if scope == "digital" {
			label = "digital only"
		}
		if len(countries) == 1 {
			parts = append(parts, countries[0]+": "+label)
		} else {
			parts = append(parts, fmt.Sprintf("%d countries: %s", len(countries), label))
		}
	}
	return strings.Join(parts, "; "), true
}
