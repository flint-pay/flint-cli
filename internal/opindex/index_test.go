package opindex

import (
	"reflect"
	"testing"
)

func TestCredentialAdapter(t *testing.T) {
	for _, tc := range []struct {
		source, token, scheme string
		partner               bool
	}{
		{CredentialAPIKey, "anything", "BearerAuth", false}, {CredentialLogin, "anything", "BearerAuth", false}, {CredentialWorkload, "anything", "BearerAuth", false},
		{CredentialAccessToken, "fcat_example", "BearerAuth", false}, {CredentialAccessToken, "fwat_example", "BearerAuth", false}, {CredentialAccessToken, "fpat_v1.example", "BearerAuth", true},
		{CredentialAccessToken, "ckat_v1.example", "CheckoutSessionBearer", false}, {CredentialAccessToken, "flint_cses_example", "CustomerSessionBearer", false}, {CredentialAccessToken, "unknown", "", false},
	} {
		c := ClassifyCredential(tc.source, tc.token)
		if c.Partner != tc.partner || tc.scheme != "" && !reflect.DeepEqual(c.Schemes, []string{tc.scheme}) || tc.scheme == "" && c.Schemes != nil {
			t.Errorf("%+v: %+v", tc, c)
		}
	}
	merchant := Entry{Audience: "merchant", Schemes: []string{"BearerAuth"}}
	buyer := Entry{Audience: "buyer", Schemes: []string{"CustomerSessionBearer"}}
	partner := ClassifyCredential(CredentialAccessToken, "fpat_v1.example")
	if partner.Allows(merchant) || partner.Allows(buyer) {
		t.Fatal("partner escaped install restriction")
	}
	merchant.PartnerAccess = true
	if !partner.Allows(merchant) || !partner.Allows(Entry{Audience: "local"}) {
		t.Fatal("partner lost local/install access")
	}
	if !ClassifyCredential(CredentialAccessToken, "new-format").Allows(buyer) {
		t.Fatal("unknown token filtered")
	}
	if ClassifyCredential(CredentialAPIKey, "key").Allows(buyer) {
		t.Fatal("merchant key accepted buyer-only op")
	}
	for _, tc := range []struct {
		schemes []string
		want    string
	}{{nil, "keyless"}, {[]string{"CustomerSessionBearer", "CheckoutSessionBearer"}, "buyer"}, {[]string{"CustomerSessionBearer", "BearerAuth"}, "merchant"}} {
		if got := audience(tc.schemes); got != tc.want {
			t.Errorf("%v: %s", tc.schemes, got)
		}
	}
}

func TestIndexEffectsScopesAndRecovery(t *testing.T) {
	doc := []byte(`{"paths":{"/v1/things":{"get":{"operationId":"list","security":[{"BearerAuth":[]}],"parameters":[{"in":"query","name":"idempotency_key"}]},"post":{"operationId":"create","security":[{"BearerAuth":[]}],"parameters":[{"in":"header","name":"Idempotency-Key","required":true}],"x-flint-route-class":"payment","x-flint-required-scopes":["things.read","things.write"]}},"/v1/things/{id}":{"patch":{"operationId":"update","security":[{"BearerAuth":[]}]},"delete":{"operationId":"delete","x-flint-destructive":true,"security":[{"BearerAuth":[]}]}}},"x-flint-scopes":[{"name":"things.read","implied_by":["things.write"]},{"name":"things.write","implied_by":["admin"]}]}`)
	ix, err := New([]Command{{Name: "things.list", OperationID: "list"}, {Name: "things.create", OperationID: "create"}, {Name: "things.update", OperationID: "update"}, {Name: "things.delete", OperationID: "delete"}, {Name: "listen", OperationID: "list"}, {Name: "history"}, {Name: "trigger"}}, doc)
	if err != nil {
		t.Fatal(err)
	}
	create, _ := ix.Lookup("things_create")
	update, _ := ix.Lookup("things update")
	del, _ := ix.Lookup("things.delete")
	listen, _ := ix.Lookup("listen")
	trigger, _ := ix.Lookup("trigger")
	clear, _ := ix.Lookup("history.clear")
	if create.Idempotency != "required" || !create.Sensitive || create.RecoveryRead != "things.list" || !create.ConfirmRequired() {
		t.Fatalf("create: %+v", create)
	}
	if !update.Overwrites || update.ConfirmRequired() || !del.Irreversible || !clear.Irreversible {
		t.Fatal("effect gates")
	}
	if listen.Kind != "write" || listen.Audience != "local" || !listen.MCP || trigger.Kind != "write" || !trigger.MCP {
		t.Fatal("local writes")
	}
	if got := ix.MissingScopes(create, []string{"admin"}, true); len(got) != 0 {
		t.Fatal(got)
	}
	if got := ix.MissingScopes(create, []string{"things.read"}, true); !reflect.DeepEqual(got, []string{"things.write"}) {
		t.Fatal(got)
	}
	if got := ix.MissingScopes(create, nil, false); got != nil {
		t.Fatal(got)
	}
	create.ScopeRequirement.Mode = "any"
	if got := ix.MissingScopes(create, []string{"things.read"}, true); got != nil {
		t.Fatal(got)
	}
	if got := ix.MissingScopes(create, nil, true); len(got) != 2 {
		t.Fatal(got)
	}
}

func TestIndexCollision(t *testing.T) {
	if _, err := New([]Command{{Name: "a-b.get"}, {Name: "a.b.get"}}, []byte(`{}`)); err == nil {
		t.Fatal("typed collision accepted")
	}
}
