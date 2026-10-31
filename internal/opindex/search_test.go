package opindex

import (
	"reflect"
	"sync"
	"testing"
)

func TestSearchPrefixOrdering(t *testing.T) {
	ix := &Index{Entries: []Entry{
		{ID: "orders.line-items.delete", Type: "operation"},
		{ID: "orders.line-items.update", Type: "operation"},
		{ID: "orders.line-items.create", Type: "operation"},
		{ID: "orders.line-items.get", Type: "operation"},
		{ID: "orders.line-items.list", Type: "operation"},
		{ID: "orders.line-items.adjust", Type: "operation"},
		{ID: "orders.line-items.modifiers.list", Type: "operation"},
		{ID: "unrelated.list", Type: "operation", Title: "Orders line items"},
	}}
	want := []string{"orders.line-items.list", "orders.line-items.get", "orders.line-items.create", "orders.line-items.update", "orders.line-items.adjust", "orders.line-items.delete", "orders.line-items.modifiers.list", "unrelated.list"}
	for range 5 {
		hits, total := ix.Search("orders.line-items", Filter{})
		if got := searchHitIDs(hits); !reflect.DeepEqual(got, want) || total != len(want) {
			t.Fatalf("got %v, total %d; want %v", got, total, want)
		}
	}
	hits, _ := ix.Search("orders.line-items.delete", Filter{})
	if hits[0].ID != "orders.line-items.delete" {
		t.Fatalf("exact ID did not rank first: %v", searchHitIDs(hits))
	}
}

func TestSearchMethodPath(t *testing.T) {
	ix := &Index{Entries: []Entry{
		{ID: "refunds.list", Type: "operation", Method: "GET", Path: "/v1/refunds", Title: "List refunds"},
		{ID: "refunds.create", Type: "operation", Method: "POST", Path: "/v1/refunds", Title: "Create a refund"},
		{ID: "refunds.get", Type: "operation", Method: "GET", Path: "/v1/refunds/{refund_id}"},
		{ID: "other.create", Type: "operation", Method: "POST", Path: "/v1/other", Title: "Post refunds"},
	}}
	for _, query := range []string{"POST /v1/refunds", " post   /v1/refunds ", "GET /v1/refunds/{refund_id}"} {
		hits, _ := ix.Search(query, Filter{})
		want := "refunds.create"
		if query == "GET /v1/refunds/{refund_id}" {
			want = "refunds.get"
		}
		if len(hits) == 0 || hits[0].ID != want || hits[0].Score != 200 {
			t.Errorf("%q: got %v, want exact route %s", query, searchHitIDs(hits), want)
		}
	}
}

func TestSearchFilterAndLimit(t *testing.T) {
	ix := &Index{Entries: []Entry{
		{ID: "items.list", Type: "operation", Kind: "read", MCP: true},
		{ID: "items.get", Type: "operation", Kind: "read"},
		{ID: "items.create", Type: "operation", Kind: "write", MCP: true},
		{ID: "item.created", Type: "event", MCP: true},
		{ID: "items", Type: "topic", MCP: true},
	}}
	hits, total := ix.Search("items", Filter{Kind: "read", Limit: 1})
	if got := searchHitIDs(hits); !reflect.DeepEqual(got, []string{"items.list"}) || total != 2 {
		t.Fatalf("kind/limit: got %v, total %d", got, total)
	}
	hits, total = ix.Search("items", Filter{Exposed: func(e Entry) bool { return e.MCP }, Limit: 2})
	if len(hits) != 2 || total != 4 {
		t.Fatalf("exposure/limit: got %v, total %d", searchHitIDs(hits), total)
	}
	for _, hit := range hits {
		if !hit.MCP {
			t.Fatalf("unexposed result: %+v", hit)
		}
	}
	for i := 0; i < 12; i++ {
		ix.Entries = append(ix.Entries, Entry{ID: "items.extra." + string(rune('a'+i)), Type: "operation"})
	}
	hits, total = ix.Search("items", Filter{})
	if len(hits) != 10 || total != 17 {
		t.Fatalf("default limit: %d hits, total %d", len(hits), total)
	}
	for _, query := range []string{"", "the and my", "zzzxxyy"} {
		hits, total = ix.Search(query, Filter{})
		if len(hits) != 0 || total != 0 {
			t.Errorf("%q: got %v, total %d", query, searchHitIDs(hits), total)
		}
	}
}

func TestSearchWordsAndSynonyms(t *testing.T) {
	ix := &Index{Entries: []Entry{
		{ID: "customers.create", Type: "operation", Title: "Create a customer"},
		{ID: "customer-deletion-requests.create", Type: "operation", Title: "Create customer deletion request"},
		{ID: "webhook-deliveries.list", Type: "operation", Title: "List webhook deliveries"},
		{ID: "auth.status", Type: "operation", Title: "Show current credential", Aliases: []string{"whoami"}},
		{ID: "orders.line-items.list", Type: "operation", Title: "List order line items"},
		{ID: "orders.list", Type: "operation", Title: "List orders"},
		{ID: "inventory-levels.list", Type: "operation", Title: "List inventory levels"},
		{ID: "promotion-codes.list", Type: "operation", Title: "List promotion codes"},
		{ID: "test-cards", Type: "topic", Title: "Sandbox payment tokens and test card numbers"},
	}}
	for query, want := range map[string]string{
		"make a customer":           "customers.create",
		"create customers":          "customers.create",
		"browse webhook deliveries": "webhook-deliveries.list",
		"who am i":                  "auth.status",
		"whoami":                    "auth.status",
		"line item":                 "orders.line-items.list",
		"stock":                     "inventory-levels.list",
		"coupon":                    "promotion-codes.list",
		"test card":                 "test-cards",
	} {
		hits, _ := ix.Search(query, Filter{})
		if len(hits) == 0 || hits[0].ID != want {
			t.Errorf("%q: got %v, want %s first", query, searchHitIDs(hits), want)
		}
	}
	if got, want := searchWords("addresses deliveries classes boxes matches dishes glass"), []string{"address", "delivery", "class", "box", "match", "dish", "glass"}; !reflect.DeepEqual(got, want) {
		t.Errorf("singularization: got %v, want %v", got, want)
	}
}

func TestSearchEventIntent(t *testing.T) {
	ix := &Index{Entries: []Entry{
		{ID: "payment-intents.get", Type: "operation", Title: "Get a payment intent"},
		{ID: "payment_intent.succeeded", Type: "event", Title: "Payment succeeded"},
	}}
	for query, want := range map[string]string{"payment": "payment-intents.get", "payment succeeded event": "payment_intent.succeeded"} {
		hits, _ := ix.Search(query, Filter{})
		if len(hits) == 0 || hits[0].ID != want {
			t.Errorf("%q: got %v, want %s first", query, searchHitIDs(hits), want)
		}
	}
}

func searchHitIDs(hits []Hit) []string {
	ids := make([]string, len(hits))
	for i, hit := range hits {
		ids[i] = hit.ID
	}
	return ids
}

func TestSearchConcurrentInitialization(t *testing.T) {
	index, err := New([]Command{{Name: "customers.list", OperationID: "listCustomers"}}, []byte(`{"paths":{"/v1/customers":{"get":{"operationId":"listCustomers","summary":"List customers"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if index.documents != nil {
		t.Fatal("search documents built before search")
	}
	var readers sync.WaitGroup
	for range 16 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			hits, _ := index.Search("customers", Filter{})
			if len(hits) == 0 || hits[0].ID != "customers.list" {
				t.Error("concurrent search lost operation")
			}
		}()
	}
	readers.Wait()
	if len(index.documents) == 0 {
		t.Fatal("search documents were not cached")
	}
}
