package cli

import (
	"encoding/json"
	"testing"
)

func TestSingleMintSnapshot(t *testing.T) {
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.FlintCLIScopes) != 0 {
		t.Fatal("regression fixture must omit bootstrap scopes")
	}
	allowed := map[string]bool{}
	for _, scope := range embeddedAllowedCLIScopes() {
		allowed[scope] = true
	}
	for _, scope := range initialCLIScopes {
		if !allowed[scope] {
			t.Errorf("login requests unsupported scope %s", scope)
		}
	}
	registry := NewRegistry()
	for _, retired := range []string{"onboarding.api-key.create", "sandboxes.api-keys.create"} {
		if _, ok := registry.ByName(retired); ok {
			t.Errorf("retired command %s", retired)
		}
	}
	cmd, ok := registry.ByName("api-keys.create")
	if !ok || cmd.APIPath != "/v1/api-keys" {
		t.Fatal("missing single mint route")
	}
	found := false
	for _, arg := range cmd.Arguments {
		if arg.Flag == "--sandbox-id" && arg.BodyPath == "sandbox_id" {
			found = true
		}
	}
	if !found {
		t.Fatal("sandbox_id has no generated flag")
	}
}

func TestContextModeCompatibility(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"mode":"test"}`, "sandbox"},
		{`{"mode":"live","environment":"sandbox"}`, "live"},
		{`{"environment":"sandbox"}`, "sandbox"},
	} {
		var c authorizedContext
		if err := json.Unmarshal([]byte(tc.raw), &c); err != nil {
			t.Fatal(err)
		}
		if c.Environment != tc.want {
			t.Fatalf("%s: %+v", tc.raw, c)
		}
	}
}

func TestCredentialAuthContextVariants(t *testing.T) {
	for _, kind := range []string{"onboarding_session", "partner_app"} {
		var envelope authContextEnvelope
		if err := json.Unmarshal([]byte(`{"data":{"auth_type":"`+kind+`","mode":"test","merchant_id":"mer_example","access":"full"}}`), &envelope); err != nil {
			t.Fatal(err)
		}
		auth := envelope.Data
		if auth.AuthType != kind || auth.Environment != "sandbox" || auth.Access != "full" || len(auth.Scopes) != 0 {
			t.Fatalf("%+v", auth)
		}
	}
}
