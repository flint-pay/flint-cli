package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
		"clock":          map[string]any{"test_clock_id": "clock_01KEZF7C003ACYXT6Q5EQZ78EE", "frozen_at": "2026-10-30T12:00:00Z"},
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
						if !outputSchemaDeclaresPath(command, strings.TrimPrefix(strings.TrimPrefix(path, "data."), "0.")) {
							t.Errorf("%s output %s not in schema", step.Name, path)
						}
					}
				}
				for _, condition := range step.Until {
					path, value, _ := strings.Cut(condition, "=")
					if !outputSchemaDeclaresPath(command, strings.TrimPrefix(strings.TrimPrefix(path, "data."), "0.")) {
						t.Errorf("%s condition %s not in schema", step.Name, path)
					}
					schema, _ := schemaForCommand(command, false)
					defs, _ := schema["$defs"].(map[string]any)
					if !strings.HasPrefix(path, "data.") {
						path = "data." + path
					}
					for _, part := range strings.Split(path, ".") {
						if _, err := strconv.Atoi(part); err == nil {
							schema = bodyArrayItems(schema, defs, map[string]bool{})
						} else {
							props, _ := bodyProperties(schema, defs, map[string]bool{})
							schema, _ = props[part].(map[string]any)
						}
					}
					values, _ := schema["enum"].([]any)
					for _, accepted := range step.Accepted {
						if !slices.Contains(values, any(accepted)) {
							t.Errorf("%s accepted state %s not in enum", step.Name, accepted)
						}
					}
					if !slices.Contains(values, any(value)) {
						t.Errorf("%s condition value %s not in enum %v", step.Name, value, values)
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
			prefix := "trigger-test"
			if !recipe.Resumable {
				prefix = ""
			}
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
					if prefix == "" {
						prefix = strings.TrimSuffix(r.Header.Get("Idempotency-Key"), "-"+step.Name)
						if !strings.HasPrefix(prefix, "flint-trigger-") {
							t.Errorf("missing generated run prefix: %q", prefix)
						}
					}
					wantKey = prefix + "-" + step.Name
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
			args := []string{"trigger", entry.Event, "--output", "json"}
			if recipe.Resumable {
				args = append(args, "--idempotency-key", prefix)
			}
			if code := app.Run(args); code != 0 {
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
					want, _ := triggerLookupPath(fixtures[step.Name], path)
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
		{"unknown", "sandbox", "TRIGGER_EVENT_UNSUPPORTED", []string{"order.paidd"}, 2},
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
			if tc.name == "list" {
				t.Setenv("FLINT_API_KEY", "")
			}
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

func TestTriggerAsyncResumeStable(t *testing.T) {
	for _, name := range []string{"dispute", "subscription"} {
		t.Run(name, func(t *testing.T) {
			recipe := triggerRecipes()[name]
			fixtures := triggerFixtures()
			count := 0
			bodies := map[string]string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				step := recipe.Steps[count%len(recipe.Steps)]
				run := count / len(recipe.Steps)
				count++
				if r.Method != "GET" {
					raw, _ := io.ReadAll(r.Body)
					key := r.Header.Get("Idempotency-Key")
					if previous, ok := bodies[key]; ok && previous != string(raw) {
						t.Errorf("replayed %s with changed body: %s != %s", key, previous, raw)
					}
					bodies[key] = string(raw)
				}
				value := fixtures[step.Name]
				if run > 0 && step.Name == "dispute" {
					value = map[string]any{"data": []any{map[string]any{"status": "won", "dispute_id": "disp_01KEZF7C003ACYXT6Q5EQZ78EE"}}}
				}
				if step.Name == "active" {
					next := "2026-11-30T12:00:00Z"
					if run > 0 {
						next = "2026-12-30T12:00:00Z"
					}
					value = map[string]any{"data": map[string]any{"status": "active", "next_billing_at": next}}
				}
				if run == 0 && step.Name == "simulate" {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":{"code":"INVALID_REQUEST","type":"invalid_request_error","message":"response lost fixture"}}`)
					return
				}
				if run == 0 && step.Name == "ready" {
					value = map[string]any{"data": map[string]any{"status": "failed"}}
				}
				json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			event := "dispute.won"
			expected := ExitAPI
			if name == "subscription" {
				event = "test_clock.ready"
				expected = ExitWait
			}
			args := []string{"trigger", event, "--idempotency-key", "stable-run", "--output", "json"}
			if code := app.Run(args); code != expected {
				t.Fatalf("first code %d: %s %s", code, out, stderr)
			}
			out.Reset()
			stderr.Reset()
			if code := app.Run(args); code != ExitOK {
				t.Fatalf("resume code %d: %s %s", code, out, stderr)
			}
			if name == "subscription" && !strings.Contains(bodies["stable-run-advance"], "2026-11-30T12:01:00Z") {
				t.Errorf("unexpected stable target: %s", bodies["stable-run-advance"])
			}
		})
	}
}

func TestTriggerHumanRecoveryAndSlowWait(t *testing.T) {
	t.Run("human_resume", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/auth-context" {
				fmt.Fprint(w, authContextJSON("sandbox"))
				return
			}
			if r.URL.Path == "/v1/orders" {
				json.NewEncoder(w).Encode(triggerFixtures()["order"])
				return
			}
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"code":"INVALID_REQUEST","type":"invalid_request_error","message":"fixture failure"}}`)
		}))
		defer server.Close()
		app, out, stderr := testApp(t, server.URL)
		if code := app.Run([]string{"trigger", "order.paid", "--output", "human"}); code != ExitAPI {
			t.Fatalf("code %d", code)
		}
		if !strings.Contains(out.String()+stderr.String(), "flint trigger order.paid --idempotency-key flint-trigger-") {
			t.Fatalf("missing human recovery: %s %s", out, stderr)
		}
	})
	t.Run("slow_wait", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
				fmt.Fprint(w, `{"data":{"status":"advancing"}}`)
			}
		}))
		defer server.Close()
		app, _, _ := testApp(t, server.URL)
		recipe := triggerRecipe{Name: "clock", Steps: []triggerStep{{Name: "ready", OperationID: "getTestClock", Path: map[string]string{"test_clock_id": "clock_fixture"}, Until: []string{"status=ready"}, Accepted: []string{"ready"}}}}
		start := time.Now()
		_, e := app.runTrigger(context.Background(), recipe, triggerEntry{Event: "test_clock.ready"}, Options{For: 10 * time.Millisecond, Timeout: time.Second}, AuthContext{}, "flint_test_fixture", server.URL, "slow-run")
		if e == nil || e.ExitCode != ExitWait || e.Code != "WAIT_CONDITION_UNMET" {
			t.Fatalf("wait error = %v", e)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("wait exceeded bound: %s", time.Since(start))
		}
	})
}

func TestTriggerErrorDetailsAndHumanDiscovery(t *testing.T) {
	for _, array := range []bool{false, true} {
		t.Run(fmt.Sprint(array), func(t *testing.T) {
			facts := any(map[string]any{"reason": "fixture", "current_version": 7})
			if array {
				facts = []any{map[string]any{"reason": "fixture", "current_version": 7}}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "INVALID_REQUEST", "message": "fixture error", "details": facts, "remediation": map[string]any{"retry": "none"}}})
			}))
			defer server.Close()
			app, out, _ := testApp(t, server.URL)
			if code := app.Run([]string{"trigger", "customer.created", "--output", "json"}); code != ExitAPI {
				t.Fatalf("code %d: %s", code, out)
			}
			var value any
			json.Unmarshal(out.Bytes(), &value)
			path := "error.details.reason"
			if array {
				path = "error.details.problems.0.reason"
			}
			if got, _ := lookupPath(value, path); got != "fixture" {
				t.Errorf("facts moved: %s", out)
			}
			if got, _ := lookupPath(value, "error.remediation.retry"); got != "none" {
				t.Errorf("remediation moved: %s", out)
			}
			if _, ok := lookupPath(value, "error.details.trigger.resume"); !ok {
				t.Errorf("missing trigger recovery: %s", out)
			}
		})
	}
	t.Run("human_id", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/auth-context" {
				fmt.Fprint(w, authContextJSON("sandbox"))
				return
			}
			json.NewEncoder(w).Encode(triggerFixtures()["customer"])
		}))
		defer server.Close()
		app, out, errOut := testApp(t, server.URL)
		if code := app.Run([]string{"trigger", "customer.created", "--output", "human"}); code != 0 {
			t.Fatalf("%d: %s", code, errOut)
		}
		if !strings.Contains(out.String(), "customer_id: cus_") {
			t.Fatalf("missing ID: %s", out)
		}
	})
	t.Run("discovery", func(t *testing.T) {
		app, out, errOut := testApp(t, "")
		t.Setenv("FLINT_API_KEY", "")
		if code := app.Run([]string{"trigger", "--output", "json"}); code != ExitUsage {
			t.Fatalf("code %d", code)
		}
		if !strings.Contains(out.String(), "EVENT_TYPE_REQUIRED") || !strings.Contains(out.String(), "stripe_webhook") || !strings.Contains(out.String(), `"calls"`) {
			t.Fatalf("incomplete discovery: %s %s", out, errOut)
		}
	})
}

func TestTriggerCheckoutCannotResume(t *testing.T) {
	for _, output := range []string{"human", "json", "ndjson"} {
		t.Run(output, func(t *testing.T) {
			requests, writes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				writes++
				switch r.URL.Path {
				case "/v1/orders":
					json.NewEncoder(w).Encode(triggerFixtures()["order"])
				case "/v1/checkout-sessions":
					json.NewEncoder(w).Encode(triggerFixtures()["session"])
				default:
					// A committed payment with an unusable response is ambiguous.
					fmt.Fprint(w, `{`)
				}
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			if code := app.Run([]string{"trigger", "checkout_session.paid", "--output", output}); code == ExitOK {
				t.Fatal("ambiguous payment unexpectedly succeeded")
			}
			text := out.String() + stderr.String()
			if writes != 3 || strings.Contains(text, "Resume:") || strings.Contains(text, `"resume":`) || !strings.Contains(text, "start a new run") {
				t.Fatalf("unsafe recovery (writes=%d): %s", writes, text)
			}
			before := requests
			out.Reset()
			stderr.Reset()
			if code := app.Run([]string{"trigger", "checkout_session.paid", "--idempotency-key", "old-run", "--output", output}); code != ExitUsage {
				t.Fatalf("resume code %d: %s %s", code, out, stderr)
			}
			if requests != before || !strings.Contains(out.String()+stderr.String(), "start a new run") {
				t.Fatalf("resume made a request or omitted refusal: %s %s", out, stderr)
			}
		})
	}
}

func TestTriggerRecoverySelection(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		for _, output := range []string{"human", "json"} {
			t.Run(fmt.Sprintf("%s/oauth=%t", output, oauth), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/auth-context" {
						if oauth {
							auth := contextTestAuth("ctx_secondary")
							auth.MerchantID, auth.SandboxID, auth.Scopes = "mer_123", "test_123", allTestScopes()
							json.NewEncoder(w).Encode(map[string]any{"data": auth})
						} else {
							fmt.Fprint(w, authContextJSON("sandbox"))
						}
						return
					}
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":{"code":"INVALID_REQUEST","type":"invalid_request_error","message":"fixture failure"}}`)
				}))
				defer server.Close()
				app, out, stderr := testApp(t, server.URL)
				// Resolve the profile from the environment, not just the original flags.
				if err := app.updateConfig(func(cfg *Config) error { cfg.Profiles["secondary profile"] = profileWithoutCredential(); return nil }); err != nil {
					t.Fatal(err)
				}
				t.Setenv("FLINT_PROFILE", "secondary profile")
				if oauth {
					t.Setenv("FLINT_API_KEY", "")
					credential := testOAuthCredential(t, server.URL, time.Now().Add(time.Hour))
					credential.Version, credential.ContextID, credential.SessionID = 2, "ctx_secondary", "session_one"
					credential.Auth = contextTestAuth("ctx_secondary")
					credential.Auth.MerchantID, credential.Auth.SandboxID, credential.Auth.Scopes = "mer_123", "test_123", allTestScopes()
					installTestOAuth(t, app, credential)
					if err := app.saveConfig(Config{DefaultProfile: "default", Profiles: map[string]Profile{"secondary profile": {ContextID: "ctx_secondary"}}}); err != nil {
						t.Fatal(err)
					}
				}
				if code := app.Run([]string{"trigger", "customer.created", "--idempotency-key", "selection-run", "--for", "3m", "--output", output}); code != ExitAPI {
					t.Fatalf("code %d: %s %s", code, out, stderr)
				}
				resume := ""
				if output == "human" {
					resume = stderr.String()
				} else {
					var value any
					json.Unmarshal(out.Bytes(), &value)
					v, _ := lookupPath(value, "error.details.trigger.resume")
					resume, _ = v.(string)
				}
				for _, fragment := range []string{"--profile 'secondary profile'", "--merchant mer_123", "--for 3m0s"} {
					if !strings.Contains(resume, fragment) {
						t.Errorf("missing %s in %s", fragment, resume)
					}
				}
				if oauth && !strings.Contains(resume, "--context ctx_secondary") {
					t.Fatalf("lost resolved OAuth context: %s", resume)
				}
				if strings.Contains(resume, "flint_test_test") || (!oauth && strings.Contains(resume, "--context")) {
					t.Fatalf("invalid environment credential recovery: %s", resume)
				}
			})
		}
	}
	command := triggerResumeCommand("customer.created", "run", Options{Profile: "secondary", ContextID: "ctx_secondary", Merchant: "mer_123", CredentialStore: "file"})
	if command != "flint trigger customer.created --idempotency-key run --profile secondary --context ctx_secondary --merchant mer_123 --credential-store file" {
		t.Fatalf("lost OAuth selection: %s", command)
	}
}
