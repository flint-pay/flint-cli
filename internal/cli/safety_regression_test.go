package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flint-pay/flint-cli/internal/webhooksigning"
)

func TestOAuthAuthorizationSafetyForCanonicalAndRawCommands(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for _, scenario := range []struct {
			name      string
			mode      string
			flags     []string
			wantExit  int
			wantCode  string
			wantCalls int32
		}{
			{"live_requires_acknowledgment", "live", nil, ExitAuth, "LIVE_ACKNOWLEDGEMENT_REQUIRED", 0},
			{"confirm_does_not_replace_live", "live", []string{"--confirm"}, ExitAuth, "LIVE_ACKNOWLEDGEMENT_REQUIRED", 0},
			{"live_requires_confirmation", "live", []string{"--live"}, ExitConfirmation, "PRODUCTION_CONFIRMATION_REQUIRED", 0},
			{"live_confirmed", "live", []string{"--live", "--confirm"}, ExitOK, "", 1},
			{"sandbox", "test", nil, ExitOK, "", 1},
			{"mode_mismatch", "test", []string{"--live", "--confirm"}, ExitAuth, "CREDENTIAL_MODE_MISMATCH", 0},
			{"dry_run", "live", []string{"--live", "--dry-run=client", "--idempotency-key", "review_oauth"}, ExitOK, "", 0},
			{"repeated_mode_cannot_hide_live", "live", []string{"--mode", "test"}, ExitAuth, "LIVE_ACKNOWLEDGEMENT_REQUIRED", 0},
		} {
			t.Run(fmt.Sprintf("raw=%v/%s", raw, scenario.name), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/v1/oauth/authorize" || r.URL.Query().Get("mode") != scenario.mode {
						t.Errorf("unexpected request: %s", r.URL)
					}
					if r.Header.Get("Authorization") != "Bearer synthetic_session" || r.Header.Get("Idempotency-Key") == "" {
						t.Error("authorization must send its session credential and mutation idempotency key")
					}
					w.Header().Set("Location", "https://example.com/callback?code=synthetic")
					w.WriteHeader(http.StatusFound)
				}))
				defer server.Close()
				app, out, stderr := testApp(t, server.URL)
				t.Setenv("FLINT_API_KEY", "")
				t.Setenv("FLINT_ACCESS_TOKEN", "synthetic_session")
				argv := []string{"oauth", "authorize", "--client-id", "papp_test", "--mode", scenario.mode, "--redirect-uri", "https://example.com/callback", "--response-type", "code", "--state", "test"}
				flags := scenario.flags
				if raw {
					path := "/v1/oauth/authorize?client_id=papp_test&mode=" + scenario.mode + "&redirect_uri=https%3A%2F%2Fexample.com%2Fcallback&response_type=code&state=test"
					if scenario.name == "repeated_mode_cannot_hide_live" {
						path += "&mode=test"
						flags = nil
					}
					argv = []string{"api", "get", path}
				}
				argv = append(argv, flags...)
				argv = append(argv, "--output", "json")
				if exit := app.Run(argv); exit != scenario.wantExit || calls.Load() != scenario.wantCalls || !strings.Contains(out.String(), scenario.wantCode) {
					t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, calls.Load(), out, stderr)
				}
				if scenario.name == "dry_run" && !strings.Contains(out.String(), `"Idempotency-Key":"review_oauth"`) {
					t.Fatalf("missing preview idempotency key: %s", out)
				}
			})
		}
	}
}

func TestMCPRawOAuthUsesProductionConfirmation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "https://example.com/callback?code=synthetic")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_ACCESS_TOKEN", "synthetic_session")
	command, _ := app.Registry.ByName("api")
	args := map[string]any{
		"method": "GET",
		"path":   "/v1/oauth/authorize?client_id=papp_test&mode=live&redirect_uri=https%3A%2F%2Fexample.com%2Fcallback&response_type=code&state=test",
		"_flint": map[string]any{"live": true},
	}
	result, _, exit := app.callMCPCommand(t.Context(), command, args, defaultOptions())
	if exit != ExitConfirmation || calls.Load() != 0 {
		t.Fatalf("exit=%d calls=%d result=%v", exit, calls.Load(), result)
	}
	args["_flint"].(map[string]any)["confirm"] = true
	result, _, exit = app.callMCPCommand(t.Context(), command, args, defaultOptions())
	if exit != ExitOK || calls.Load() != 1 {
		t.Fatalf("exit=%d calls=%d result=%v", exit, calls.Load(), result)
	}
}

func TestRawOAuthPathVariantsCannotSkipSafetyClassification(t *testing.T) {
	for _, path := range []string{"/v1/oauth/authorize/", "/v1/oauth/%61uthorize"} {
		t.Run(path, func(t *testing.T) {
			app, out, stderr := testApp(t, "http://127.0.0.1:1")
			t.Setenv("FLINT_ACCESS_TOKEN", "synthetic_session")
			app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("unconfirmed authorization must not reach the network")
				return nil, fmt.Errorf("unexpected request")
			})}
			exit := app.Run([]string{"api", "get", path + "?mode=live", "--live", "--output", "json"})
			wantExit, wantCode := ExitConfirmation, "PRODUCTION_CONFIRMATION_REQUIRED"
			if strings.HasSuffix(path, "/") {
				wantExit, wantCode = ExitUsage, "UNDOCUMENTED_API_OPERATION"
			}
			if exit != wantExit || !strings.Contains(out.String(), wantCode) {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
			}
		})
	}
}

func TestRawMutationPaginationFailsBeforeAnyRequest(t *testing.T) {
	for _, methodPath := range [][]string{
		{"post", "/v1/customers"},
		{"patch", "/v1/customers/cus_test"},
		{"delete", "/v1/webhook-endpoints/whep_test"},
		{"get", "/v1/oauth/authorize?mode=live"},
	} {
		for _, flag := range []string{"--all", "--paginate"} {
			t.Run(strings.Join(methodPath, " ")+flag, func(t *testing.T) {
				app, out, stderr := testApp(t, "http://127.0.0.1:1")
				app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Error("pagination validation must precede all network requests")
					return nil, fmt.Errorf("unexpected request")
				})}
				argv := append([]string{"api"}, methodPath...)
				argv = append(argv, flag, "--output", "json", "--confirm", "--live")
				if exit := app.Run(argv); exit != ExitUsage || !strings.Contains(out.String(), "PAGINATION_REQUIRES_GET") {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
				}
			})
		}
	}
}

func TestDefaultHTTPClientReusesConnectionsAcrossAppCopies(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			io.WriteString(w, authContextJSON("sandbox"))
			return
		}
		io.WriteString(w, `{"data":[]}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	for i := 0; i < 10; i++ {
		child := *app
		if exit := child.Run([]string{"customers", "list", "--output", "json"}); exit != ExitOK {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
		}
	}
	if count := connections.Load(); count != 1 {
		t.Fatalf("20 sequential API requests opened %d connections, want 1", count)
	}
}

func TestSignupCancellationStopsBeforeKeyIssuance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/onboarding/start" {
			t.Errorf("signup continued after cancellation: %s", r.URL.Path)
		}
		cancel()
		io.WriteString(w, `{"data":{"verification_token":"synthetic"}}`)
	}))
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	app.Context = ctx
	app.StoreCredential = func(string, string) error { t.Error("canceled signup stored a credential"); return nil }
	if exit := app.Run(signupRegressionArgs()); exit != ExitNetwork || calls.Load() != 1 {
		t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, calls.Load(), out, stderr)
	}
}

func TestStreamAndHelpReuseDefaultConnections(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var connections atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "event: ready\ndata: {}\n\n")
				} else {
					io.WriteString(w, `[]`)
				}
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.Start()
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			for i := 0; i < 4; i++ {
				if stream {
					response, _, err := app.openListenStream(t.Context(), server.URL, "synthetic", "/v1/webhook-events/stream", "", time.Second, false)
					if err != nil {
						t.Fatal(err)
					}
					_, readErr := io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if readErr != nil {
						t.Fatal(readErr)
					}
				} else {
					if _, err := app.searchFlintHelp(server.URL, "test", defaultOptions()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if count := connections.Load(); count != 1 {
				t.Fatalf("opened %d connections, want 1", count)
			}
		})
	}
}

func TestLocalForwardingClosesItsPerDeliveryConnection(t *testing.T) {
	closed := make(chan struct{}, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	secret, err := webhooksigning.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		result := app.forwardListenPayload(t.Context(), time.Second, server.URL, secret, listenWebhookEvent{WebhookEventID: "whev_test", Payload: []byte(`{}`)})
		if result.err != nil || result.statusCode != http.StatusNoContent {
			t.Fatalf("forward result: %+v", result)
		}
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			t.Fatal("delivery left its private connection pool open")
		}
	}
}

type cancelSignupOnEOF struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r cancelSignupOnEOF) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err == io.EOF {
		r.cancel()
	}
	return n, err
}

func TestSignupCancellationAfterKeyIssuanceStillRevokesKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var revocations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/onboarding/start":
			io.WriteString(w, `{"data":{"verification_token":"synthetic"}}`)
		case "/v1/onboarding/verify-email":
			io.WriteString(w, `{"data":{"onboarding_session_token":"synthetic"}}`)
		case "/v1/onboarding/state":
			io.WriteString(w, `{"data":{"can_issue_api_key":true,"default_sandbox_id":"test_review"}}`)
		case "/v1/onboarding/api-key":
			io.WriteString(w, `{"data":{"secret_key":"flint_test_synthetic","api_key_id":"key_review","merchant_id":"mer_review"}}`)
		case "/v1/api-keys/key_review/revoke":
			if ctx.Err() == nil {
				t.Error("cleanup should run after cancellation")
			}
			revocations.Add(1)
			io.WriteString(w, `{"data":{"api_key_id":"key_review"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	app.Context = ctx
	baseClient := server.Client()
	defer baseClient.CloseIdleConnections()
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		response, err := baseClient.Transport.RoundTrip(req)
		if err == nil && req.URL.Path == "/v1/onboarding/api-key" {
			response.Body = cancelSignupOnEOF{ReadCloser: response.Body, cancel: cancel}
		}
		return response, err
	})}
	app.StoreCredential = func(string, string) error { t.Error("canceled signup stored a credential"); return nil }
	if exit := app.Run(signupRegressionArgs()); exit != ExitNetwork || revocations.Load() != 1 || !strings.Contains(out.String(), "REQUEST_CANCELED") {
		t.Fatalf("exit=%d revocations=%d stdout=%s stderr=%s", exit, revocations.Load(), out, stderr)
	}
	if strings.Contains(out.String(), "flint_test_synthetic") {
		t.Fatal("cleanup exposed the issued secret")
	}
}

func signupRegressionArgs() []string {
	return []string{"signup", "--email", "review@example.com", "--first-name", "Test", "--last-name", "User", "--verification-code", "123456", "--output", "json"}
}
