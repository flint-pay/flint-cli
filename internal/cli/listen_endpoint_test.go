package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixtureListenEndpoint(id string) listenEndpoint {
	return listenEndpoint{ID: id, URL: listenMarkerURL, CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Version: 3, Status: "active", SigningMethods: []string{"jwt"}, EnabledEvents: []string{"*"}}
}
func writeListenEndpoint(w http.ResponseWriter, id string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": fixtureListenEndpoint(id)})
}
func mockLocalListen(app *App) {
	app.ForwardHTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
	})}
}
func TestListenManagedEndpointCreateReuseAndPagination(t *testing.T) { // AC 1, 2
	t.Setenv("FLINT_WEBHOOK_SECRET", "whsec_ignored_listener_secret")
	for _, scenario := range []string{"create", "reuse", "oldest second page", "tie lowest ID"} {
		t.Run(scenario, func(t *testing.T) {
			creates, lists, signs := 0, 0, 0
			wantID := "whep_managed"
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/auth-context":
					fmt.Fprint(w, authContextJSON("sandbox"))
				case r.URL.Path == "/v1/webhook-endpoints" && r.Method == "GET":
					lists++
					if r.URL.Query().Get("page_size") != "100" {
						t.Error("missing page_size")
					}
					endpoints := []listenEndpoint{}
					next := ""
					if scenario != "create" {
						endpoint := fixtureListenEndpoint(wantID)
						if scenario == "oldest second page" && lists == 1 {
							endpoint.ID = "whep_new"
							endpoint.CreatedAt = endpoint.CreatedAt.Add(time.Hour)
							next = "second"
						}
						endpoints = append(endpoints, endpoint)
						if scenario == "tie lowest ID" {
							other := endpoint
							other.ID = "whep_z"
							endpoints = append([]listenEndpoint{other}, endpoints...)
						}
						if lists == 2 && r.URL.Query().Get("page_token") != "second" {
							t.Error("missing page token")
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": endpoints, "meta": map[string]any{"next_page_token": next}})
				case r.URL.Path == "/v1/webhook-endpoints" && r.Method == "POST":
					creates++
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					want := map[string]any{"url": listenMarkerURL, "status": "active", "signing_methods": []any{"jwt"}, "enabled_events": []any{"*"}, "description": listenEndpointDescription}
					if !reflect.DeepEqual(body, want) || r.Header.Get("Idempotency-Key") == "" {
						t.Errorf("create request=%v", body)
					}
					writeListenEndpoint(w, wantID)
				case strings.HasSuffix(r.URL.Path, "/sign-delivery"):
					signs++
					if r.URL.Path != "/v1/webhook-endpoints/"+wantID+"/sign-delivery" {
						t.Error("wrong signing endpoint")
					}
					mockListenDelivery(w, r)
				case r.URL.Path == "/v1/webhook-events/stream":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, listenEventFrame("whev_one", "customer.created", `{}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			app, out, errOut := testApp(t, api.URL)
			mockLocalListen(app)
			exit := app.Run([]string{"listen", "--forward-to", "http://localhost/hook", "--max-events", "1", "--no-input", "--quiet", "--output", "ndjson"})
			origin := "reused"
			wantCreates := 0
			if scenario == "create" {
				origin = "created"
				wantCreates = 1
			}
			if exit != 0 || creates != wantCreates || signs != 1 {
				t.Fatalf("exit=%d create=%d signs=%d out=%s err=%s", exit, creates, signs, out, errOut)
			}
			record := findListenRecord(t, out.String(), "listener")
			if record["webhook_endpoint_source"] != origin || record["webhook_endpoint_id"] != wantID || record["issuer"] != api.URL || !reflect.DeepEqual(record["event_type"], []any{}) {
				t.Fatalf("listener=%v", record)
			}
			if scenario == "oldest second page" && lists != 2 {
				t.Fatal("did not paginate")
			}
		})
	}
}
func TestListenManagedEndpointRepair(t *testing.T) { // AC 3
	for _, drift := range []string{"inactive", "hmac only", "narrowed", "version conflict"} {
		t.Run(drift, func(t *testing.T) {
			endpoint := fixtureListenEndpoint("whep_managed")
			endpoint.SigningMethods = []string{"hmac", "jwt"}
			switch drift {
			case "inactive":
				endpoint.Status = "inactive"
			case "hmac only":
				endpoint.SigningMethods = []string{"hmac"}
			case "narrowed":
				endpoint.EnabledEvents = []string{"customer.created"}
			case "version conflict":
				endpoint.Status = "inactive"
			}
			patches, signs := 0, 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/auth-context":
					fmt.Fprint(w, authContextJSON("sandbox"))
				case r.URL.Path == "/v1/webhook-endpoints":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []listenEndpoint{endpoint}})
				case r.Method == "PATCH":
					patches++
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["expected_version"] != float64(endpoint.Version) || body["status"] != "active" || !reflect.DeepEqual(body["enabled_events"], []any{"*"}) || !reflect.DeepEqual(body["signing_methods"], []any{"hmac", "jwt"}) {
						t.Errorf("patch=%v endpoint=%v", body, endpoint)
					}
					if drift == "version conflict" && patches == 1 {
						endpoint.Version++
						w.WriteHeader(409)
						fmt.Fprint(w, `{"error":{"code":"CONCURRENT_MODIFICATION"}}`)
						return
					}
					endpoint.Status = "active"
					endpoint.SigningMethods = []string{"hmac", "jwt"}
					endpoint.EnabledEvents = []string{"*"}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": endpoint})
				case r.URL.Path == "/v1/webhook-endpoints/whep_managed":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": endpoint})
				case strings.HasSuffix(r.URL.Path, "/sign-delivery"):
					signs++
					mockListenDelivery(w, r)
				case r.URL.Path == "/v1/webhook-events/stream":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, listenEventFrame("whev_one", "customer.created", `{}`))
				default:
					t.Error("unexpected request")
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			app, out, _ := testApp(t, api.URL)
			mockLocalListen(app)
			if code := app.Run([]string{"listen", "--forward-to", "http://localhost/hook", "--max-events", "1", "--output", "json"}); code != 0 {
				t.Fatalf("exit=%d %s", code, out)
			}
			want := 1
			if drift == "version conflict" {
				want = 2
			}
			if patches != want || signs != 1 {
				t.Fatalf("patches=%d signs=%d", patches, signs)
			}
		})
	}
}
func TestListenSandboxGuardBeforeHTTP(t *testing.T) { // AC 4
	for _, scenario := range []string{"live key", "live flag", "cached live context"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
			defer api.Close()
			app, out, _ := testApp(t, api.URL)
			args := []string{"listen", "--forward-to", "http://localhost/hook", "--for", "1s", "--output", "json"}
			if scenario == "live flag" {
				args = append(args, "--live")
			} else if scenario == "live key" {
				t.Setenv("FLINT_API_KEY", "flint_live_fixture")
			} else {
				app.cacheAuthContext(api.URL, "flint_test_test", authContextEnvelope{Data: AuthContext{AuthType: "api_key", Environment: "live", MerchantID: "mer_fixture"}})
			}
			if code := app.Run(args); code != ExitUsage || calls != 0 || !strings.Contains(out.String(), "TEST_MODE_REQUIRED") {
				t.Fatalf("exit=%d calls=%d %s", code, calls, out)
			}
		})
	}
}
func TestListenSubscriptionFilters(t *testing.T) { // AC 5-8, 11
	for _, scenario := range []string{"implicit", "explicit enabled", "explicit disabled", "wildcard selected", "wildcard all", "over 50", "inactive flag", "hmac flag"} {
		t.Run(scenario, func(t *testing.T) {
			endpoint := fixtureListenEndpoint("whep_fixture")
			endpoint.EnabledEvents = []string{"customer.created"}
			selected := []string{}
			wantQuery := "customer.created"
			wantExit := ExitOK
			switch scenario {
			case "explicit enabled":
				selected = []string{"customer.created"}
			case "explicit disabled":
				selected = []string{"order.paid"}
				wantExit = ExitUsage
			case "wildcard selected":
				endpoint.EnabledEvents = []string{"*"}
				selected = []string{"a,b"}
				wantQuery = "a,b"
			case "wildcard all":
				endpoint.EnabledEvents = []string{"*"}
				wantQuery = ""
			case "over 50":
				for i := 0; i < 51; i++ {
					endpoint.EnabledEvents = append(endpoint.EnabledEvents, fmt.Sprintf("future.type%d", i))
				}
				wantQuery = ""
			case "inactive flag":
				endpoint.Status = "inactive"
				wantExit = ExitAPI
			case "hmac flag":
				endpoint.SigningMethods = []string{"hmac"}
				wantExit = ExitAPI
			}
			streams, signs, patches := 0, 0, 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/auth-context":
					fmt.Fprint(w, authContextJSON("sandbox"))
				case r.Method == "PATCH":
					patches++
					http.NotFound(w, r)
				case r.URL.Path == "/v1/webhook-endpoints/whep_fixture":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": endpoint})
				case strings.HasSuffix(r.URL.Path, "/sign-delivery"):
					signs++
					mockListenDelivery(w, r)
				case r.URL.Path == "/v1/webhook-events/stream":
					streams++
					if got := r.URL.Query().Get("event_type"); got != wantQuery {
						t.Errorf("query=%s want=%s", got, wantQuery)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if scenario == "over 50" {
						fmt.Fprint(w, listenEventFrame("whev_drop", "order.paid", `{}`))
					}
					typ := "customer.created"
					if scenario == "wildcard selected" {
						typ = "a"
					}
					fmt.Fprint(w, listenEventFrame("whev_one", typ, `{}`))
				default:
					t.Error("unexpected request")
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			app, out, _ := testApp(t, api.URL)
			mockLocalListen(app)
			args := []string{"listen", "--forward-to", "http://localhost/hook", "--webhook-endpoint-id", "whep_fixture", "--max-events", "1", "--output", "ndjson"}
			for _, typ := range selected {
				args = append(args, "--event-type", typ)
			}
			code := app.Run(args)
			if code != wantExit || patches != 0 {
				t.Fatalf("exit=%d patches=%d out=%s", code, patches, out)
			}
			if wantExit != 0 {
				if streams != 0 || signs != 0 {
					t.Fatal("request after invalid endpoint/filter")
				}
				if wantExit == ExitUsage && (!strings.Contains(out.String(), "EVENT_TYPE_NOT_ENABLED") || !strings.Contains(out.String(), "not_enabled")) {
					t.Fatal(out.String())
				}
			} else if signs != 1 {
				t.Fatalf("signs=%d", signs)
			}
		})
	}
}
func TestListenSigningSkipsAndDrift(t *testing.T) { // AC 9, 10; full failure table
	for _, scenario := range []string{"configuration_required", "payload_expired", "unknown", "event_not_found", "inactive", "hmac", "deleted", "forbidden", "concurrent"} {
		t.Run(scenario, func(t *testing.T) {
			gets, signs := 0, 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/auth-context":
					fmt.Fprint(w, authContextJSON("sandbox"))
				case r.URL.Path == "/v1/webhook-endpoints/whep_fixture":
					gets++
					endpoint := fixtureListenEndpoint("whep_fixture")
					if gets > 1 {
						switch scenario {
						case "inactive":
							endpoint.Status = "inactive"
						case "hmac":
							endpoint.SigningMethods = []string{"hmac"}
						case "deleted":
							w.WriteHeader(404)
							fmt.Fprint(w, `{"error":{"code":"RESOURCE_NOT_FOUND"}}`)
							return
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": endpoint})
				case strings.HasSuffix(r.URL.Path, "/sign-delivery"):
					signs++
					if signs == 1 {
						status, code, param, reason := 409, "ACTION_NOT_ALLOWED", "", scenario
						switch scenario {
						case "event_not_found":
							status, code, param = 404, "RESOURCE_NOT_FOUND", "webhook_event_id"
						case "forbidden":
							status, code = 403, "FORBIDDEN"
						case "concurrent":
							code = "CONCURRENT_MODIFICATION"
						}
						w.WriteHeader(status)
						_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "param": param, "message": "Bearer DO_NOT_PRINT", "details": []any{map[string]any{"reason": reason, "body": "PRIVATE_SIGNED_BODY", "headers": map[string]any{"Authorization": "SECRET_JWT"}}}}})
						return
					}
					mockListenDelivery(w, r)
				case r.URL.Path == "/v1/webhook-events/stream":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"whev_before\"}\n\n", listenEventFrame("whev_first", "customer.created", `{}`), listenEventFrame("whev_second", "customer.created", `{}`))
				default:
					t.Error("unexpected route")
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			app, out, errOut := testApp(t, api.URL)
			mockLocalListen(app)
			code := app.Run([]string{"listen", "--forward-to", "http://localhost/hook", "--webhook-endpoint-id", "whep_fixture", "--max-events", "1", "--output", "ndjson"})
			checkpoint := findListenRecord(t, out.String(), "checkpoint")
			if scenario == "inactive" || scenario == "hmac" || scenario == "deleted" {
				if code != ExitAPI || checkpoint["cursor"] != "whev_before" || !strings.Contains(out.String(), "WEBHOOK_ENDPOINT_NOT_USABLE") || !strings.Contains(out.String(), "Resume with --cursor whev_before.") {
					t.Fatalf("exit=%d %s", code, out)
				}
			} else if scenario == "forbidden" {
				if code != 0 || checkpoint["withheld"] != float64(1) {
					t.Fatal(out.String())
				}
			} else {
				if code != 0 || signs != 2 || checkpoint["forwarded"] != float64(1) {
					t.Fatalf("exit=%d signs=%d %s", code, signs, out)
				}
				if scenario != "concurrent" {
					skip := findListenRecord(t, out.String(), "skipped")
					want := scenario
					if scenario == "configuration_required" {
						want = "not_subscribed"
					}
					if scenario == "unknown" {
						want = "not_allowed"
					}
					if skip["reason"] != want || checkpoint["skipped"] != float64(1) || checkpoint["processed"] != float64(1) || checkpoint["cursor"] != "whev_second" {
						t.Fatal(out.String())
					}
				}
			}
			for _, secret := range []string{"DO_NOT_PRINT", "PRIVATE_SIGNED_BODY", "SECRET_JWT"} {
				if strings.Contains(out.String()+errOut.String(), secret) {
					t.Fatal("signed error leaked")
				}
			}
		})
	}
}
func TestListenRemovedLocalSigningFlags(t *testing.T) { // AC 12
	for _, flag := range []string{"--print-secret", "--webhook-secret=whsec_private", "--webhook-secret"} {
		app, out, _ := testApp(t, "http://127.0.0.1:1")
		if code := app.Run([]string{"listen", flag, "--output", "json"}); code != ExitUsage || !strings.Contains(out.String(), "LOCAL_SIGNING_REMOVED") || strings.Contains(out.String(), "whsec_private") {
			t.Fatalf("%s: exit=%d %s", flag, code, out)
		}
	}
}
func TestListenPreparationErrorsAndDryRun(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":{"code":"INSUFFICIENT_SCOPE","message":"Missing webhooks.write."}}`)
	}))
	defer api.Close()
	app, out, _ := testApp(t, api.URL)
	if code := app.Run([]string{"listen", "--forward-to", "http://localhost/hook", "--for", "1s", "--output", "json"}); code != ExitAuth || !strings.Contains(out.String(), "Could not prepare the CLI listener endpoint:") || !strings.Contains(out.String(), "Pass --webhook-endpoint-id") || !strings.Contains(out.String(), "INSUFFICIENT_SCOPE") {
		t.Fatal(out.String())
	}
	calls := 0
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("must not call") })}
	out.Reset()
	if code := app.Run([]string{"listen", "--forward-to", "http://localhost/hook", "--dry-run=client", "--output", "json"}); code != 0 || calls != 0 {
		t.Fatalf("exit=%d calls=%d %s", code, calls, out)
	}
}
func TestListenManagedBannerAndPublishedContract(t *testing.T) { // AC 13; signed material tests in listen_delivery_test.go
	app, out, _ := testApp(t, "http://127.0.0.1:1")
	if code := app.Run([]string{"listen", "--help", "--output", "json"}); code != 0 || !strings.Contains(out.String(), "creates or reuses") {
		t.Fatal(out.String())
	}
	entries := errorCatalog()["listen_codes"].([]map[string]any)
	codes := map[string]bool{}
	for _, e := range entries {
		codes[e["code"].(string)] = true
	}
	for _, code := range []string{"TEST_MODE_REQUIRED", "EVENT_TYPE_NOT_ENABLED", "WEBHOOK_ENDPOINT_NOT_USABLE", "LOCAL_SIGNING_REMOVED"} {
		if !codes[code] {
			t.Fatal(code)
		}
	}
	if codes["WEBHOOK_ENDPOINT_REQUIRED"] {
		t.Fatal("retired code")
	}
	endpoint := fixtureListenEndpoint("whep_fixture")
	if filter, e := listenEffectiveFilter(endpoint, []string{}); e != nil || len(filter) != 0 {
		t.Fatal("wildcard filter")
	}
	// A fresh idempotency key is generated per creation, never retained after deletion.
	a, _ := newIdempotencyKey()
	b, _ := newIdempotencyKey()
	if a == b {
		t.Fatal("idempotency keys repeat")
	}
}

func TestListenResolvedEndpointBanner(t *testing.T) {
	for _, origin := range []string{"created", "reused", "flag"} {
		t.Run(origin, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/auth-context":
					fmt.Fprint(w, authContextJSON("sandbox"))
				case r.URL.Path == "/v1/webhook-endpoints" && r.Method == "GET":
					endpoints := []listenEndpoint{}
					if origin == "reused" {
						endpoints = append(endpoints, fixtureListenEndpoint("whep_fixture"))
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": endpoints})
				case r.URL.Path == "/v1/webhook-endpoints" || r.URL.Path == "/v1/webhook-endpoints/whep_fixture":
					writeListenEndpoint(w, "whep_fixture")
				case strings.HasSuffix(r.URL.Path, "/sign-delivery"):
					mockListenDelivery(w, r)
				case r.URL.Path == "/v1/webhook-events/stream":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, listenEventFrame("whev_one", "customer.created", `{}`))
				default:
					t.Error("unexpected request")
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			app, out, _ := testApp(t, api.URL)
			mockLocalListen(app)
			args := []string{"listen", "--forward-to", "http://localhost/hook", "--max-events", "1"}
			if origin == "flag" {
				args = append(args, "--webhook-endpoint-id", "whep_fixture")
			}
			if code := app.Run(args); code != 0 {
				t.Fatalf("exit=%d %s", code, out)
			}
			suffix := ""
			if origin == "created" {
				suffix = " (created for flint listen; later runs reuse it)"
			}
			if origin == "reused" {
				suffix = " (reused)"
			}
			if !strings.Contains(out.String(), "Endpoint: whep_fixture"+suffix+". Signed by Flint; delivery is at-least-once.") || !strings.Contains(out.String(), "Issuer: "+api.URL) || strings.Contains(out.String(), "FLINT_WEBHOOK_ENDPOINT_ID=whep_fixture") != (origin != "flag") {
				t.Fatal(out.String())
			}
		})
	}
}
