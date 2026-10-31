package cli

import (
	"github.com/flint-pay/flint-cli/internal/opindex"
	"strings"
	"testing"
)

func TestOperationIndexClassification(t *testing.T) {
	a := &App{Registry: NewRegistry()}
	ix, err := a.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	exposed := 0
	events := 0
	credential := opindex.ClassifyCredential(opindex.CredentialAPIKey, "")
	for _, entry := range ix.Entries {
		if entry.Type == "event" {
			events++
			continue
		}
		if entry.Type != "operation" {
			continue
		}
		counts[entry.Audience]++
		if strings.Contains(entry.ID, "_") || len(opindex.ToolName(entry.ID)) > 52 {
			t.Errorf("invalid name %s", entry.ID)
		}
		if entry.MCP && credential.Allows(entry) {
			exposed++
		}
		if entry.MCP {
			hits, _ := ix.Search(entry.ID, opindex.Filter{Exposed: func(e opindex.Entry) bool { return e.MCP }})
			if len(hits) == 0 || hits[0].ID != entry.ID {
				t.Errorf("unreachable %s", entry.ID)
			}
		}
	}
	trigger, ok := ix.Lookup("trigger")
	if !ok || !trigger.MCP || trigger.Kind != "write" || trigger.Audience != "local" {
		t.Fatalf("trigger: %+v", trigger)
	}
	t.Logf("audiences=%v merchant-exposed=%d events=%d bridge=%d/%d", counts, exposed, events, ix.PublishedDestructive, ix.DerivedDestructive)
	if events != 251 || counts["buyer"] != 70 || counts["merchant"] != 531 || counts["local"] != 32 || counts["keyless"] != 8 || exposed != 541 {
		t.Errorf("classification changed: %v events=%d", counts, events)
	}
	if ix.DerivedDestructive == 0 || ix.PublishedDestructive == ix.DerivedDestructive {
		t.Fatal("contract now marks every derived destructive operation; remove the bridge")
	}
	for _, name := range []string{"api", "schema.input", "help", "mcp.serve", "config.set"} {
		e, ok := ix.Lookup(name)
		if !ok || e.MCP {
			t.Errorf("hidden %s: %+v", name, e)
		}
	}
}

func TestSearchGolden(t *testing.T) {
	ix, err := (&App{Registry: NewRegistry()}).operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	// Search uses the same route-derived names as the terminal registry.
	for _, tc := range []struct {
		q, want string
		top     int
	}{
		{"refund an order", "refunds.create", 1}, {"refund", "refunds.list", 1}, {"refund status", "refunds.list", 1}, {"refund", "refunds.create", 3}, {"refund status", "refunds.create", 3}, {"orders", "orders.list", 1}, {"today orders", "orders.list", 1},
		{"create customer", "customers.create", 1}, {"cancel payment intent", "payment-intents.cancel", 1}, {"capture payment", "payment-intents.capture", 1}, {"pay order", "orders.pay", 1},
		{"webhook deliveries", "webhook-deliveries.list", 1}, {"resend webhook", "webhook-deliveries.resend", 1}, {"rotate webhook secret", "webhook-endpoints.rotate-secret", 1}, {"delete webhook endpoint", "webhook-endpoints.delete", 1},
		{"payment succeeded event", "payment_intent.succeeded", 1}, {"test card", "test-cards", 1}, {"who am i", "auth.status", 1}, {"timeline of a payment", "resource-timelines.get", 1}, {"reset sandbox", "sandboxes.reset", 1},
		{"api request logs", "api-request-logs.list", 1}, {"gift card balance", "gift-cards.adjust-balance", 2}, {"discount code", "promotion-codes.list", 1},
		{"forward webhooks to localhost", "listen", 1}, {"exit codes", "exit-codes", 1}, {"api schema for a command", "schema.input", 3}, {"POST /v1/refunds", "refunds.create", 1},
	} {
		t.Run(tc.q, func(t *testing.T) {
			hits, _ := ix.Search(tc.q, opindex.Filter{Exposed: func(e opindex.Entry) bool { return e.ID != "safety" }})
			found := false
			ids := []string{}
			for i, h := range hits {
				ids = append(ids, h.ID)
				if i < tc.top && h.ID == tc.want {
					found = true
				}
			}
			if !found {
				t.Errorf("want %s top %d; %v", tc.want, tc.top, ids)
			}
		})
	}
	hits, _ := ix.Search("orders.line-items", opindex.Filter{})
	if len(hits) < 3 {
		t.Fatal(hits)
	}
	for _, h := range hits[:3] {
		if !strings.HasPrefix(h.ID, "orders.line-items.") {
			t.Fatal(hits)
		}
	}
}

func TestOperationIndexCacheRespectsRegistryMetadata(t *testing.T) {
	first := &App{Registry: NewRegistry()}
	second := &App{Registry: NewRegistry()}
	original, err := first.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	shared, err := second.operationIndex()
	if err != nil || original != shared {
		t.Fatalf("identical registries did not reuse the contract: %v", err)
	}
	command, ok := second.Registry.ByName("customers.list")
	if !ok {
		t.Fatal("missing customers.list")
	}
	command.Mutation = true
	changed, err := second.operationIndex()
	if err != nil || changed == original {
		t.Fatalf("changed registry reused stale metadata: %v", err)
	}
	before, _ := original.Lookup("customers.list")
	after, _ := changed.Lookup("customers.list")
	if before.Kind != "read" || after.Kind != "write" {
		t.Fatalf("classification before=%s after=%s", before.Kind, after.Kind)
	}
}
