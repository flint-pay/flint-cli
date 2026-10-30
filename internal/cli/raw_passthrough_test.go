package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUnknownRawCanonicalPaths(t *testing.T) {
	for _, path := range []string{"/v1/future-resources?name=a%20b", "/v1/future-resources/future_123"} {
		if e := validateRawPublicRoute("GET", path); e != nil {
			t.Errorf("%s: %v", path, e)
		}
	}
	for _, path := range []string{"https://example.com/v1/future", "//example.com/v1/future", "/v1/future#", "/v1/future#fragment", "/v1/../private", "/v1/%2e%2e/private", "/v1/future//x", "/v1/future/", "/v1/future%2fx", "/v1/%252e%252e/private", "/v1/future\\x", "/v1/future%0ax", "/v1/%66uture", "/v1/future%ZZ"} {
		if e := validateRawPublicRoute("GET", path); e == nil {
			t.Errorf("accepted %s", path)
		}
	}
	if e := validateRawPublicRoute("TRACE", "/v1/future"); e == nil {
		t.Error("accepted TRACE")
	}
}

func TestUnknownRawWriteForwardsKeyOnce(t *testing.T) {
	for _, key := range []string{"", "caller-saved-key"} {
		t.Run(key, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/auth-context" {
					fmt.Fprint(w, authContextJSON("sandbox"))
					return
				}
				calls++
				if r.Method != "POST" || r.URL.Path != "/v1/future-resources" || r.URL.Query().Get("mode") != "test" {
					t.Errorf("request %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Idempotency-Key") != key {
					t.Errorf("key = %q", r.Header.Get("Idempotency-Key"))
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"future_field":true}` {
					t.Errorf("body %s", body)
				}
				w.WriteHeader(503)
				fmt.Fprint(w, `{"error":{"type":"api_error","code":"SERVICE_UNAVAILABLE","message":"fixture","remediation":{"retry":"same_request"}}}`)
			}))
			defer server.Close()
			app, out, stderr := testApp(t, server.URL)
			app.Stdin = strings.NewReader(`{"future_field":true}`)
			argv := []string{"api", "post", "/v1/future-resources?mode=test", "--input", "-", "--confirm", "--output", "json"}
			if key != "" {
				argv = append(argv, "--idempotency-key", key)
			}
			if exit := app.Run(argv); exit == ExitOK {
				t.Fatalf("exit=%d out=%s", exit, out)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
			if !strings.Contains(stderr.String(), "CLI test schema snapshot "+embeddedSnapshotHash()) {
				t.Errorf("warning %s", stderr)
			}
			if strings.Contains(out.String(), "retry with the same") {
				t.Errorf("unsafe recovery advice %s", out)
			}
		})
	}
}

func TestUnknownRawWriteRequiresConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth-context" {
			t.Error("unconfirmed write sent")
		}
		fmt.Fprint(w, authContextJSON("live"))
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "flint_live_test")
	if exit := app.Run([]string{"api", "post", "/v1/future-resources", "--live", "--output", "json"}); exit != ExitConfirmation {
		t.Fatalf("exit=%d out=%s", exit, out)
	}
}

func TestMCPUnknownRawRouteRemainsUnavailable(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	cmd, _ := app.Registry.ByName("api")
	result := app.runMCPCommand(context.Background(), cmd, map[string]any{"method": "GET", "path": "/v1/future-resources"}, Options{}, mcpPolicy{}, nil)
	if result.exit != ExitUsage {
		t.Fatalf("result=%+v", result)
	}
}

func TestUnknownRawTransportFailureIsNotReplayed(t *testing.T) {
	app, out, _ := testApp(t, "http://127.0.0.1:1")
	calls := 0
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/auth-context" {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(authContextJSON("sandbox")))}, nil
		}
		calls++
		return nil, io.ErrUnexpectedEOF
	})}
	exit := app.Run([]string{"api", "post", "/v1/future-resources", "--idempotency-key", "caller-saved-key", "--confirm", "--output", "json"})
	if exit != ExitNetwork || calls != 1 || !strings.Contains(out.String(), `"idempotency_key":"caller-saved-key"`) || strings.Contains(out.String(), "is safe") {
		t.Fatalf("exit=%d calls=%d out=%s", exit, calls, out)
	}
}

func TestUnknownRawWriteCannotReplayInHTTPTransport(t *testing.T) {
	for _, body := range []string{"", `{"action":true}`} {
		t.Run(body, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					io.WriteString(w, `{"data":{}}`)
					return
				}
				io.Copy(io.Discard, r.Body)
				if r.Header.Get("Idempotency-Key") != "caller-key" {
					t.Error("caller key was not forwarded")
				}
				if writes.Add(1) == 1 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				io.WriteString(w, `{"data":{"ok":true}}`)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			app.HTTPClient = &http.Client{Transport: transport}
			if _, e := app.doRequest(context.Background(), server.URL, "", http.MethodGet, "/v1/prime", nil, "", false); e != nil {
				t.Fatal(e)
			}
			ctx := context.WithValue(context.Background(), responseContractKey{}, &Command{CanonicalName: "api", Method: http.MethodPost, APIPath: "/v1/future-resources", Mutation: true})
			_, e := app.doRequest(ctx, server.URL, "", http.MethodPost, "/v1/future-resources", []byte(body), "caller-key", false)
			if e == nil || writes.Load() != 1 {
				t.Fatalf("writes=%d error=%v", writes.Load(), e)
			}
		})
	}
}
