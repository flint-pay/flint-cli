package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHumanMoneyCurrencyPrecision(t *testing.T) {
	for _, tc := range []struct{ amount, code, want string }{
		{"2500", "USD", "25.00 USD"}, {"2500", "EUR", "25.00 EUR"},
		{"2500", "JPY", "2500 JPY"}, {"2500", "KRW", "2500 KRW"},
		{"2500", "KWD", "2.500 KWD"}, {"1", "BHD", "0.001 BHD"},
		{"-1", "USD", "-0.01 USD"}, {"0", "KWD", "0.000 KWD"},
		{"-9223372036854775808", "USD", "-92233720368547758.08 USD"},
		{"2500", "ZZZ", "2500 ZZZ (minor units)"},
	} {
		t.Run(tc.code+tc.amount, func(t *testing.T) {
			got, ok := humanMoney(map[string]any{"amount": json.Number(tc.amount), "currency": tc.code})
			if !ok || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
	for _, value := range []any{
		map[string]any{"amount": json.Number("1.5"), "currency": "USD"},
		map[string]any{"amount": json.Number("12"), "currency": "USD", "other": true},
		map[string]any{"amount": "12", "currency": "USD"},
	} {
		if _, ok := humanMoney(value); ok {
			t.Fatalf("recognized non-Money: %v", value)
		}
	}
}

func TestHumanOutputMoneyLabelsAndCompactList(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	rows := []any{
		map[string]any{"customer_id": "cus_a", "name": "Alice", "status": "active", "amount_money": map[string]any{"amount": 1234, "currency": "USD"}, "created_at": now.Add(-time.Hour).Format(time.RFC3339), "metadata": map[string]any{"private_detail": "not a summary"}},
		map[string]any{"customer_id": "cus_b", "email": "bob@example.test", "status": "inactive", "amount_money": map[string]any{"amount": 25, "currency": "JPY"}},
	}
	envelope := map[string]any{"data": rows}
	before, _ := json.Marshal(envelope)
	var out bytes.Buffer
	if err := renderHuman(&out, envelope, &Command{CanonicalName: "customers.list"}, now); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Customer ID", "Amount money", "12.34 USD", "25 JPY", "1h ago", "cus_b"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "not a summary") || strings.Count(out.String(), "Customer ID") != 1 || strings.Count(out.String(), "\n") != 3 {
		t.Fatalf("not compact: %s", &out)
	}
	after, _ := json.Marshal(envelope)
	if !bytes.Equal(before, after) {
		t.Fatal("human rendering mutated JSON resources")
	}
	out.Reset()
	renderHumanValue(&out, map[string]any{"short": "one", "a_very_long_label_beyond_twenty": "two", "amount_money": map[string]any{"amount": 1, "currency": "KWD"}}, "", now, 0)
	first := -1
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		pos := strings.LastIndex(line, " ") + 1
		if first == -1 {
			first = pos
		} else if pos != first && !strings.HasSuffix(line, "KWD") {
			t.Fatalf("unaligned labels: %s", &out)
		}
	}
	if !strings.Contains(out.String(), "0.001 KWD") {
		t.Fatalf("money missing: %s", &out)
	}
}

func TestHumanListPrefersNestedResourceID(t *testing.T) {
	var out bytes.Buffer
	rows := []any{map[string]any{"address_id": "addr_one", "customer_id": "cus_parent", "name": "Work\naddress\x1b[31m"}}
	if !renderResourceTable(&out, rows, &Command{CanonicalName: "customers.addresses.list"}, time.Now()) {
		t.Fatal("no table")
	}
	if !strings.HasPrefix(out.String(), "Address ID") || strings.Contains(out.String(), "cus_parent") || strings.Contains(out.String(), "\x1b") || strings.Count(out.String(), "\n") != 2 {
		t.Fatalf("bad table: %q", out.String())
	}
}

func TestHumanOrdersShowNumberPaymentTotalAndBuyer(t *testing.T) {
	var out bytes.Buffer
	rows := []any{map[string]any{"order_id": "ord_customer", "customer_id": "cus_fallback"}, map[string]any{"order_id": "ord_expanded", "customer": map[string]any{"email": "expanded@example.test"}}, map[string]any{"order_id": "ord_example", "order_number": "1010", "status": "closed", "payment_status": "paid", "pricing_amounts": map[string]any{"total_money": map[string]any{"amount": 3570, "currency": "USD"}}, "buyer_contact": map[string]any{"email": "buyer@example.test"}}}
	if !renderResourceTable(&out, rows, &Command{CanonicalName: "orders.list", OperationID: "listOrders"}, time.Now()) {
		t.Fatal("no table")
	}
	for _, want := range []string{"1010", "paid", "35.70 USD", "buyer@example.test", "cus_fallback", "expanded@example.test"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, &out)
		}
	}
}

func TestHumanTaxRegistrationsIdentifyJurisdictionAndDates(t *testing.T) {
	rows := []any{
		map[string]any{"tax_registration_id": "trg_us", "country": "US", "state": "NY", "status": "active", "registration_number_last4": "1234", "start_date": "2026-01-01", "end_date": "2026-12-31"},
		map[string]any{"tax_registration_id": "trg_gb", "country": "GB", "state": nil, "status": "scheduled", "registration_number_last4": "5678", "start_date": "2027-01-01", "end_date": nil},
	}
	envelope := map[string]any{"data": rows}
	before, _ := json.Marshal(envelope)
	var out bytes.Buffer
	if e := renderHuman(&out, envelope, &Command{CanonicalName: "tax-connection.registrations.list", OperationID: "listTaxRegistrations"}, time.Now()); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"Country", "State", "Registration number last4", "Start date", "End date", "US", "NY", "GB", "1234", "5678", "2026-01-01", "2026-12-31", "2027-01-01"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, &out)
		}
	}
	if strings.Count(out.String(), "\n") != 3 || strings.Contains(out.String(), "<nil>") {
		t.Fatalf("not a compact table: %s", &out)
	}
	after, _ := json.Marshal(envelope)
	if !bytes.Equal(before, after) {
		t.Fatal("rendering mutated JSON")
	}
}

func TestHumanTaxConnectionLeadsWithHealthAndSummarizesCoverage(t *testing.T) {
	coverage := []any{map[string]any{"country": "US", "category_scope": "all"}}
	for _, country := range []string{"CA", "GB", "AU"} {
		coverage = append(coverage, map[string]any{"country": country, "category_scope": "digital"})
	}
	for _, status := range []string{"active", "not_connected", "attention_required"} {
		t.Run(status, func(t *testing.T) {
			data := map[string]any{"status": status, "status_reason": "registration_required", "supported_actions": []any{"connect"}, "coverage": coverage, "legal_business_name": "Example"}
			envelope := map[string]any{"data": data}
			before, _ := json.Marshal(envelope)
			var out bytes.Buffer
			if e := renderHuman(&out, envelope, &Command{CanonicalName: "tax-connection.get", OperationID: "getTaxConnection"}, time.Now()); e != nil {
				t.Fatal(e)
			}
			text := out.String()
			last := -1
			for _, want := range []string{"Status ", "Status reason", "Supported actions", "Coverage", "Legal business name"} {
				at := strings.Index(text, want)
				if at <= last {
					t.Fatalf("fields out of order (%s): %s", want, text)
				}
				last = at
			}
			for _, want := range []string{status, "registration_required", "connect", "US: all; 3 countries: digital only"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
			if strings.Contains(text, "Category scope") {
				t.Fatalf("coverage not summarized: %s", text)
			}
			after, _ := json.Marshal(envelope)
			if !bytes.Equal(before, after) {
				t.Fatal("rendering mutated JSON")
			}
		})
	}
}

func TestHumanTaxCoverageSummaryEdges(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
		ok    bool
	}{
		{[]any{}, "(none)", true},
		{[]any{map[string]any{"country": "CA", "category_scope": "digital"}}, "CA: digital only", true},
		{[]any{map[string]any{"country": "US", "category_scope": "future_scope"}}, "US: future_scope", true},
		{[]any{map[string]any{"country": "US"}}, "", false},
		{nil, "", false},
	} {
		got, ok := taxCoverageSummary(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%v: %q, %v; want %q, %v", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

func TestHumanCustomerTaxExemptionsIdentifyCertificates(t *testing.T) {
	rows := []any{
		map[string]any{"customer_id": "cus_example", "customer_tax_exemption_id": "ctex_resale", "reason": "resale", "coverage": map[string]any{"type": "states", "country": "US", "states": []any{"TX", "NY"}}, "certificate_number": "SYNTHETIC-42", "start_date": "2026-01-01", "end_date": "2027-12-31", "status": "active"},
		map[string]any{"customer_id": "cus_example", "customer_tax_exemption_id": "ctex_government", "reason": "federal_government", "coverage": map[string]any{"type": "all_states", "country": "US"}, "certificate_number": nil, "start_date": "2027-01-01", "end_date": nil, "status": "scheduled"},
	}
	envelope := map[string]any{"data": rows}
	before, _ := json.Marshal(envelope)
	var out bytes.Buffer
	if e := renderHuman(&out, envelope, &Command{CanonicalName: "customers.tax-exemptions.list", OperationID: "listCustomerTaxExemptions"}, time.Now()); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"Customer tax exemption ID", "Reason", "Coverage", "Certificate number", "Start date", "End date", "Status", "ctex_resale", "ctex_government", "resale", "federal_government", "TX, NY", "All states", "SYNTHETIC-42", "2026-01-01", "2027-12-31", "scheduled"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, &out)
		}
	}
	if strings.Contains(out.String(), "cus_example") || strings.Contains(out.String(), "<nil>") || strings.Count(out.String(), "\n") != 3 {
		t.Fatal(&out)
	}
	after, _ := json.Marshal(envelope)
	if !bytes.Equal(before, after) {
		t.Fatal("rendering mutated JSON")
	}
}
