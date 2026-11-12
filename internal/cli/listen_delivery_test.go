package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mockListenDelivery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EventID string `json:"webhook_event_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	parts := strings.Split(r.URL.Path, "/")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": listenSignedDelivery{
		EndpointID: parts[3], EventID: body.EventID, Body: `{}`, ExpiresAt: time.Now().Add(5 * time.Minute),
		Headers: map[string]string{"Content-Type": "application/json", "Authorization": "Bearer mock-token", "Flint-Webhook-Endpoint-Id": parts[3], "webhook-id": body.EventID, "User-Agent": listenUserAgent},
	}})
}

func mockListenSource(t *testing.T) listenDeliverySource {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(mockListenDelivery))
	t.Cleanup(server.Close)
	return listenDeliverySource{baseURL: server.URL, key: "flint_test_fixture", endpointID: "whep_fixture"}
}

func TestListenRequiresEndpointForForwarding(t *testing.T) {
	server := listenAPI(t, func(http.ResponseWriter, *http.Request, int) { t.Error("opened stream") })
	app, out, _ := testApp(t, server.URL)
	if exit := app.Run([]string{"listen", "--forward-to", "http://localhost/hook", "--for", "1s", "--output", "json"}); exit != ExitUsage {
		t.Fatalf("exit=%d", exit)
	}
	for _, expected := range []string{"--webhook-endpoint-id", "sandbox endpoint", "webhook-endpoints create"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("missing %q", expected)
		}
	}
}

func TestListenSignedDeliveryVerbatimAndNoFrameOutput(t *testing.T) {
	const body = "{ \"data\": {\"object\":{\"label\":\"é\"}}, \"test\":false }\n"
	headers := map[string]string{"Authorization": "Bearer sensitive-signed-token", "Content-Type": "application/json", "User-Agent": "Flint-Webhooks/1.0", "Flint-Webhook-Endpoint-Id": "whep_fixture", "webhook-id": "whev_fixture", "webhook-timestamp": "1800000000", "webhook-signature": "v1,optional-hmac", "Future-Header": "unchanged"}
	var signed, forwarded atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth-context":
			fmt.Fprint(w, authContextJSON("sandbox"))
		case "/v1/webhook-events/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, listenEventFrame("whev_fixture", "customer.created", `{"different":"stream body"}`))
		case "/v1/webhook-endpoints/whep_fixture/sign-delivery":
			signed.Add(1)
			request, _ := io.ReadAll(r.Body)
			if r.Method != "POST" || string(request) != `{"webhook_event_id":"whev_fixture"}` || r.Header.Get("Authorization") != "Bearer flint_test_test" {
				t.Error("incorrect signing request")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": listenSignedDelivery{EndpointID: "whep_fixture", EventID: "whev_fixture", Body: body, Headers: headers, ExpiresAt: time.Now().Add(5 * time.Minute)}})
		default:
			t.Error("unexpected API route")
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Error("body bytes changed")
		}
		for k, v := range headers {
			if r.Header.Get(k) != v {
				t.Errorf("header %s changed", k)
			}
		}
		// Reflect a frame; it must not reach output even on delivery failure.
		w.WriteHeader(400)
		fmt.Fprint(w, body+headers["Authorization"])
	}))
	defer local.Close()
	app, out, errOut := testApp(t, api.URL)
	if exit := app.Run([]string{"listen", "--forward-to", local.URL, "--webhook-endpoint-id", "whep_fixture", "--max-events", "1", "--debug", "--output", "ndjson"}); exit != ExitAPI {
		t.Fatalf("exit=%d", exit)
	}
	if signed.Load() != 1 || forwarded.Load() != 1 {
		t.Fatal("missing signing/forward request")
	}
	for _, secret := range []string{body, headers["Authorization"], "optional-hmac"} {
		if strings.Contains(out.String()+errOut.String(), secret) {
			t.Fatal("frame leaked to diagnostics")
		}
	}
}

func TestListenSigningLimits(t *testing.T) {
	for _, daily := range []bool{false, true} {
		t.Run(fmt.Sprint(daily), func(t *testing.T) {
			var calls int
			started := time.Now()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if daily {
					w.WriteHeader(409)
					fmt.Fprint(w, `{"error":{"code":"LIMIT_REACHED","details":[{"reason":"webhook_listener_signatures_daily","limit":10000,"resets_at":"2026-11-13T00:00:00Z"}]}}`)
					return
				}
				if calls == 1 {
					w.Header().Set("Retry-After", "2")
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"code":"RATE_LIMIT_EXCEEDED"}}`)
					return
				}
				if time.Since(started) < 2*time.Second {
					t.Error("Retry-After ignored")
				}
				mockListenDelivery(w, r)
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			_, e := app.signListenDelivery(context.Background(), time.Second, listenDeliverySource{baseURL: server.URL, endpointID: "whep_fixture"}, "whev_fixture")
			if daily {
				if calls != 1 || e == nil || e.Code != "LIMIT_REACHED" || !strings.Contains(e.Message, "2026-11-13T00:00:00Z") {
					t.Fatal("daily quota not surfaced")
				}
			} else if calls != 2 || e != nil {
				t.Fatalf("calls=%d err=%v", calls, e)
			}
		})
	}
}

func TestListenResignsExpiredFramesAndRetries(t *testing.T) {
	now := time.Now()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		expiry := now.Add(5 * time.Minute)
		if calls == 1 {
			expiry = now.Add(-time.Second)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": listenSignedDelivery{EndpointID: "whep_fixture", EventID: "whev_fixture", Body: `{}`, Headers: map[string]string{"Authorization": fmt.Sprintf("Bearer frame-%d", calls)}, ExpiresAt: expiry}})
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	forwarded := 0
	app.ForwardHTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		forwarded++
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer frame-%d", forwarded+1) {
			t.Error("stale frame forwarded")
		}
		status := 503
		if forwarded == 2 {
			status = 204
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	result := app.forwardListenPayload(context.Background(), time.Second, "http://localhost/hook", listenDeliverySource{baseURL: server.URL, endpointID: "whep_fixture"}, listenWebhookEvent{WebhookEventID: "whev_fixture"})
	if !result.delivered() || calls != 3 || forwarded != 2 {
		t.Fatal("expired frame was not refreshed")
	}
}

func TestListenDailyQuotaStopsBeforeAdvancingCursor(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth-context":
			fmt.Fprint(w, authContextJSON("sandbox"))
		case "/v1/webhook-events/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"whev_before\"}\n\n")
			fmt.Fprint(w, listenEventFrame("whev_first", "customer.created", `{}`), listenEventFrame("whev_second", "customer.created", `{}`))
		default:
			calls++
			w.WriteHeader(409)
			fmt.Fprint(w, `{"error":{"code":"LIMIT_REACHED","details":[{"reason":"webhook_listener_signatures_daily","limit":10000,"resets_at":"2026-11-13T00:00:00Z"}]}}`)
		}
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	app.ForwardHTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Error("forwarded without frame"); return nil, nil })}
	if exit := app.Run([]string{"listen", "--forward-to", "http://localhost/hook", "--webhook-endpoint-id", "whep_fixture", "--max-events", "2", "--output", "ndjson"}); exit != ExitAPI {
		t.Fatalf("exit=%d", exit)
	}
	checkpoint := findListenRecord(t, out.String(), "checkpoint")
	if calls != 1 || checkpoint["cursor"] != "whev_before" || checkpoint["processed"] != float64(0) || !strings.Contains(out.String(), "2026-11-13T00:00:00Z") {
		t.Fatalf("daily quota did not stop safely: %s", out.String())
	}
}

func TestListenMalformedReceiverCannotLeakFrame(t *testing.T) {
	const token = "Bearer sensitive-reflected-frame"
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		// An invalid status line makes net/http include remote bytes in its error.
		_, _ = fmt.Fprint(conn, r.Header.Get("Authorization")+"\r\n\r\n")
	}))
	defer local.Close()
	app, _, _ := testApp(t, "")
	result := app.forwardListenAttempt(context.Background(), time.Second, local.URL, listenSignedDelivery{Body: `{}`, Headers: map[string]string{"Authorization": token}})
	if result.err == nil || !strings.Contains(result.err.Error(), "sensitive-reflected-frame") {
		t.Fatal("fixture did not reflect the frame in a transport error")
	}
	if strings.Contains(listenForwardErrorReason(result.err, time.Second), "sensitive-reflected-frame") {
		t.Fatal("transport diagnostic leaked a signed frame")
	}
}
