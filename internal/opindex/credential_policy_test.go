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
