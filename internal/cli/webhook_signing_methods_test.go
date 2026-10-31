package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// signingMethodFamily is a resource whose signing_methods set the CLI exposes
// through repeatable --signing-method flags.
type signingMethodFamily struct {
	group, id, path, idField string
	createArgs               []string
	createMCP                map[string]any
	secretFields             []string
	// scalarUpdate needs no expected_version; versionedUpdate replaces a
	// set or pinned configuration and therefore needs one.
	scalarUpdate, versionedUpdate []string
	// emptyUpdateValid reports whether a PATCH with no fields is accepted.
	emptyUpdateValid bool
}

var signingMethodFamilies = []signingMethodFamily{
	{
		group: "webhook-endpoints", id: "whep_test", path: "/v1/webhook-endpoints", idField: "webhook_endpoint_id",
		createArgs:       []string{"--url", "https://example.com/webhooks/flint", "--enabled-events", "payment_intent.succeeded"},
		createMCP:        map[string]any{"url": "https://example.com/webhooks/flint", "enabled_events": []any{"payment_intent.succeeded"}},
		secretFields:     []string{"secret"},
		scalarUpdate:     []string{"--url", "https://example.com/new"},
		versionedUpdate:  []string{"--enabled-events", "payment_intent.succeeded"},
		emptyUpdateValid: true,
	},
	{
		group: "delivery-rate-callbacks", id: "dcb_test", path: "/v1/delivery-rate-callbacks", idField: "delivery_rate_callback_id",
		createArgs:      []string{"--input", `{"name":"Synthetic rates","configuration":{"url":"https://example.com/delivery-rates"}}`},
		createMCP:       map[string]any{"name": "Synthetic rates", "configuration": map[string]any{"url": "https://example.com/delivery-rates"}},
		secretFields:    []string{"secret", "key_id"},
		scalarUpdate:    []string{"--input", `{"name":"Renamed rates"}`},
		versionedUpdate: []string{"--input", `{"configuration":{"url":"https://example.com/new"}}`},
	},
}

// secretNotIssuedJSON is the 409 Flint returns when hmac is added before a
// shared secret exists.
const secretNotIssuedJSON = `{"error":{"type":"conflict_error","code":"ACTION_NOT_ALLOWED","message":"This resource has no shared secret.","param":"signing_methods","details":[{"code":"ACTION_NOT_ALLOWED","reason":"secret_not_issued","param":"signing_methods","message":"This resource has no shared secret."}]}}`

func TestSigningMethodRequests(t *testing.T) {
	for _, family := range signingMethodFamilies {
		t.Run(family.group, func(t *testing.T) { testSigningMethodRequests(t, family) })
	}
}

func testSigningMethodRequests(t *testing.T, family signingMethodFamily) {
	group, id, path, createArgs, secretFields := family.group, family.id, family.path, family.createArgs, family.secretFields
	for _, tc := range []struct {
		name, action, input string
		flags               []string
		wantMethods         []any
		wantVersion         bool
		wantExit            int
	}{
		{name: "create defaults on server", action: "create"},
		{name: "create jwt", action: "create", flags: []string{"--signing-methods", "jwt"}, wantMethods: []any{"jwt"}},
		{name: "create hmac", action: "create", flags: []string{"--signing-methods", "hmac"}, wantMethods: []any{"hmac"}},
		{name: "create both", action: "create", flags: []string{"--signing-methods", "jwt", "--signing-methods", "hmac"}, wantMethods: []any{"jwt", "hmac"}},
		{name: "invalid method", action: "create", flags: []string{"--signing-methods", "rsa"}, wantExit: ExitUsage},
		{name: "duplicate method", action: "create", flags: []string{"--signing-methods", "jwt", "--signing-methods", "jwt"}, wantExit: ExitUsage},
		{name: "update scalar without version", action: "update", flags: family.scalarUpdate},
		{name: "update signing without version", action: "update", flags: []string{"--signing-methods", "jwt"}, wantExit: ExitUsage},
		{name: "update set or configuration without version", action: "update", flags: family.versionedUpdate, wantExit: ExitUsage},
		{name: "update signing with version", action: "update", flags: []string{"--signing-methods", "jwt", "--signing-methods", "hmac", "--expected-version", "3"}, wantMethods: []any{"jwt", "hmac"}, wantVersion: true},
		{name: "update input without version", action: "update", input: `{"signing_methods":["jwt"]}`, wantExit: ExitUsage},
		{name: "update input with version", action: "update", input: `{"signing_methods":["jwt"],"expected_version":3}`, wantMethods: []any{"jwt"}, wantVersion: true},
		{name: "secret not issued", action: "update", flags: []string{"--signing-methods", "hmac", "--expected-version", "3"}, wantMethods: []any{"hmac"}, wantVersion: true, wantExit: ExitAPI},
		{name: "empty set", action: "update", input: `{"signing_methods":[],"expected_version":3}`, wantExit: ExitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				wantMethod, wantPath := "POST", path
				if tc.action == "update" {
					wantMethod, wantPath = "PATCH", wantPath+"/"+id
				}
				if r.Method != wantMethod || r.URL.Path != wantPath {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if tc.name == "secret not issued" {
					w.WriteHeader(http.StatusConflict)
					fmt.Fprint(w, secretNotIssuedJSON)
					return
				}
				methods := tc.wantMethods
				if methods == nil {
					methods = []any{"jwt"}
				}
				data := map[string]any{family.idField: id, "signing_methods": methods, "version": 3}
				if tc.action == "create" && slices.Contains(methods, any("hmac")) {
					for _, field := range secretFields {
						data[field] = "synthetic-" + field
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer server.Close()
			app, out, errOut := testApp(t, server.URL)
			args := []string{group, tc.action}
			if tc.action == "create" {
				args = append(args, createArgs...)
			} else {
				args = append(args, id)
			}
			args = append(args, tc.flags...)
			if tc.input != "" {
				args = append(args, "--input", tc.input)
			}
			args = append(args, "--output", "json", "--idempotency-key", "synthetic-webhook-action")
			if exit := app.Run(args); exit != tc.wantExit {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, errOut)
			}
			if tc.wantExit == ExitAPI {
				if got == nil || !strings.Contains(out.String(), `"code":"ACTION_NOT_ALLOWED"`) || !strings.Contains(out.String(), `"reason":"secret_not_issued"`) || !strings.Contains(out.String(), `"http_status":409`) {
					t.Fatalf("missing conflict: stdout=%s stderr=%s", out, errOut)
				}
				return
			}
			if tc.wantExit != ExitOK {
				if got != nil {
					t.Fatal("invalid body reached endpoint")
				}
				return
			}
			if got == nil {
				t.Fatal("endpoint was not called")
			}
			methods, present := got["signing_methods"]
			if present != (tc.wantMethods != nil) || (present && !reflect.DeepEqual(methods, tc.wantMethods)) {
				t.Fatalf("signing_methods = %#v, want %#v", methods, tc.wantMethods)
			}
			if version, present := got["expected_version"]; present != tc.wantVersion || (present && version != float64(3)) {
				t.Fatalf("expected_version = %#v, present=%v", version, present)
			}
			if tc.action == "create" {
				hmac := slices.Contains(tc.wantMethods, any("hmac"))
				for _, field := range secretFields {
					if strings.Contains(out.String(), `"`+field+`"`) != hmac {
						t.Fatalf("%s present=%v with hmac=%v: stdout=%s", field, !hmac, hmac, out)
					}
				}
				if !hmac && strings.Contains(strings.ToLower(errOut.String()), "save") {
					t.Fatalf("unexpected secret advice: stderr=%s", errOut)
				}
			}
		})
	}
}

func TestSigningMethodMCPValidation(t *testing.T) {
	for _, family := range signingMethodFamilies {
		for _, action := range []string{"create", "update"} {
			testSigningMethodMCPValidation(t, family, action)
		}
	}
}

func testSigningMethodMCPValidation(t *testing.T, family signingMethodFamily, action string) {
	t.Helper()
	cmd, _ := NewRegistry().ByName(family.group + "." + action)
	for _, methods := range [][]any{nil, {"jwt"}, {"hmac"}, {"jwt", "hmac"}, {}, {"jwt", "jwt"}, {"unknown"}} {
		for _, withVersion := range []bool{false, true} {
			args := map[string]any{}
			if action == "create" {
				for key, value := range family.createMCP {
					args[key] = value
				}
			} else {
				args[family.idField] = family.id
				if withVersion {
					args["expected_version"] = float64(3)
				}
			}
			if methods != nil {
				args["signing_methods"] = methods
			}
			valid := methods == nil || reflect.DeepEqual(methods, []any{"jwt"}) || reflect.DeepEqual(methods, []any{"hmac"}) || reflect.DeepEqual(methods, []any{"jwt", "hmac"})
			valid = valid && (action == "create" || methods == nil || withVersion)
			valid = valid && (action == "create" || methods != nil || family.emptyUpdateValid)
			if err := validateMCPArguments(cmd, args); (err == nil) != valid {
				t.Errorf("%s.%s methods=%v version=%v valid=%v: %v", family.group, action, methods, withVersion, valid, err)
			}
		}
	}
}

func TestWebhookCreateOutputSchemaAllowsAbsentSecret(t *testing.T) {
	cmd, _ := NewRegistry().ByName("webhook-endpoints.create")
	schema, err := schemaForCommand(cmd, false)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("https://example.com/webhook-output", schema); err != nil {
		t.Fatal(err)
	}
	compiled, compileErr := compiler.Compile("https://example.com/webhook-output")
	if compileErr != nil {
		t.Fatal(compileErr)
	}
	for _, withSecret := range []bool{false, true} {
		body := map[string]any{"data": map[string]any{
			"webhook_endpoint_id": "whep_test", "api_version": "default", "enabled_events": []any{"payment_intent.succeeded"},
			"created_at": "2026-10-28T00:00:00Z", "updated_at": "2026-10-28T00:00:00Z",
			"signing_methods": []any{"jwt"}, "status": "active", "url": "https://example.com/webhooks/flint", "version": float64(1),
		}, "request_id": "req_01J0000000000000000000000A"}
		if withSecret {
			body["data"].(map[string]any)["secret"] = "synthetic-shared-secret"
			body["data"].(map[string]any)["signing_methods"] = []any{"hmac"}
		}
		if err := compiled.Validate(body); err != nil {
			t.Errorf("withSecret=%v: %v", withSecret, err)
		}
	}
}

func TestDeliveryRateCallbackCreateOutputSchemaAllowsAbsentSecret(t *testing.T) {
	cmd, _ := NewRegistry().ByName("delivery-rate-callbacks.create")
	schema, err := schemaForCommand(cmd, false)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("https://example.com/callback-output", schema); err != nil {
		t.Fatal(err)
	}
	compiled, compileErr := compiler.Compile("https://example.com/callback-output")
	if compileErr != nil {
		t.Fatal(compileErr)
	}
	for _, withSecret := range []bool{false, true} {
		data := map[string]any{
			"delivery_rate_callback_id": "dcb_test", "current_delivery_rate_callback_revision_id": "dcbr_test",
			"name": "Synthetic rates", "status": "inactive", "version": float64(1),
			"configuration": map[string]any{"url": "https://example.com/delivery-rates"},
			"created_at":    "2026-10-28T00:00:00Z", "updated_at": "2026-10-28T00:00:00Z",
			"signing_methods": []any{"jwt"}, "circuit_state": "closed", "circuit_failure_count": float64(0),
		}
		if withSecret {
			data["signing_methods"] = []any{"jwt", "hmac"}
			data["secret"], data["key_id"] = "synthetic-secret", "key_synthetic"
		}
		if err := compiled.Validate(map[string]any{"data": data, "request_id": "req_01J0000000000000000000000A"}); err != nil {
			t.Errorf("withSecret=%v: %v", withSecret, err)
		}
	}
}

func TestDeliveryRateCallbackRotateSecretRequest(t *testing.T) {
	var method, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		method, path = r.Method, r.URL.Path
		fmt.Fprint(w, `{"data":{"delivery_rate_callback_id":"dcb_test","key_id":"key_synthetic","secret":"synthetic-secret"}}`)
	}))
	defer server.Close()
	app, out, errOut := testApp(t, server.URL)
	if exit := app.Run([]string{"delivery-rate-callbacks", "rotate-secret", "dcb_test"}); exit != ExitConfirmation || method != "" {
		t.Fatalf("rotation without --confirm: exit=%d request=%s %s stdout=%s stderr=%s", exit, method, path, out, errOut)
	}
	app, out, errOut = testApp(t, server.URL)
	if exit := app.Run([]string{"delivery-rate-callbacks", "rotate-secret", "dcb_test", "--confirm", "--output", "json", "--idempotency-key", "synthetic-rotation"}); exit != ExitOK {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, errOut)
	}
	if method != "POST" || path != "/v1/delivery-rate-callbacks/dcb_test/rotate-secret" || !strings.Contains(out.String(), `"key_id":"key_synthetic"`) {
		t.Fatalf("request=%s %s stdout=%s", method, path, out)
	}
}

func TestSigningHelpAndMCPDescriptions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wants []string
	}{
		{"webhook-endpoints.create", []string{"--signing-methods", "jwt|hmac"}},
		{"webhook-endpoints.update", []string{"--signing-methods", "--expected-version", "signing_methods"}},
		{"webhook-endpoints.rotate-secret", []string{"--confirm"}},
		{"listen", []string{"local HMAC listen secret", "regardless of endpoint signing methods", "JWT handlers need an HMAC path or a local test bypass"}},
		{"delivery-rate-callbacks.create", []string{"--signing-methods", "jwt|hmac"}},
		{"delivery-rate-callbacks.update", []string{"--signing-methods", "--expected-version", "signing_methods"}},
		{"delivery-rate-callbacks.rotate-secret", []string{"--confirm"}},
	} {
		app, out, errOut := testApp(t, "")
		if exit := app.Run([]string{"help", tc.name}); exit != ExitOK {
			t.Fatalf("%s exit=%d stdout=%s stderr=%s", tc.name, exit, out, errOut)
		}
		cmd, ok := app.Registry.ByName(tc.name)
		if !ok {
			t.Fatal("missing command", tc.name)
		}
		if !commandOwned(cmd) {
			operation, ok := openAPIOperationByID(cmd.OperationID)
			if !ok {
				t.Fatal("missing operation", cmd.OperationID)
			}
			summary := strings.TrimSpace(operation.Summary)
			if cmd.Description != summary || !strings.HasPrefix(out.String(), summary+"\n") {
				t.Errorf("%s CLI help must use operation summary %q: %s", tc.name, summary, out)
			}
		}
		for _, want := range tc.wants {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s help lacks %q: %s", tc.name, want, out)
			}
		}
	}
	for name, wants := range map[string][]string{
		"webhook-endpoints.create":              {"signing_methods defaults to [jwt]", "Authorization: Bearer <token>", "Flint's JWKS", "save it if present"},
		"webhook-endpoints.update":              {"replaces the set", "409 ACTION_NOT_ALLOWED with reason secret_not_issued", "expected_version is required when sending", "signing_methods"},
		"webhook-endpoints.rotate-secret":       {"issue the first one and enable hmac"},
		"delivery-rate-callbacks.create":        {"signing_methods defaults to [jwt]", "Authorization: Bearer <token>", "aud set to the delivery_rate_callback_id", "save them if present"},
		"delivery-rate-callbacks.update":        {"replaces the set", "409 ACTION_NOT_ALLOWED with reason secret_not_issued", "expected_version is required when sending", "signing_methods"},
		"delivery-rate-callbacks.rotate-secret": {"issue the first one and enable hmac", "secret and key_id"},
		"listen":                                {"HMAC listen secret", "regardless of endpoint signing methods", "JWT handlers need an HMAC path or a local test bypass"},
	} {
		cmd, _ := NewRegistry().ByName(name)
		schema, _ := schemaForMCPTool(cmd, mcpPolicy{})
		description := mcpToolDescription(cmd, schema)
		for _, want := range wants {
			if !strings.Contains(description, want) {
				t.Errorf("%s MCP description lacks %q: %s", name, want, description)
			}
		}
	}
}
