package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func outcomeResponse(status int, requestID, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"X-Request-Id": []string{requestID}}, Body: io.NopCloser(strings.NewReader(body))}
}

type contextResponseBody struct{ ctx context.Context }

func (b contextResponseBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b contextResponseBody) Close() error             { return nil }

func TestRequestBudgetAllowsSlowResponses(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			app, _, _ := testApp(t, "http://127.0.0.1:1")
			synctest.Test(t, func(t *testing.T) {
				start, attempts := time.Now(), 0
				app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					attempts++
					deadline, _ := r.Context().Deadline()
					if time.Until(deadline) != 150*time.Second {
						t.Fatal("attempt did not get the remaining budget", time.Until(deadline))
					}
					select {
					case <-time.After(125 * time.Second):
						return outcomeResponse(200, "req_slow", `{"data":{"payment_intent_id":"pi_fixture"}}`), nil
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				})}
				path, key := "/v1/payment-intents/pi_fixture/confirm", "saved-action"
				if method == "GET" {
					path, key = "/v1/payment-intents/pi_fixture", ""
				}
				response, e := app.doRequestWithin(t.Context(), defaultOptions().Timeout, "http://127.0.0.1:1", "", method, path, []byte(`{}`), key, false)
				if e != nil || response == nil || attempts != 1 || time.Since(start) != 125*time.Second {
					t.Fatalf("attempts=%d elapsed=%s err=%+v", attempts, time.Since(start), e)
				}
			})
		})
	}
}

func TestRequestBudgetExplicitOverride(t *testing.T) {
	_, opts, _, parseErr := parseInvocation(NewRegistry(), []string{"customers", "list", "--timeout", "17s"})
	if parseErr != nil || opts.Timeout != 17*time.Second {
		t.Fatalf("timeout=%s error=%+v", opts.Timeout, parseErr)
	}
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	synctest.Test(t, func(t *testing.T) {
		started, attempts := time.Now(), 0
		app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		_, e := app.doRequestWithin(t.Context(), opts.Timeout, "http://127.0.0.1:1", "", "GET", "/v1/customers", nil, "", false)
		if e == nil || e.Code != "CLIENT_TIMEOUT" || attempts != 1 || time.Since(started) != 17*time.Second {
			t.Fatalf("attempts=%d elapsed=%s error=%+v", attempts, time.Since(started), e)
		}
	})
}

func TestRequestOutcomeKeyedAPIResponses(t *testing.T) {
	for _, tc := range []struct {
		name, path, recovery, code, retry, extra string
		status, attempts                         int
		unknown                                  bool
	}{
		{"server_error", "/v1/payment-intents/pi_fixture/confirm", "/v1/payment-intents/pi_fixture", "INTERNAL_ERROR", "same_request", "", 500, 3, true},
		{"server_stop", "/v1/payment-intents/pi_fixture/capture", "/v1/payment-intents/pi_fixture", "SERVICE_UNAVAILABLE", "after_refresh", "", 503, 1, true},
		{"server_unretryable", "/v1/refunds", "/v1/refunds?idempotency_key=saved-action", "HTTP_501", "none", "", 501, 1, true},
		{"recovery_required", "/v1/refunds", "/v1/refunds?idempotency_key=saved-action", "IDEMPOTENCY_RECOVERY_REQUIRED", "none", "", 409, 1, true},
		{"in_progress", "/v1/orders/ord_fixture/pay", "/v1/orders/ord_fixture/payment-attempts?idempotency_key=saved-action", "IDEMPOTENCY_KEY_IN_PROGRESS", "same_request", "", 409, 3, true},
		{"unknown_reason", "/v1/payment-intents/pi_fixture/capture", "/v1/payment-intents/pi_fixture", "SERVICE_UNAVAILABLE", "none", `,"reason":"outcome_unknown"`, 409, 1, true},
		{"unknown_detail", "/v1/orders/ord_fixture/pay", "/v1/orders/ord_fixture/payment-attempts?idempotency_key=saved-action", "SERVICE_UNAVAILABLE", "none", `,"details":[{"reason":"outcome_unknown"}]`, 409, 1, true},
		{"unknown_legacy_detail", "/v1/refunds", "/v1/refunds?idempotency_key=saved-action", "SERVICE_UNAVAILABLE", "none", `,"details":{"reason":"outcome_unknown"}`, 409, 1, true},
		{"definite_conflict", "/v1/payment-intents/pi_fixture/confirm", "", "VERSION_CONFLICT", "after_refresh", "", 409, 1, false},
		{"definite_key_reuse", "/v1/refunds", "", "IDEMPOTENCY_KEY_REUSED", "none", "", 409, 1, false},
	} {
		for _, mk := range []struct{ method, key string }{{"POST", "saved-action"}, {"POST", ""}, {"GET", ""}} {
			t.Run(tc.name+"/"+mk.method+"/"+mk.key, func(t *testing.T) {
				app, _, stderr := testApp(t, "http://127.0.0.1:1")
				synctest.Test(t, func(t *testing.T) {
					attempts := 0
					apiError := fmt.Sprintf(`{"type":"api_error","code":%q,"message":"fixture","remediation":{"retry":%q,"next_steps":"Server recovery step."}%s}`, tc.code, tc.retry, tc.extra)
					app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						attempts++
						if r.Header.Get("Idempotency-Key") != mk.key {
							t.Fatal("key changed")
						}
						return outcomeResponse(tc.status, fmt.Sprintf("req_%d", attempts), `{"error":`+apiError+`}`), nil
					})}
					path := tc.path
					if mk.method == "GET" {
						path = "/v1/payment-intents/pi_fixture"
					}
					_, e := app.doRequestWithin(t.Context(), defaultOptions().Timeout, "http://127.0.0.1:1", "", mk.method, path, []byte(`{}`), mk.key, false)
					wantAttempts := tc.attempts
					if mk.method == "POST" && mk.key == "" {
						wantAttempts = 1
					}
					if e == nil || e.HTTPStatus != tc.status || attempts != wantAttempts {
						t.Fatalf("attempts=%d error=%+v", attempts, e)
					}
					env := mcpCLIErrorEnvelope(e)
					if mk.key == "" || !tc.unknown {
						if e.Code != tc.code || lookupPathValue(env, "error.details.outcome") != nil || lookupPathValue(env, "error.remediation.retry") != tc.retry {
							t.Fatal(env)
						}
						return
					}
					for path, want := range map[string]any{
						"error.code": "CLIENT_TIMEOUT", "error.details.outcome": "unknown", "error.details.retry": "none", "error.remediation.retry": "none", "error.retryable": false,
						"error.details.idempotency_key": mk.key, "error.details.attempt_count": float64(wantAttempts), "error.request_id": fmt.Sprintf("req_%d", wantAttempts),
					} {
						if got := lookupPathValue(env, path); got != want {
							t.Errorf("%s=%v want %v", path, got, want)
						}
					}
					lastRead := 0
					if tc.path == "/v1/orders/ord_fixture/pay" {
						lastRead = 1
					}
					if lookupPathValue(env, fmt.Sprintf("error.details.recovery_reads.%d.path", lastRead)) != tc.recovery || lookupPathValue(env, fmt.Sprintf("error.details.request_ids.%d", wantAttempts-1)) != fmt.Sprintf("req_%d", wantAttempts) {
						t.Fatal(env)
					}
					var expected any
					if err := json.Unmarshal([]byte(apiError), &expected); err != nil {
						t.Fatal(err)
					}
					last, _ := json.Marshal(lookupPathValue(env, "error.details.last_error"))
					want, _ := json.Marshal(expected)
					if string(last) != string(want) {
						t.Fatalf("API error changed: %s", last)
					}
					app.writeError(e, defaultOptions())
					if !strings.Contains(stderr.String(), "GET "+tc.recovery) || !strings.Contains(stderr.String(), "A retry with the same idempotency key is safe and returns the original result.") {
						t.Fatal(stderr.String())
					}
				})
			})
		}
	}
}

func TestRequestBudgetRetriesKeepRemainingTimeKeyAndBody(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	synctest.Test(t, func(t *testing.T) {
		start, attempts := time.Now(), 0
		app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			raw, _ := io.ReadAll(r.Body)
			deadline, _ := r.Context().Deadline()
			if string(raw) != `{"amount":100}` || r.Header.Get("Idempotency-Key") != "saved-action" || deadline != start.Add(80*time.Second) {
				t.Fatalf("body=%s key=%s remaining=%s", raw, r.Header.Get("Idempotency-Key"), time.Until(deadline))
			}
			if attempts == 1 {
				time.Sleep(12 * time.Second)
				return outcomeResponse(503, "req_retry", `{"error":{"code":"SERVICE_UNAVAILABLE","remediation":{"retry":"same_request"}}}`), nil
			}
			return outcomeResponse(200, "req_success", `{"data":{}}`), nil
		})}
		response, e := app.doRequestWithin(t.Context(), 80*time.Second, "http://127.0.0.1:1", "", "POST", "/v1/payment-intents/pi_fixture/capture", []byte(`{"amount":100}`), "saved-action", false)
		if e != nil || response == nil || attempts != 2 || time.Since(start) < 12*time.Second {
			t.Fatalf("attempts=%d e=%+v", attempts, e)
		}
	})
}

func TestRequestOutcomeTimeoutBodyAndCancellation(t *testing.T) {
	for _, failure := range []string{"headers", "body", "cancel", "transport"} {
		t.Run(failure, func(t *testing.T) {
			app, _, stderr := testApp(t, "http://127.0.0.1:1")
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					attempts++
					switch failure {
					case "cancel":
						time.AfterFunc(time.Second, cancel)
						<-r.Context().Done()
						return nil, r.Context().Err()
					case "transport":
						return nil, io.ErrUnexpectedEOF
					case "body":
						response := outcomeResponse(200, "req_body", "")
						response.Body = contextResponseBody{r.Context()}
						return response, nil
					default:
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
				})}
				_, e := app.doRequestWithin(ctx, 3*time.Second, "http://127.0.0.1:1", "", "POST", "/v1/payment-intents/pi_fixture/confirm", []byte(`{}`), "saved-action", false)
				wantCode, wantAttempts := "CLIENT_TIMEOUT", 1
				if failure == "cancel" {
					wantCode = "CLIENT_CANCELED"
				}
				if failure == "transport" {
					wantAttempts = 3
				}
				if e == nil || e.Code != wantCode || attempts != wantAttempts || errorRetryable(e) {
					t.Fatalf("attempts=%d e=%+v", attempts, e)
				}
				envelope := mcpCLIErrorEnvelope(e)
				for path, want := range map[string]any{"error.details.outcome": "unknown", "error.details.retry": "none", "error.remediation.retry": "none", "error.details.attempt_count": float64(wantAttempts), "error.details.idempotency_key": "saved-action", "error.retryable": false} {
					if got := lookupPathValue(envelope, path); got != want {
						t.Errorf("%s=%v want %v", path, got, want)
					}
				}
				if got := lookupPathValue(envelope, "error.details.recovery_reads.0.path"); got != "/v1/payment-intents/pi_fixture" {
					t.Fatal(envelope)
				}
				if failure == "body" && e.RequestID != "req_body" {
					t.Fatal(e)
				}
				app.writeError(e, defaultOptions())
				if !strings.Contains(stderr.String(), "Attempts (") || !strings.Contains(stderr.String(), "Its outcome is unknown") || !strings.Contains(stderr.String(), "GET /v1/payment-intents/pi_fixture") {
					t.Fatal(stderr.String())
				}
			})
		})
	}
}

// Adapted from the triage's timeout-then-IN_PROGRESS reproduction. The fake
// transport times out early, leaving time for a same-key recovery attempt.
func TestRequestOutcomeRetainsTimeoutBeforeInProgress(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		const apiError = `{"type":"api_error","code":"IDEMPOTENCY_KEY_IN_PROGRESS","message":"in progress","remediation":{"retry":"same_request","next_steps":"Wait before sending the identical request."}}`
		app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			raw, _ := io.ReadAll(r.Body)
			if r.Header.Get("Idempotency-Key") != "saved-action" || string(raw) != `{}` {
				t.Fatal("key or body changed")
			}
			if attempts == 1 {
				time.Sleep(2 * time.Second)
				return nil, context.DeadlineExceeded
			}
			response := outcomeResponse(409, "req_in_progress", `{"error":`+apiError+`}`)
			response.Header.Set("Retry-After", "110")
			return response, nil
		})}
		_, e := app.doRequestWithin(t.Context(), 90*time.Second, "http://127.0.0.1:1", "", "POST", "/v1/orders/ord_fixture/pay", []byte(`{}`), "saved-action", false)
		if e == nil || e.Code != "CLIENT_TIMEOUT" || e.HTTPStatus != 409 || attempts != 2 || !errors.Is(e.Cause, context.DeadlineExceeded) {
			t.Fatalf("attempts=%d e=%+v", attempts, e)
		}
		envelope := mcpCLIErrorEnvelope(e)
		for path, want := range map[string]any{
			"error.details.attempts.0.duration_ms": float64(2000), "error.details.attempts.0.error_code": "CLIENT_TIMEOUT",
			"error.details.attempts.1.error_code": "IDEMPOTENCY_KEY_IN_PROGRESS", "error.details.request_ids.0": "req_in_progress",
			"error.details.last_error.remediation.retry": "same_request", "error.remediation.retry": "none",
			"error.details.recovery_reads.0.path": "/v1/orders/ord_fixture", "error.details.recovery_reads.1.path": "/v1/orders/ord_fixture/payment-attempts?idempotency_key=saved-action",
		} {
			if got := lookupPathValue(envelope, path); got != want {
				t.Errorf("%s=%v want %v", path, got, want)
			}
		}
		original, _ := json.Marshal(lookupPathValue(envelope, "error.details.last_error"))
		var expected any
		json.Unmarshal([]byte(apiError), &expected)
		want, _ := json.Marshal(expected)
		if string(original) != string(want) {
			t.Fatalf("API error changed: %s", original)
		}
		result := mcpCommandError(&Command{Mutation: true}, mcpCallResult{value: envelope, exit: e.ExitCode}, mcpPolicy{}, "")
		if text := mcpResultText(result); strings.Contains(text, "To retry, call the tool again") || !strings.Contains(text, "outcome is unknown") {
			t.Fatal(text)
		}
	})
}

func TestRequestRecoveryReadsUseOperations(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"POST", "/v1/payment-intents/pi_fixture/confirm", "/v1/payment-intents/pi_fixture"},
		{"POST", "/v1/payment-intents/pi_fixture/capture", "/v1/payment-intents/pi_fixture"},
		{"POST", "/v1/orders/ord_fixture/pay", "/v1/orders/ord_fixture/payment-attempts?idempotency_key=saved%26action"},
		{"POST", "/v1/refunds", "/v1/refunds?idempotency_key=saved%26action"},
		{"GET", "/v1/refunds", ""},
	} {
		t.Run(tc.path+tc.method, func(t *testing.T) {
			reads := recoveryReads(tc.method, tc.path, "saved&action")
			if tc.want == "" {
				if len(reads) != 0 {
					t.Fatal(reads)
				}
				return
			}
			if len(reads) == 0 || reads[len(reads)-1]["path"] != tc.want {
				t.Fatal(reads)
			}
		})
	}
}

func TestRequestOutcomeRequestIDsAndRetryStops(t *testing.T) {
	for _, retry := range []string{"same_request", "none", "after_refresh"} {
		t.Run(retry, func(t *testing.T) {
			app, _, _ := testApp(t, "http://127.0.0.1:1")
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					attempts++
					if attempts == 1 {
						response := outcomeResponse(200, "req_partial", "")
						response.Body = failingResponseBody{}
						return response, nil
					}
					return outcomeResponse(503, "req_final", fmt.Sprintf(`{"error":{"type":"api_error","code":"SERVICE_UNAVAILABLE","message":"unavailable","remediation":{"retry":%q}}}`, retry)), nil
				})}
				_, e := app.doRequestWithin(t.Context(), 80*time.Second, "http://127.0.0.1:1", "", "POST", "/v1/refunds", []byte(`{}`), "saved-action", false)
				want := 2
				if retry == "same_request" {
					want = 3
				}
				if e == nil || attempts != want || e.Code != "CLIENT_TIMEOUT" {
					t.Fatalf("attempts=%d e=%+v", attempts, e)
				}
				env := mcpCLIErrorEnvelope(e)
				if lookupPathValue(env, "error.details.request_ids.0") != "req_partial" || lookupPathValue(env, "error.details.request_ids.1") != "req_final" || lookupPathValue(env, "error.details.last_error.remediation.retry") != retry {
					t.Fatal(env)
				}
			})
		})
	}
}

func TestRequestBudgetFederationNoKeyWriteRunsOnce(t *testing.T) {
	var exchanges, writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			writeExchange(w, int(exchanges.Add(1)))
			return
		}
		writes.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"type":"authentication_error","code":"INVALID_CREDENTIAL","message":"expired","remediation":{"retry":"none"}}}`)
	}))
	defer server.Close()
	app := federationApp(t, server.URL)
	_, e := app.doRequest(t.Context(), server.URL, workloadCredential, "POST", "/v1/customers", []byte(`{}`), "", false)
	if e == nil || writes.Load() != 1 || exchanges.Load() != 1 {
		t.Fatalf("writes=%d exchanges=%d error=%+v", writes.Load(), exchanges.Load(), e)
	}
}

func TestRequestOutcomeRetainsAmbiguityBeforeRefusedAttempt(t *testing.T) {
	for _, firstFailure := range []string{"timeout", "server_error"} {
		for _, status := range []int{400, 401, 403, 409, 429, 503} {
			t.Run(firstFailure+"/"+fmt.Sprint(status), func(t *testing.T) {
				app, _, _ := testApp(t, "http://127.0.0.1:1")
				synctest.Test(t, func(t *testing.T) {
					attempts := 0
					app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						attempts++
						if attempts == 1 {
							if firstFailure == "timeout" {
								return nil, context.DeadlineExceeded
							}
							return outcomeResponse(500, "req_uncertain", `{"error":{"type":"api_error","code":"INTERNAL_ERROR","message":"uncertain","remediation":{"retry":"same_request"}}}`), nil
						}
						response := outcomeResponse(status, "req_refused", `{"error":{"type":"api_error","code":"TEST","message":"refused","remediation":{"retry":"none"}}}`)
						return response, nil
					})}
					_, e := app.doRequestWithin(t.Context(), 80*time.Second, "http://127.0.0.1:1", "", "POST", "/v1/orders/ord_fixture/pay", []byte(`{}`), "saved-action", false)
					if e == nil || e.Code != "CLIENT_TIMEOUT" || attempts != 2 || lookupPathValue(mcpCLIErrorEnvelope(e), "error.details.last_error.remediation.retry") != "none" {
						t.Fatalf("attempts=%d e=%+v", attempts, e)
					}
				})
			})
		}
	}
}

func TestRequestBudgetFederationObeysServerRetry(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		for _, retry := range []string{"none", "after_refresh", "same_request", ""} {
			t.Run(method+"/"+retry, func(t *testing.T) {
				var exchanges, requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/oauth/token" {
						writeExchange(w, int(exchanges.Add(1)))
						return
					}
					requests.Add(1)
					if method == "POST" && r.Header.Get("Idempotency-Key") != "saved-action" {
						t.Error("key changed on credential refresh")
					}
					w.WriteHeader(401)
					if retry != "" {
						fmt.Fprintf(w, `{"error":{"code":"TEST","remediation":{"retry":%q}}}`, retry)
					}
				}))
				defer server.Close()
				app := federationApp(t, server.URL)
				key := ""
				if method == "POST" {
					key = "saved-action"
				}
				_, e := app.doRequestWithin(t.Context(), 10*time.Second, server.URL, workloadCredential, method, "/v1/customers", nil, key, false)
				want := int32(1)
				if retry == "same_request" {
					want = 2
				}
				if e == nil || e.HTTPStatus != 401 || requests.Load() != want || exchanges.Load() != want {
					t.Fatalf("requests=%d exchanges=%d e=%+v", requests.Load(), exchanges.Load(), e)
				}
			})
		}
	}
}
