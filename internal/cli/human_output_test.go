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
