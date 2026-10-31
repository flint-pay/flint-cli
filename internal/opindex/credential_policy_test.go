package opindex

import "testing"

func TestCredentialPolicyIncludesBoundAndRawRoutes(t *testing.T) {
	document := []byte(`{"paths":{"/v1/auth-context":{"get":{"operationId":"getAuthContext","security":[{"BearerAuth":[]}]}},"/v1/raw-only":{"get":{"operationId":"rawOnly","security":[{"CustomerSessionBearer":[]}]}}}}`)
	index, err := New([]Command{{Name: "auth.status", OperationID: "getAuthContext"}}, document)
	if err != nil {
		t.Fatal(err)
	}
	discovery, ok := index.Lookup("auth.status")
	if !ok || discovery.Audience != "local" {
		t.Fatalf("bound discovery changed: %#v", discovery)
	}
	merchant := ClassifyCredential(CredentialAPIKey, "")
	buyer := ClassifyCredential(CredentialAccessToken, "flint_cses_example")
	for _, tc := range []struct {
		operationID, audience string
		merchant, buyer       bool
	}{
		{"getAuthContext", "merchant", true, false},
		{"rawOnly", "buyer", false, true},
	} {
		policy, ok := index.CredentialPolicy(tc.operationID)
		if !ok || policy.Audience != tc.audience || merchant.Allows(policy) != tc.merchant || buyer.Allows(policy) != tc.buyer {
			t.Errorf("%s: %#v", tc.operationID, policy)
		}
	}
	if _, ok := index.CredentialPolicy("unknown"); ok {
		t.Fatal("invented policy for unknown route")
	}
}

func TestOperationSafetyIncludesRawOnlyRoutes(t *testing.T) {
	document := []byte(`{"paths":{"/v1/things":{"post":{"operationId":"createThing","x-flint-route-class":"sensitive_write"}},"/v1/things/{id}/void":{"post":{"operationId":"voidThing"}},"/v1/things/{id}":{"patch":{"operationId":"updateThing","x-flint-route-class":"standard_write"}},"/v1/oauth/authorize":{"get":{"operationId":"authorizePartnerInstall","x-flint-route-class":"sensitive_write"}}}}`)
	index, err := New([]Command{{Name: "things.create", OperationID: "createThing", Mutation: true}}, document)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id    string
		gated bool
	}{{"createThing", true}, {"voidThing", true}, {"updateThing", false}, {"authorizePartnerInstall", true}} {
		entry, ok := index.OperationSafety(tc.id)
		if !ok || entry.Kind != "write" || entry.ConfirmRequired() != tc.gated {
			t.Fatalf("%s: %+v", tc.id, entry)
		}
	}
	exposed, _ := index.Lookup("things.create")
	route, _ := index.OperationSafety("createThing")
	if exposed.Sensitive != route.Sensitive || exposed.Irreversible != route.Irreversible {
		t.Fatal(exposed, route)
	}
	if _, ok := index.OperationSafety("unknown"); ok {
		t.Fatal("unknown operation acquired safety")
	}
}
