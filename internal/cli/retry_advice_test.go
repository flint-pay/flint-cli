package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Reuses the triage's three-remedy fixture, checking terminal and MCP output.
func TestRetryAdviceFollowsRemediation(t *testing.T) {
	cmd, _ := NewRegistry().ByName("customers.create")
	for _, tc := range []struct{ retry, code string }{
		{"same_request", "IDEMPOTENCY_KEY_IN_PROGRESS"},
		{"after_refresh", "VERSION_CONFLICT"},
		{"none", "IDEMPOTENCY_KEY_REUSED"},
		{"none", "IDEMPOTENCY_RECOVERY_REQUIRED"},
		{"", "IDEMPOTENCY_KEY_REUSED"},
	} {
		t.Run(tc.retry+tc.code, func(t *testing.T) {
			app, out, stderr := testApp(t, "http://127.0.0.1:1")
			serverError := map[string]any{"type": "conflict_error", "code": tc.code, "message": "fixture error", "details": []any{map[string]any{"reason": "fixture"}}, "remediation": map[string]any{"retry": tc.retry, "next_steps": "Inspect the current resource before deciding what to do."}}
			e := apiErrorFromResponse(&apiResponse{Status: 409, Value: map[string]any{"error": serverError}})
			withIdempotencyRetryContext(e, "saved-action")
			opts := defaultOptions()
			app.writeError(e, opts)
			human := stderr.String()
			if !strings.Contains(human, "Inspect the current resource") || !strings.Contains(human, "Idempotency key: saved-action") {
				t.Fatal(human)
			}
			opts.Output = "json"
			app.writeError(e, opts)
			env := mcpCLIErrorEnvelope(e)
			if lookupPathValue(env, "error.remediation.retry") != tc.retry || lookupPathValue(env, "error.details.idempotency_key") != "saved-action" || lookupPathValue(env, "error.details.problems.0.reason") != "fixture" {
				t.Fatal(out.String())
			}
			mcp := mcpResultText(mcpCommandError(cmd, mcpCallResult{value: env, exit: e.ExitCode}, mcpPolicy{}, ""))
			for _, text := range []string{human, mcp} {
				switch tc.retry {
				case "same_request":
					if !strings.Contains(text, "identical request") || !strings.Contains(text, "backoff") || !strings.Contains(text, "saved-action") || strings.Contains(text, "use a new") {
						t.Fatal(text)
					}
				case "after_refresh":
					if !strings.Contains(text, "rebuild the request") || !strings.Contains(text, "use a new") || strings.Contains(text, "resend the identical") {
						t.Fatal(text)
					}
				default:
					if strings.Contains(text, "resend the identical") || strings.Contains(text, "use a new") || strings.Contains(text, "To retry, call") || strings.Contains(text, "reuse --idempotency-key") {
						t.Fatal(text)
					}
				}
			}
			if got := lookupPathValue(serverError, "remediation.retry"); got != tc.retry {
				t.Fatal("server error mutated", serverError)
			}
		})
	}
}

func TestRetryAdviceUnsentFailuresKeepNoKey(t *testing.T) {
	for _, failure := range []string{"validation", "confirmation", "scope", "preview output"} {
		t.Run(failure, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth-context" {
					if failure == "scope" {
						fmt.Fprint(w, limitedAuthContextJSON("sandbox"))
					} else {
						fmt.Fprint(w, authContextJSON("sandbox"))
					}
					return
				}
				writes++
				fmt.Fprint(w, `{"data":{}}`)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			args := []string{"customers", "create", "--input", `{"not_a_field":true}`, "--idempotency-key", "never-sent"}
			switch failure {
			case "confirmation":
				args = []string{"webhook-endpoints", "delete", "whep_fixture", "--idempotency-key", "never-sent"}
			case "scope":
				args = []string{"customers", "create", "--email", "fixture@example.com", "--idempotency-key", "never-sent"}
			case "preview output":
				args = []string{"customers", "create", "--email", "fixture@example.com", "--dry-run=client", "--field", "definitely_missing", "--idempotency-key", "never-sent"}
			}
			exit, env := runJSON(t, app, out, args...)
			if exit == ExitOK || writes != 0 || lookupPathValue(env, "error.details.idempotency_key") != nil || strings.Contains(out.String(), "never-sent") {
				t.Fatalf("writes=%d exit=%d out=%s", writes, exit, out)
			}
			var e *CLIError
			if failure == "validation" {
				e = usageError("INVALID_REQUEST_BODY", "Invalid input.", "input")
			} else {
				e = cliError(ExitConfirmation, "confirmation_required", "CONFIRMATION_REQUIRED", "Confirm this operation.")
			}
			withIdempotencyRetryContext(e, "never-sent")
			app.writeError(e, defaultOptions())
			if strings.Contains(stderr.String(), "never-sent") || strings.Contains(stderr.String(), "idempotency") {
				t.Fatal(stderr.String())
			}
			cmd, _ := NewRegistry().ByName("customers.create")
			result := mcpCommandError(cmd, mcpCallResult{value: env, exit: exit}, mcpPolicy{}, "")
			if strings.Contains(mcpResultText(result), "never-sent") || strings.Contains(mcpResultText(result), "_flint.idempotency_key") {
				t.Fatal(result)
			}
		})
	}
}

func TestRetryAdviceNoKeyRoutes(t *testing.T) {
	for _, name := range []string{"customers.get", "discount-previews.create"} {
		t.Run(name, func(t *testing.T) {
			app, out, stderr := testApp(t, "http://127.0.0.1:1")
			cmd, _ := NewRegistry().ByName(name)
			opts := defaultOptions()
			opts.IdempotencyKey = "ignored-key"
			key, e := requestIdempotencyKey(cmd, opts)
			if e != nil || key != "" {
				t.Fatal(key, e)
			}
			apiError := map[string]any{"type": "api_error", "code": "RATE_LIMIT_EXCEEDED", "message": "busy", "remediation": map[string]any{"retry": "same_request", "next_steps": "Wait before sending the identical request."}}
			e = apiErrorFromResponse(&apiResponse{Status: 429, Value: map[string]any{"error": apiError}})
			withIdempotencyRetryContext(e, key)
			app.writeError(e, opts)
			opts.Output = "json"
			app.writeError(e, opts)
			env := mcpCLIErrorEnvelope(e)
			text := stderr.String() + out.String() + mcpResultText(mcpCommandError(cmd, mcpCallResult{value: env, exit: e.ExitCode}, mcpPolicy{}, ""))
			if strings.Contains(text, "idempotency") || strings.Contains(text, "ignored-key") || !strings.Contains(text, "Wait before") {
				t.Fatal(text)
			}
		})
	}
}

func TestRetryAdviceBeforeTransportDispatch(t *testing.T) {
	for _, failure := range []string{"canceled", "invalid header", "dial"} {
		t.Run(failure, func(t *testing.T) {
			app, _, _ := testApp(t, "http://127.0.0.1:1")
			calls := 0
			app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}
			})}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			key := "never-sent"
			if failure == "canceled" {
				cancel()
			}
			if failure == "invalid header" {
				key += "\ninvalid"
			}
			_, e := app.doRequest(ctx, "http://127.0.0.1:1", "", "POST", "/v1/customers", nil, key, false)
			if e == nil || e.requestSent || lookupPathValue(mcpCLIErrorEnvelope(e), "error.details.idempotency_key") != nil {
				t.Fatal(e)
			}
			if failure != "dial" && calls != 0 {
				t.Fatal("transport reached", calls)
			}
		})
	}
}

func TestRetryAdviceKeepsKeyAfterSentOutputFailure(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		writes++
		w.WriteHeader(201)
		fmt.Fprint(w, `{"data":{"customer_id":"cus_fixture"},"request_id":"req_committed"}`)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	exit, env := runJSON(t, app, out, "customers", "create", "--email", "fixture@example.com", "--field", "definitely_missing", "--idempotency-key", "saved-action")
	if exit == ExitOK || writes != 1 || lookupPathValue(env, "error.details.idempotency_key") != "saved-action" || strings.Contains(out.String(), "resend") {
		t.Fatalf("writes=%d exit=%d out=%s", writes, exit, out)
	}
}

func TestRetryAdviceCanonicalWireKeys(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"customers.create", "refunds.create"} {
		cmd, _ := registry.ByName(name)
		opts := defaultOptions()
		opts.IdempotencyKey = " \tsaved-action \t"
		if key, e := requestIdempotencyKey(cmd, opts); e != nil || key != "saved-action" {
			t.Fatalf("%s key=%q error=%+v", name, key, e)
		}
	}
	cmd, _ := registry.ByName("refunds.create")
	opts := defaultOptions()
	opts.IdempotencyKey = " \t "
	if _, e := requestIdempotencyKey(cmd, opts); e == nil {
		t.Fatal("blank required key accepted")
	}

	writes, commits := 0, 0
	firstKey := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth-context" {
			w.Header().Set("Connection", "close")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		writes++
		key := r.Header.Get("Idempotency-Key")
		if writes == 1 {
			firstKey = key
			commits++
		} else if key == "" || key != firstKey {
			commits++
		}
		// The write commits but its response is lost. Only a stable nonempty wire
		// key can prevent the retry from committing it again.
		connection, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		connection.Close()
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	app.HTTPClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	// Blank explicit keys are rejected before dispatch by the flag parser.
	exit, env := runJSON(t, app, out, "customers", "create", "--email", "fixture@example.com", "--idempotency-key", " \t ")
	if exit != ExitUsage || writes != 0 || lookupPathValue(env, "error.code") != "INVALID_FLAG_VALUE" || lookupPathValue(env, "error.details.idempotency_key") != nil {
		t.Fatalf("writes=%d exit=%d out=%s", writes, exit, out)
	}
	exit, env = runJSON(t, app, out, "customers", "create", "--email", "fixture@example.com", "--idempotency-key", " \tsaved-action \t")
	if exit != ExitNetwork || writes != 3 || commits != 1 || firstKey != "saved-action" || lookupPathValue(env, "error.details.idempotency_key") != firstKey {
		t.Fatalf("writes=%d commits=%d key=%q exit=%d out=%s", writes, commits, firstKey, exit, out)
	}
}
