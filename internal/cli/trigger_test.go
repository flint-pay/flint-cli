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
)

func triggerFixtures() map[string]any {
	money := map[string]any{"amount": 2500, "currency": "USD"}
	fixtures := map[string]any{
		"customer":       map[string]any{"customer_id": "cus_01KEZF7C003ACYXT6Q5EQZ78EE"},
		"order":          map[string]any{"order_id": "ord_01KEZF7C003ACYXT6Q5EQZ78EE", "settlement_amounts": map[string]any{"outstanding_money": money}},
		"pay":            map[string]any{"order": map[string]any{"order_id": "ord_01KEZF7C003ACYXT6Q5EQZ78EE"}, "order_payment_attempt": map[string]any{"order_payment_attempt_id": "opat_01KEZF7C003ACYXT6Q5EQZ78EE", "payment_intents": []any{map[string]any{"payment_intent_id": "pi_01KEZF7C003ACYXT6Q5EQZ78EE"}}}},
		"refund":         map[string]any{"refund_id": "ref_01KEZF7C003ACYXT6Q5EQZ78EE"},
		"session":        map[string]any{"checkout_session_id": "cs_01KEZF7C003ACYXT6Q5EQZ78EE", "checkout_auth_token": "ckat_private_fixture"},
		"dispute":        []any{map[string]any{"dispute_id": "disp_01KEZF7C003ACYXT6Q5EQZ78EE", "status": "needs_response"}},
		"simulate":       map[string]any{"status": "under_review"},
		"intent":         map[string]any{"payment_intent_id": "pi_01KEZF7C003ACYXT6Q5EQZ78EE"},
		"confirm":        map[string]any{"status": "succeeded"},
		"payment_method": []any{map[string]any{"payment_method_id": "pm_01KEZF7C003ACYXT6Q5EQZ78EE", "status": "active"}},
		"clock":          map[string]any{"test_clock_id": "clock_01KEZF7C003ACYXT6Q5EQZ78EE"},
		"plan":           map[string]any{"subscription_plan_id": "plan_01KEZF7C003ACYXT6Q5EQZ78EE"},
		"subscription":   map[string]any{"subscription_id": "sub_01KEZF7C003ACYXT6Q5EQZ78EE"},
		"active":         map[string]any{"status": "active", "next_billing_at": "2026-12-01T00:00:00Z"},
		"advance":        map[string]any{"status": "advancing"}, "ready": map[string]any{"status": "ready"},
		"invoice": map[string]any{"invoice_id": "inv_01KEZF7C003ACYXT6Q5EQZ78EE", "outstanding_money": money},
		"payment": map[string]any{"invoice_manual_payment": map[string]any{"invoice_manual_payment_id": "imp_01KEZF7C003ACYXT6Q5EQZ78EE"}},
	}
	results := map[string]any{}
	for name, value := range fixtures {
		results[name] = map[string]any{"data": value, "request_id": "req_" + name}
	}
	return results
}

func TestTriggerRecipesMatchEmbeddedSpec(t *testing.T) {
	events := listenEventTypes()
	seen := map[string]bool{}
	for _, entry := range triggerEntries() {
		if seen[entry.Event] || !slices.Contains(events, entry.Event) {
			t.Errorf("duplicate or unknown event: %s", entry.Event)
		}
		seen[entry.Event] = true
	}
	for name, recipe := range triggerRecipes() {
		t.Run(name, func(t *testing.T) {
			for _, step := range recipe.Steps {
				command, ok := triggerOperation(step.OperationID)
				if !ok {
					t.Fatalf("missing %s", step.OperationID)
				}
				operation, _ := openAPIOperationByID(step.OperationID)
				paths := map[string]bool{}
				for _, p := range operation.Parameters {
					if p["in"] == "path" {
						paths[p["name"].(string)] = true
					}
				}
				if len(paths) != len(step.Path) {
					t.Errorf("%s path mismatch", step.Name)
				}
				for key := range step.Path {
					if !paths[key] {
						t.Errorf("%s unknown path %s", step.Name, key)
					}
				}
				if step.Body != nil {
					resolved, e := triggerResolve(step.Body, triggerFixtures())
					if e != nil {
						t.Fatal(e)
					}
					if _, e := validateRequestBody(command, "trigger", resolved.(map[string]any)); e != nil {
						t.Errorf("%s: %v, details: %v", step.Name, e, e.Details)
					}
				}
				for _, outputs := range []map[string]string{step.Outputs, step.Secrets} {
					for _, path := range outputs {
						if !outputSchemaDeclaresPath(command, path) {
							t.Errorf("%s output %s not in schema", step.Name, path)
						}
					}
				}
				for _, condition := range step.Until {
					path, _, _ := strings.Cut(condition, "=")
					if !outputSchemaDeclaresPath(command, path) {
						t.Errorf("%s condition %s not in schema", step.Name, path)
					}
				}
			}
		})
	}
}

func TestTriggerRecipeCallSequence(t *testing.T) {
	for _, entry := range triggerEntries() {
		t.Run(entry.Event, func(t *testing.T) {
			recipe := triggerRecipes()[entry.Recipe]
			fixtures := triggerFixtures()
			count := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				if count >= len(recipe.Steps) {
					t.Error("unexpected request")
					w.WriteHeader(500)
					return
				}
				step := recipe.Steps[count]
				count++
				command, _ := triggerOperation(step.OperationID)
				expected := command.APIPath
				for name, ref := range step.Path {
					value, e := triggerResolve(ref, fixtures)
					if e != nil {
						t.Error(e)
					}
					expected = strings.ReplaceAll(expected, "{"+name+"}", fmt.Sprint(value))
				}
				if r.Method != command.Method || r.URL.Path != expected {
					t.Errorf("got %s %s want %s %s", r.Method, r.URL.Path, command.Method, expected)
				}
				for name, ref := range step.Query {
					value, _ := triggerResolve(ref, fixtures)
					if r.URL.Query().Get(name) != fmt.Sprint(value) {
						t.Errorf("wrong query %s", name)
					}
				}
				wantKey := ""
				if r.Method != "GET" {
					wantKey = "trigger-test-" + step.Name
				}
				if r.Header.Get("Idempotency-Key") != wantKey {
					t.Errorf("wrong idempotency key: %q", r.Header.Get("Idempotency-Key"))
				}
				bearer := "Bearer flint_test_test"
				if step.Credential != "" {
					bearer = "Bearer ckat_private_fixture"
				}
				if r.Header.Get("Authorization") != bearer {
					t.Errorf("wrong bearer for %s", step.Name)
				}
				if step.Body != nil {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					text, _ := json.Marshal(body)
					if strings.Contains(string(text), "{order.") {
						t.Errorf("unresolved reference: %s", text)
					}
					for path, value := range entry.Set {
						if path, ok := strings.CutPrefix(path, step.Name+"."); ok {
							got, _ := lookupPath(body, path)
							if got != value {
								t.Errorf("variant %s = %v", path, got)
							}
						}
					}
				}
				w.WriteHeader(200)
				json.NewEncoder(w).Encode(fixtures[step.Name])
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			if code := app.Run([]string{"trigger", entry.Event, "--idempotency-key", "trigger-test", "--output", "json"}); code != 0 {
				t.Fatalf("code %d: %s %s", code, stderr, out)
			}
			through := len(recipe.Steps)
			for i, step := range recipe.Steps {
				if step.Name == entry.Through {
					through = i + 1
				}
			}
			if count != through {
				t.Errorf("calls %d want %d", count, through)
			}
			if strings.Contains(out.String(), "ckat_private_fixture") {
				t.Error("checkout credential leaked")
			}
			var output struct {
				Data struct {
					Steps    []map[string]any `json:"steps"`
					IDs      map[string]any   `json:"ids"`
					Expected []string         `json:"expected_events"`
				} `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &output); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(output.Data.Expected, []string{entry.Event}) || len(output.Data.Steps) != through {
				t.Errorf("bad output %s", out)
			}
			for i, record := range output.Data.Steps {
				if record["request_id"] != "req_"+recipe.Steps[i].Name {
					t.Errorf("bad request id %v", record)
				}
			}
			for _, step := range recipe.Steps[:through] {
				for name, path := range step.Outputs {
					want, _ := lookupResourcePath(fixtures[step.Name], path)
					if output.Data.IDs[name] != want {
						t.Errorf("missing %s", name)
					}
				}
			}
		})
	}
}

func TestTriggerLocalChecksAndRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, environment, code string
		args                    []string
		exit                    int
	}{
		{"live", "live", "TEST_MODE_REQUIRED", []string{"order.paid"}, 3},
		{"unverified", "unverified", "TEST_MODE_REQUIRED", []string{"order.paid"}, 3},
		{"live_flag", "sandbox", "TEST_MODE_REQUIRED", []string{"order.paid", "--live"}, 2},
		{"unknown", "sandbox", "INVALID_EVENT_TYPE", []string{"order.paidd"}, 2},
		{"unsupported", "sandbox", "TRIGGER_EVENT_UNSUPPORTED", []string{"invoice.overdue"}, 2},
		{"list", "sandbox", "", []string{"--list"}, 0},
		{"dry", "sandbox", "", []string{"order.paid", "--dry-run=client"}, 0},
		{"override", "sandbox", "UNKNOWN_FLAG", []string{"order.paid", "--override", "pay.x=1"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/auth-context" {
					writes++
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				fmt.Fprint(w, authContextJSON(tc.environment))
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			args := append([]string{"trigger"}, tc.args...)
			args = append(args, "--output", "json")
			if code := app.Run(args); code != tc.exit {
				t.Fatalf("code %d want %d: %s", code, tc.exit, stderr)
			}
			if tc.code != "" && !strings.Contains(stderr.String()+out.String(), tc.code) {
				t.Errorf("missing code %s: %s", tc.code, stderr.String()+out.String())
			}
			if writes != 0 {
				t.Error("wrote on refused/dry run")
			}
			if tc.name == "dry" && !strings.Contains(out.String(), "createOrder") {
				t.Error("missing dry plan")
			}
		})
	}
}

func TestTriggerResumeReplaysKeysAndWaitBound(t *testing.T) {
	for _, wait := range []bool{false, true} {
		t.Run(fmt.Sprint(wait), func(t *testing.T) {
			fail := true
			keys := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				if r.Method != "GET" {
					keys = append(keys, r.Header.Get("Idempotency-Key"))
				}
				if r.URL.Path == "/v1/orders" {
					json.NewEncoder(w).Encode(triggerFixtures()["order"])
					return
				}
				if strings.HasSuffix(r.URL.Path, "/pay") {
					if fail && !wait {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":"INVALID_REQUEST","message":"fixture failure"}}`)
						return
					}
					json.NewEncoder(w).Encode(triggerFixtures()["pay"])
					return
				}
				fmt.Fprint(w, `{"data":[],"request_id":"req_wait"}`)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			event := "order.paid"
			args := []string{"trigger", event, "--idempotency-key", "resume-test", "--output", "json"}
			expected := 1
			if wait {
				args[1] = "dispute.won"
				args = append(args, "--for", "1ms")
				expected = 6
			}
			if code := app.Run(args); code != expected {
				t.Fatalf("code %d want %d: %s", code, expected, stderr)
			}
			if !strings.Contains(stderr.String()+out.String(), "completed_steps") || !strings.Contains(stderr.String()+out.String(), "resume-test") {
				t.Errorf("missing resume: %s", stderr)
			}
			if wait {
				if !strings.Contains(stderr.String()+out.String(), "Stripe") {
					t.Error("missing webhook hint")
				}
				return
			}
			fail = false
			stderr.Reset()
			if code := app.Run(args); code != 0 {
				t.Fatalf("resume code %d: %s", code, stderr)
			}
			if keys[0] != keys[2] {
				t.Errorf("changed replay key %v", keys)
			}
		})
	}
}
