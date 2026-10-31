package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestRequiredIdempotencyKeyFailsBeforeAnyRequest(t *testing.T) {
	for _, argv := range [][]string{
		{"refunds", "create", "--payment-intent", "pi_test", "--refund-type", "full"},
		{"orders", "pay", "ord_test", "--input", `{"action":"pay"}`},
		{"payment-intents", "create", "--amount", "2500", "--currency", "USD", "--payment-option", "card"},
		{"orders", "payment-intents", "create", "ord_test"},
		{"api", "POST", "/v1/refunds", "--input", `{}`},
	} {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprint(argv, dryRun), func(t *testing.T) {
				app, out, _ := testApp(t, "https://api.example.com")
				app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Fatal("missing required key sent a request, including authentication")
					return nil, nil
				})}
				args := append([]string(nil), argv...)
				if dryRun {
					args = append(args, "--dry-run=client")
				}
				exit, result := runJSON(t, app, out, args...)
				code, _ := lookupPath(result, "error.code")
				param, _ := lookupPath(result, "error.param")
				message, _ := lookupPath(result, "error.message")
				if exit != ExitUsage || code != "IDEMPOTENCY_KEY_REQUIRED" || param != "--idempotency-key" || !strings.Contains(fmt.Sprint(message), "reuse the same key on retry") {
					t.Fatalf("exit=%d result=%v", exit, result)
				}
			})
		}
	}
}

func TestPreparedRequestIdempotencyClasses(t *testing.T) {
	for _, tc := range []struct{ name, class string }{
		{"customers.create", "optional"}, {"refunds.create", "required"}, {"discount-previews.create", "none"}, {"customers.get", "none"},
	} {
		for _, key := range []string{"", "saved-action-1"} {
			t.Run(tc.name+"/"+key, func(t *testing.T) {
				app, _, _ := testApp(t, "")
				cmd, ok := app.Registry.ByName(tc.name)
				if !ok {
					t.Fatal("missing command", tc.name)
				}
				if got := commandIdempotencyClass(cmd); got != tc.class {
					t.Fatalf("class=%s want %s", got, tc.class)
				}
				opts := defaultOptions()
				opts.IdempotencyKey = key
				opts.Input = jsonString(t, minimalRequestBody(t, cmd))
				for _, arg := range cmd.Arguments {
					if arg.Positional > 0 {
						opts.Positionals = append(opts.Positionals, arg.IDPrefix+"test")
					}
				}
				req, err := app.prepareRequest(t.Context(), cmd, opts, ResolvedConfig{}, "", "")
				if tc.class == "required" && key == "" {
					if err == nil || err.Code != "IDEMPOTENCY_KEY_REQUIRED" {
						t.Fatalf("error=%v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				switch tc.class {
				case "none":
					if req.IdempotencyKey != "" {
						t.Fatalf("none route sent %q", req.IdempotencyKey)
					}
				case "optional":
					if key == "" && (!strings.HasPrefix(req.IdempotencyKey, "flint-cli-") || len(req.IdempotencyKey) != len("flint-cli-")+32) {
						t.Fatalf("generated key=%q", req.IdempotencyKey)
					}
				}
				if tc.class != "none" && key != "" && req.IdempotencyKey != key {
					t.Fatalf("key=%q", req.IdempotencyKey)
				}
			})
		}
	}
}

func TestEveryMCPToolRequiresOnlyItsContractKey(t *testing.T) {
	for _, cmd := range NewRegistry().Commands {
		if cmd.Local || cmd.CanonicalName == "api" {
			continue
		}
		t.Run(cmd.CanonicalName, func(t *testing.T) {
			operation, _ := openAPIOperationByID(cmd.OperationID)
			required := false
			for _, parameter := range operation.Parameters {
				if parameter["name"] == "Idempotency-Key" && parameter["required"] == true {
					required = true
				}
			}
			schema, err := schemaForMCPTool(cmd, mcpPolicy{})
			if err != nil {
				t.Fatal(err)
			}
			schema = deepCopyMap(schema)
			// Check execution controls independently of each operation's request body.
			delete(schema, "allOf")
			delete(schema, "oneOf")
			delete(schema, "anyOf")
			props := schema["properties"].(map[string]any)
			controls, _ := props["idempotency_key"].(map[string]any)
			rootRequired := []any{}
			if required {
				rootRequired = append(rootRequired, "idempotency_key")
			}
			requiredNames := mcpStrings(schema["required"])
			found := false
			for _, name := range requiredNames {
				found = found || name == "idempotency_key"
			}
			if found != required {
				t.Fatalf("root required=%v, required key=%v", requiredNames, required)
			}
			schema = map[string]any{"type": "object", "required": rootRequired, "properties": map[string]any{"idempotency_key": controls}}
			compiler := jsonschema.NewCompiler()
			if err := compiler.AddResource("https://test.invalid/schema", schema); err != nil {
				t.Fatal(err)
			}
			compiled, compileErr := compiler.Compile("https://test.invalid/schema")
			if compileErr != nil {
				t.Fatal(compileErr)
			}
			if validationErr := compiled.Validate(map[string]any{}); (validationErr != nil) != required {
				t.Fatalf("missing key: required=%v error=%v", required, validationErr)
			}
			if required && compiled.Validate(map[string]any{"idempotency_key": ""}) == nil {
				t.Fatal("accepted an empty required key")
			}
			if required {
				if validationErr := compiled.Validate(map[string]any{"idempotency_key": "saved-action-1"}); validationErr != nil {
					t.Fatal(validationErr)
				}
			}
		})
	}
}

func TestRetrySignalsAndReplaySafety(t *testing.T) {
	for _, methodKey := range []struct{ method, key string }{{"GET", ""}, {"POST", "saved-action-1"}, {"POST", ""}} {
		for _, tc := range []struct {
			name   string
			status int
			body   string
			retry  bool
		}{
			{"in_progress", 409, `{"error":{"code":"IDEMPOTENCY_KEY_IN_PROGRESS","remediation":{"retry":"same_request"}}}`, true},
			{"rate_limit", 429, `{"error":{"code":"RATE_LIMIT_EXCEEDED","remediation":{"retry":"same_request"}}}`, true},
			{"internal", 500, `{"error":{"code":"INTERNAL_ERROR","remediation":{"retry":"same_request"}}}`, true},
			{"unavailable", 503, `{"error":{"code":"PAYMENT_OPTION_UNAVAILABLE","details":[{"reason":"platform_funding_unavailable"}],"remediation":{"retry":"same_request"}}}`, true},
			{"caller_action", 429, `{"error":{"code":"RATE_LIMIT_EXCEEDED","remediation":{"retry":"none"}}}`, false},
			{"uncertain", 500, `{"error":{"code":"IDEMPOTENCY_RECOVERY_REQUIRED","remediation":{"retry":"none"}}}`, false},
			{"refresh", 409, `{"error":{"code":"VERSION_CONFLICT","remediation":{"retry":"after_refresh"}}}`, false},
			{"refresh_503", 503, `{"error":{"code":"TEST","remediation":{"retry":"after_refresh"}}}`, false},
			{"missing_signal", 503, `{"error":{"code":"TEST"}}`, false},
			{"invalid_signal", 503, `{"error":{"remediation":{"retry":true}}}`, false},
			{"proxy429", 429, `<html>busy</html>`, true}, {"proxy500", 500, `{}`, true},
			{"proxy502", 502, `bad gateway`, true}, {"proxy503", 503, ``, true}, {"proxy504", 504, `null`, true},
			{"proxy501", 501, `not implemented`, false}, {"validation", 400, `{"error":{"remediation":{"retry":"none"}}}`, false},
		} {
			t.Run(methodKey.method+"/"+methodKey.key+"/"+tc.name, func(t *testing.T) {
				app, out, _ := testApp(t, "")
				synctest.Test(t, func(t *testing.T) {
					attempts := 0
					app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(req *http.Request) (*http.Response, error) {
						attempts++
						if req.Header.Get("Idempotency-Key") != methodKey.key {
							t.Fatal("key changed between attempts")
						}
						return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
					})}
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					_, err := app.doRequest(ctx, "https://api.example.com", "", methodKey.method, "/v1/test", []byte(`{}`), methodKey.key, false)
					wantRetry := tc.retry && (methodKey.method == "GET" || methodKey.key != "")
					wantAttempts := 1
					if wantRetry {
						wantAttempts = 3
					}
					wantRetryable := wantRetry
					if methodKey.method == "POST" && methodKey.key != "" && (tc.status >= 500 || tc.name == "in_progress") {
						wantRetryable = false // Exhausted writes require recovery first.
					}
					if err == nil || attempts != wantAttempts || errorRetryable(err) != wantRetryable {
						t.Fatalf("attempts=%d want=%d error=%+v", attempts, wantAttempts, err)
					}
					opts := defaultOptions()
					opts.Output = "json"
					app.writeError(err, opts)
					if !strings.Contains(out.String(), fmt.Sprintf(`"retryable":%t`, wantRetryable)) {
						t.Fatal(out.String())
					}
					if methodKey.key != "" && !strings.Contains(out.String(), `"idempotency_key":"saved-action-1"`) {
						t.Fatal(out.String())
					}
				})
			})
		}
	}
}

func TestTransportRetriesTimeoutsAndCancellation(t *testing.T) {
	for _, methodKey := range []struct{ method, key string }{{"GET", ""}, {"POST", "saved-action-1"}, {"POST", ""}} {
		for _, failure := range []string{"connection", "body", "timeout", "cancel", "transport_cancel"} {
			t.Run(methodKey.method+"/"+methodKey.key+"/"+failure, func(t *testing.T) {
				app, _, _ := testApp(t, "")
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					attempts := 0
					app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(req *http.Request) (*http.Response, error) {
						attempts++
						if req.Header.Get("Idempotency-Key") != methodKey.key {
							t.Fatal("key changed")
						}
						switch failure {
						case "connection":
							return nil, errors.New("connection reset")
						case "body":
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: failingResponseBody{}}, nil
						case "transport_cancel":
							return nil, context.Canceled
						case "timeout":
							<-req.Context().Done()
							return nil, req.Context().Err()
						default:
							cancel()
							return nil, ctx.Err()
						}
					})}
					_, err := app.doRequest(ctx, "https://api.example.com", "", methodKey.method, "/v1/test", []byte(`{}`), methodKey.key, false)
					want := 1
					if !strings.Contains(failure, "cancel") && failure != "timeout" && (methodKey.method == "GET" || methodKey.key != "") {
						want = 3
					}
					if err == nil || attempts != want {
						t.Fatalf("attempts=%d want=%d error=%v", attempts, want, err)
					}
					if strings.Contains(failure, "cancel") && errorRetryable(err) {
						t.Fatal("cancellation is retryable")
					}
				})
			})
		}
	}
}

func TestRetryAfterDeadlinePreservesLastError(t *testing.T) {
	for _, header := range []string{"120", "date"} {
		t.Run(header, func(t *testing.T) {
			app, out, _ := testApp(t, "")
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				started := time.Now()
				retryAfter := header
				if header == "date" {
					retryAfter = started.Add(120 * time.Second).UTC().Format(http.TimeFormat)
				}
				app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
					attempts++
					return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{retryAfter}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","code":"RATE_LIMIT_EXCEEDED","message":"wait","remediation":{"retry":"same_request"}}}`))}, nil
				})}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				_, err := app.doRequest(ctx, "https://api.example.com", "", "POST", "/v1/test", nil, "saved-action-1", false)
				if err == nil || err.Code != "RATE_LIMIT_EXCEEDED" || err.HTTPStatus != 429 || err.Type != "rate_limit_error" || err.RetryAfterMs == nil || *err.RetryAfterMs != 120000 || attempts != 1 || time.Since(started) != 0 {
					t.Fatalf("attempts=%d elapsed=%s error=%+v", attempts, time.Since(started), err)
				}
				opts := defaultOptions()
				opts.Output = "json"
				app.writeError(err, opts)
				for _, want := range []string{`"retry_after_ms":120000`, `"retry":"same_request"`, `"idempotency_key":"saved-action-1"`} {
					if !strings.Contains(out.String(), want) {
						t.Fatal(out.String())
					}
				}
			})
		})
	}
}

func TestResponseReadFailurePreservesRetryAfterBeyondDeadline(t *testing.T) {
	app, out, _ := testApp(t, "")
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		started := time.Now()
		app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(&listenTerminalErrorReader{reader: strings.NewReader(`{"error":`), err: io.ErrUnexpectedEOF})}, nil
		})}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err := app.doRequest(ctx, "https://api.example.com", "", http.MethodGet, "/v1/test", nil, "", false)
		if err == nil || err.Code != "CLIENT_TIMEOUT" || err.HTTPStatus != 429 || !errors.Is(err.Cause, io.ErrUnexpectedEOF) || err.RetryAfterMs == nil || *err.RetryAfterMs != 120000 || attempts != 1 || time.Since(started) != 0 {
			t.Fatalf("attempts=%d elapsed=%s error=%+v", attempts, time.Since(started), err)
		}
		opts := defaultOptions()
		opts.Output = "json"
		app.writeError(err, opts)
		if !strings.Contains(out.String(), `"retry_after_ms":120000`) {
			t.Fatal(out.String())
		}
	})
}

func TestRetryBackoffAndRetryAfter(t *testing.T) {
	for attempt := 1; attempt <= 6; attempt++ {
		base := min(500*time.Millisecond*time.Duration(1<<(attempt-1)), 5*time.Second)
		for i := 0; i < 20; i++ {
			delay := retryDelay(attempt, "")
			if delay < base/2 || delay > base {
				t.Fatalf("attempt=%d delay=%s base=%s", attempt, delay, base)
			}
			if delay := retryDelay(attempt, "0"); delay < base/2 || delay > base {
				t.Fatalf("Retry-After 0 skipped backoff: %s", delay)
			}
			if delay := retryDelay(attempt, "10"); delay != 10*time.Second {
				t.Fatalf("Retry-After ignored: %s", delay)
			}
		}
	}
	app, _, _ := testApp(t, "")
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		start := time.Now()
		app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			status, body := 503, `{"error":{"remediation":{"retry":"same_request"}}}`
			if attempts == 2 {
				status, body = 200, `{"data":{}}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"2"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		response, err := app.doRequest(t.Context(), "https://api.example.com", "", "GET", "/v1/test", nil, "", false)
		if err != nil || response == nil || attempts != 2 || time.Since(start) != 2*time.Second {
			t.Fatalf("attempts=%d elapsed=%s error=%v", attempts, time.Since(start), err)
		}
	})
}

func TestKeyedWriteFailuresExposeOnlyDispatchedKey(t *testing.T) {
	for _, status := range []int{400, 401, 403, 409, 422, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			app, out, stderr := testApp(t, "")
			app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"api_error","code":"TEST","message":"test","details":[{"reason":"original"}],"remediation":{"retry":"none"}}}`))}, nil
			})}
			_, err := app.doRequest(t.Context(), "https://api.example.com", "", "POST", "/v1/test", nil, "saved-action-1", false)
			if err == nil || errorRetryable(err) {
				t.Fatalf("error=%v", err)
			}
			opts := defaultOptions()
			opts.Output = "json"
			app.writeError(err, opts)
			for _, want := range []string{`"idempotency_key":"saved-action-1"`, `"reason":"original"`, `"retry":"none"`} {
				if !strings.Contains(out.String(), want) {
					t.Fatal(out.String())
				}
			}
			opts.Output = "human"
			app.writeError(err, opts)
			if !strings.Contains(stderr.String(), "Idempotency key: saved-action-1") {
				t.Fatal(stderr.String())
			}
		})
	}
	for _, key := range []string{"", "saved-action-1"} {
		t.Run("local-validation/"+key, func(t *testing.T) {
			app, out, _ := testApp(t, "")
			args := []string{"customers", "create", "--input", `{"not_a_field":true}`, "--dry-run=client"}
			if key != "" {
				args = append(args, "--idempotency-key", key)
			}
			exit, result := runJSON(t, app, out, args...)
			actual, _ := lookupPath(result, "error.details.idempotency_key")
			if exit != ExitUsage || actual != nil {
				t.Fatalf("exit=%d result=%v", exit, result)
			}
		})
	}
}

func TestKeyAdvicePreservesExistingKeyAndAvoidsDuplicates(t *testing.T) {
	app, _, stderr := testApp(t, "")
	err := apiErrorFromResponse(&apiResponse{Status: 409, Value: map[string]any{"error": map[string]any{
		"type": "api_error", "code": "TEST", "message": "test",
		"details":     map[string]any{"idempotency_key": "saved-action-1"},
		"remediation": map[string]any{"retry": "none"},
	}}})
	for i := 0; i < 3; i++ {
		err = withIdempotencyRetryContext(err, "saved-action-1")
	}
	app.writeError(err, defaultOptions())
	if strings.Count(stderr.String(), "Idempotency key: saved-action-1") != 1 {
		t.Fatal(stderr.String())
	}
}
