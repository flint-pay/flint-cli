package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifiedLogoutRejectsEnvironmentCredentials(t *testing.T) {
	for _, variable := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_CHECKOUT_SESSION_SECRET"} {
		t.Run(variable, func(t *testing.T) {
			a, out, _ := testApp(t, "")
			for _, name := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_CHECKOUT_SESSION_SECRET"} {
				t.Setenv(name, "")
			}
			t.Setenv(variable, "synthetic-credential")
			a.DeleteCredential = func(string) error {
				t.Error("active environment credential must prevent keychain deletion")
				return nil
			}
			if exit := a.Run([]string{"auth", "logout", "--confirm", "--output", "json"}); exit != ExitAuth {
				t.Fatalf("exit=%d output=%s", exit, out)
			}
			if !strings.Contains(out.String(), "ENVIRONMENT_CREDENTIAL_ACTIVE") || !strings.Contains(out.String(), variable) || strings.Contains(out.String(), "synthetic-credential") {
				t.Fatalf("logout must identify the variable without printing the credential: %s", out)
			}
		})
	}
}

func TestVerifiedMCPShutdownWithoutEOF(t *testing.T) {
	a, _, _ := testApp(t, "")
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Context = ctx
	a.Stdin = reader
	done := make(chan int, 1)
	go func() { done <- a.serveMCP(defaultOptions()) }()
	// A write rendezvous ensures the server has started reading before cancel.
	if _, err := io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case exit := <-done:
		if exit != ExitOK {
			t.Fatalf("exit=%d", exit)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MCP did not stop while its input pipe remained open")
	}
}

func TestVerifiedLocalRequestsHonorCancellation(t *testing.T) {
	for _, command := range [][]string{{"doctor"}, {"init"}, {"config", "validate"}, {"auth", "import", "--stdin"}} {
		t.Run(strings.Join(command, "-"), func(t *testing.T) {
			started, closed := make(chan struct{}), make(chan struct{})
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/v1/developer/auth-context" {
					t.Error("request continued after cancellation")
					return
				}
				close(started)
				<-r.Context().Done()
				close(closed)
			}))
			defer server.Close()
			a, _, _ := testApp(t, server.URL)
			a.Stdin = strings.NewReader("flint_test_synthetic")
			a.StoreCredential = func(string, string) error { t.Error("canceled import must not store credentials"); return nil }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.Context = ctx
			done := make(chan int, 1)
			go func() { done <- a.Run(append(command, "--timeout", "5s", "--output", "json")) }()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("auth request did not start")
			}
			cancel()
			select {
			case exit := <-done:
				if exit != ExitNetwork || requests.Load() != 1 {
					t.Fatalf("exit=%d requests=%d", exit, requests.Load())
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not stop the active command")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("HTTP request remained open")
			}
		})
	}
}

func TestVerifiedListenErrorBodyTimeout(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			a, _, _ := testApp(t, server.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			started := time.Now()
			_, retryable, e := a.openListenStream(ctx, server.URL, "flint_test_synthetic", "/v1/webhook-events/stream", "", 200*time.Millisecond, false)
			if e == nil || e.Code != "REQUEST_TIMEOUT" || retryable != (status == http.StatusServiceUnavailable) {
				t.Fatalf("retryable=%v error=%v", retryable, e)
			}
			if time.Since(started) > 2*time.Second || ctx.Err() != nil {
				t.Fatal("error body was only bounded by the parent context")
			}
		})
	}
}

func TestVerifiedMCPPreservesFlagLikeValues(t *testing.T) {
	a, _, _ := testApp(t, "")
	for _, name := range []string{"--help", "--live", "--", "--name=value"} {
		response := a.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{
			"name": "organizations.create", "arguments": map[string]any{"name": name, "_flint": map[string]any{"dry_run": "client"}},
		}}, defaultOptions())
		if got, _ := lookupPath(response, "result.structuredContent.data.body.name"); got != name {
			t.Fatalf("name=%q response=%v", name, response)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "--help" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		io.WriteString(w, "[]")
	}))
	defer server.Close()
	t.Setenv("FLINT_HELP_URL", server.URL)
	response := a.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{
		"name": "help.search", "arguments": map[string]any{"query": "--help"},
	}}, defaultOptions())
	if got, _ := lookupPath(response, "result.structuredContent.data.query"); got != "--help" {
		t.Fatalf("response=%v", response)
	}
}

func TestVerifiedMCPCommandHelpIsStructured(t *testing.T) {
	a, out, _ := testApp(t, "")
	response := a.handleMCP(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: map[string]any{
		"name": "help", "arguments": map[string]any{"topic": "customers.create"},
	}}, defaultOptions())
	if got, _ := lookupPath(response, "result.structuredContent.data.canonical_name"); got != "customers.create" {
		t.Fatalf("response=%v", response)
	}
	if exit := a.Run([]string{"help", "customers.create"}); exit != ExitOK || json.Valid(out.Bytes()) || !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("terminal help changed: exit=%d output=%s", exit, out)
	}
}

func TestVerifiedCheckoutTransformsRemainVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			io.WriteString(w, authContextJSON("sandbox"))
			return
		}
		io.WriteString(w, `{"data":{"checkout_session":{"checkout_session_id":"cs_review"},"hosted_checkout":{"url":"https://checkout.example/session"}}}`)
	}))
	defer server.Close()
	for _, flags := range [][]string{{"--jq", ".data.hosted_checkout.url"}, {"--select", "hosted_checkout.url"}, {"--jq", ".data.hosted_checkout.url", "--output", "json"}} {
		a, out, _ := testApp(t, server.URL)
		argv := append([]string{"checkout", "create", "--quick-pay-name", "T-shirt", "--amount", "2500", "--currency", "USD"}, flags...)
		if exit := a.Run(argv); exit != ExitOK || !strings.Contains(out.String(), "https://checkout.example/session") {
			t.Fatalf("flags=%v exit=%d output=%q", flags, exit, out.String())
		}
	}
}
