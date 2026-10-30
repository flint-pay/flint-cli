package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
					if time.Until(deadline) != 80*time.Second {
						t.Fatal("attempt did not get the remaining budget", time.Until(deadline))
					}
					select {
					case <-time.After(11 * time.Second):
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
				if e != nil || response == nil || attempts != 1 || time.Since(start) != 11*time.Second {
					t.Fatalf("attempts=%d elapsed=%s err=%+v", attempts, time.Since(start), e)
				}
			})
		})
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
