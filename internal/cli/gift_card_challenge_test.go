package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestEmbeddedCheckoutOriginJSONInput(t *testing.T) {
	for _, fixture := range []struct {
		command, resourceID, path string
	}{
		{"checkout-sessions.create", "", "/v1/checkout-sessions"},
		{"invoices.checkout-sessions.create", "inv_test", "/v1/invoices/inv_test/checkout-sessions"},
		{"me.invoices.checkout-sessions.create", "inv_test", "/v1/me/invoices/inv_test/checkout-sessions"},
		{"return-resolutions.checkout-sessions.create", "rres_test", "/v1/return-resolutions/rres_test/checkout-sessions"},
		{"me.return-resolutions.checkout-sessions.create", "rres_test", "/v1/me/return-resolutions/rres_test/checkout-sessions"},
	} {
		t.Run(fixture.command, func(t *testing.T) {
			body := map[string]any{"surface": "embedded", "page_origin": "https://shop.example.com"}
			if fixture.resourceID == "" {
				body["type"] = "order"
				body["order_id"] = "ord_test"
			}
			var sent map[string]any
			calls := 0
			const challengeURL = "https://checkout.example.com/gift-card-challenge/opaque"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				calls++
				if r.Method != http.MethodPost || r.URL.Path != fixture.path {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
					t.Errorf("decode request: %v", err)
				}
				fmt.Fprintf(w, `{"data":{"checkout_session":{"checkout_session_id":"cs_test","surface":"embedded","page_origin":"https://shop.example.com","gift_card_challenge":{"url":%q}}}}`, challengeURL)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			command, ok := app.Registry.ByName(fixture.command)
			if !ok {
				t.Fatal("command missing")
			}
			arguments := deepCopyMap(body)
			argv := append([]string(nil), command.Path[1:]...)
			if fixture.resourceID != "" {
				argv = append(argv, fixture.resourceID)
				for _, argument := range command.Arguments {
					if argument.Positional == 1 {
						arguments[argument.Name] = fixture.resourceID
					}
				}
			}
			if err := validateMCPArguments(command, arguments); err != nil {
				t.Fatalf("embedded origin rejected by exported input schema: %v", err)
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			app.Stdin = strings.NewReader(string(raw))
			argv = append(argv, "--input", "-", "--output", "json")
			if exit := app.Run(argv); exit != ExitOK || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
			}
			if calls != 1 || !reflect.DeepEqual(sent, body) {
				t.Fatalf("calls=%d body=%#v, want %#v", calls, sent, body)
			}
			if !strings.Contains(out.String(), challengeURL) || !strings.Contains(out.String(), `"page_origin":"https://shop.example.com"`) {
				t.Fatalf("checkout challenge fields missing from output: %s", out)
			}
		})
	}
}

func TestCustomerAccountJSONInputPreservesSparseUpdates(t *testing.T) {
	for _, input := range []string{
		`{"expected_version":7,"customer_account":null}`,
		`{"expected_version":7}`,
		`{"expected_version":7,"customer_account":{"mode":"flint_hosted"}}`,
	} {
		t.Run(input, func(t *testing.T) {
			var expected, sent map[string]any
			if err := json.Unmarshal([]byte(input), &expected); err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				calls++
				if r.Method != http.MethodPatch || r.URL.Path != "/v1/settings" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
					t.Errorf("decode request: %v", err)
				}
				fmt.Fprint(w, `{"data":{"settings_id":"set_test","settings_scope":"merchant","version":8}}`)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			command, _ := app.Registry.ByName("settings.update")
			if err := validateMCPArguments(command, expected); err != nil {
				t.Fatalf("sparse settings input rejected: %v", err)
			}
			app.Stdin = strings.NewReader(input)
			if exit := app.Run([]string{"settings", "update", "--input", "-", "--output", "json"}); exit != ExitOK || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
			}
			if calls != 1 || !reflect.DeepEqual(sent, expected) {
				t.Fatalf("calls=%d body=%#v, want %#v", calls, sent, expected)
			}
		})
	}
}

func TestGiftCardChallengeAndAccountOutputSchemaBoundaries(t *testing.T) {
	registry := NewRegistry()
	checkout, _ := registry.ByName("checkout-sessions.get")
	checkoutSchema, schemaErr := schemaForCommand(checkout, false)
	if schemaErr != nil {
		t.Fatal(schemaErr)
	}
	definitions := checkoutSchema["$defs"].(map[string]any)
	session := definitions["CheckoutSession"].(map[string]any)
	challenge := session["properties"].(map[string]any)["gift_card_challenge"].(map[string]any)
	validateExportedPropertyFixtures(t, challenge, definitions,
		[]any{map[string]any{"url": "https://checkout.example.com/gift-card-challenge/opaque"}},
		[]any{nil, map[string]any{}, map[string]any{"url": true}, map[string]any{"url": "https://example.com", "extra": true}})

	settings, _ := registry.ByName("settings.get")
	settingsSchema, schemaErr := schemaForCommand(settings, false)
	if schemaErr != nil {
		t.Fatal(schemaErr)
	}
	definitions = settingsSchema["$defs"].(map[string]any)
	settingsDefinition := definitions["Settings"].(map[string]any)
	account := settingsDefinition["properties"].(map[string]any)["customer_account"].(map[string]any)
	validateExportedPropertyFixtures(t, account, definitions,
		[]any{map[string]any{"mode": "flint_hosted"}}, []any{nil})
}

func TestGiftCardChallengeErrorPreservesRemediation(t *testing.T) {
	for _, reason := range []string{"proof_required", "proof_rejected", "page_origin_required"} {
		t.Run(reason, func(t *testing.T) {
			apiError := map[string]any{
				"type": "validation_error", "code": "GIFT_CARD_CHALLENGE_REQUIRED",
				"message": "Synthetic challenge error", "param": "Flint-Gift-Card-Challenge",
				"reason": reason, "retryable": false,
			}
			if reason != "page_origin_required" {
				apiError["remediation"] = map[string]any{"next_actions": []any{map[string]any{
					"action_type": "complete_gift_card_challenge",
					"url":         "https://checkout.example.com/gift-card-challenge/opaque",
				}}}
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/orders/ord_test/gift-cards" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": apiError})
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_ACCESS_TOKEN", "ckat_v1.x")
			app.Stdin = strings.NewReader(`{"code":"synthetic-code","expected_version":7}`)
			if exit := app.Run([]string{"orders", "gift-cards", "create", "ord_test", "--input", "-", "--output", "json"}); exit != ExitAPI || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
			}
			var output map[string]any
			if err := json.Unmarshal(out.Bytes(), &output); err != nil {
				t.Fatal(err)
			}
			actual := output["error"].(map[string]any)
			if key, _ := lookupPath(actual, "details.idempotency_key"); !strings.HasPrefix(fmt.Sprint(key), "flint-cli-") {
				t.Fatalf("key missing from challenge error: %s", out)
			}
			delete(actual, "details")
			// No retry policy was supplied, so retain the server's remediation
			// exactly without adding advice about resending this request.
			apiError["http_status"] = float64(http.StatusBadRequest)
			if calls != 1 || !reflect.DeepEqual(actual, apiError) {
				t.Fatalf("calls=%d challenge error changed: %s", calls, out)
			}
		})
	}
}

func validateExportedPropertyFixtures(t *testing.T, property map[string]any, definitions map[string]any, valid, invalid []any) {
	t.Helper()
	schema := deepCopyMap(property)
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["$defs"] = definitions
	compiler := jsonschema.NewCompiler()
	const resourceURL = "https://example.com/cli-property-schema"
	if err := compiler.AddResource(resourceURL, schema); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(resourceURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range valid {
		if err := compiled.Validate(value); err != nil {
			t.Errorf("valid property value %#v rejected: %v", value, err)
		}
	}
	for _, value := range invalid {
		if err := compiled.Validate(value); err == nil {
			t.Errorf("invalid property value %#v accepted", value)
		}
	}
}
