package cli

import (
	"fmt"
	"strings"
	"testing"
)

func TestSubscriptionQuantityFlags(t *testing.T) {
	for _, name := range []string{"subscriptions.change-quantity", "me.subscriptions.change-quantity", "subscriptions.change-plan", "me.subscriptions.change-plan", "subscriptions.update", "me.subscriptions.update", "subscription-plans.create", "subscription-plans.update", "subscription-previews.create", "me.subscription-previews.create", "checkout-sessions.create", "payment-links.create", "payment-links.resolve"} {
		t.Run(name, func(t *testing.T) {
			cmd, ok := NewRegistry().ByName(name)
			if !ok {
				t.Fatal("missing command", name)
			}
			for _, arg := range cmd.Arguments {
				if arg.Flag == "--quantity" || strings.Contains(arg.Flag, "quantity-options") {
					t.Fatalf("retired multiplier flag: %s", arg.Flag)
				}
			}
		})
	}
}

func TestSubscriptionChangeQuantityLineItems(t *testing.T) {
	for _, buyer := range []bool{false, true} {
		t.Run(fmt.Sprint(buyer), func(t *testing.T) {
			app, out, _ := testApp(t, "http://127.0.0.1:1")
			prefix := []string{"subscriptions", "change-quantity", "sub_test"}
			if buyer {
				t.Setenv("FLINT_ACCESS_TOKEN", "flint_cses_test")
				prefix = append([]string{"me"}, prefix...)
			}
			args := append(prefix, "--billing-starts", "period_end", "--line-item", "sli_01KEZF7C00PPWVJ2N321V84ASA=137", "--line-item", "sli_01KEZF7C00PPWVJ2N321V84ASB=2", "--idempotency-key", "quantity-test", "--dry-run=client")
			exit, result := runJSON(t, app, out, args...)
			if exit != ExitOK {
				t.Fatalf("exit=%d result=%v", exit, result)
			}
			for path, want := range map[string]any{"data.body.line_items.0.subscription_line_item_id": "sli_01KEZF7C00PPWVJ2N321V84ASA", "data.body.line_items.0.quantity": float64(137), "data.body.line_items.1.quantity": float64(2), "data.body.billing_starts": "period_end"} {
				if got := lookupPathValue(result, path); got != want {
					t.Errorf("%s=%v want %v", path, got, want)
				}
			}
		})
	}
}

func TestSubscriptionChangeQuantityRejectsInvalidLines(t *testing.T) {
	for _, flags := range [][]string{
		{}, {"--quantity", "1"}, {"--line-item", "sli_test"}, {"--line-item", "sli_test=0"}, {"--line-item", "sli_test=10000"}, {"--line-item", "sli_test=1.5"}, {"--line-item", "sli_test=abc"},
		{"--line-item", "sli_01KEZF7C00PPWVJ2N321V84ASA=1", "--line-item", "sli_01KEZF7C00PPWVJ2N321V84ASA=2"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			app, out, _ := testApp(t, "http://127.0.0.1:1")
			args := append([]string{"subscriptions", "change-quantity", "sub_test", "--billing-starts", "period_end", "--dry-run=client", "--idempotency-key", "quantity-test"}, flags...)
			if exit, result := runJSON(t, app, out, args...); exit != ExitUsage {
				t.Fatalf("exit=%d result=%v", exit, result)
			}
		})
	}
}

func TestSubscriptionChangeQuantityLineLimitAndInputOverride(t *testing.T) {
	for _, count := range []int{1, 50, 51} {
		app, out, _ := testApp(t, "http://127.0.0.1:1")
		args := []string{"subscriptions", "change-quantity", "sub_test", "--billing-starts", "period_end", "--input", `{"line_items":[{"subscription_line_item_id":"sli_01KEZF7C00PPWVJ2N321V84ASA","quantity":999}]}`, "--dry-run=client", "--idempotency-key", "quantity-test"}
		for i := 0; i < count; i++ {
			args = append(args, "--line-item", fmt.Sprintf("sli_%026d=137", i))
		}
		exit, result := runJSON(t, app, out, args...)
		if count > 50 {
			if exit != ExitUsage {
				t.Fatalf("accepted %d lines: %v", count, result)
			}
			continue
		}
		if exit != ExitOK || lookupPathValue(result, "data.body.line_items.0.quantity") != float64(137) {
			t.Fatalf("count=%d exit=%d result=%v", count, exit, result)
		}
		lines, _ := lookupPathValue(result, "data.body.line_items").([]any)
		if len(lines) != count {
			t.Fatalf("got %d lines want %d", len(lines), count)
		}
	}
}
