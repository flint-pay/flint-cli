package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	webhooksigning "github.com/flint-pay/flint-cli/internal/webhooksigning"
)

func TestListenProjectsPublishedMerchantEnvelope(t *testing.T) {
	for _, eventType := range []string{"customer.created", "future.new_type"} {
		frame := map[string]any{"api_version": "2026-09-07", "created_at": "2026-10-31T00:00:00Z", "data": map[string]any{"object": map[string]any{"customer_id": "cus_fixture"}}, "event_type": eventType, "event_source": "merchant", "merchant_id": "mer_fixture", "mode": "test", "payload_version": 2, "request": nil, "sandbox_id": "test_fixture", "test": false, "webhook_event_id": "whev_fixture"}
		raw, _ := json.Marshal(frame)
		frame["api_request_log_id"] = "req_fixture"
		frame["resource_id"] = "cus_fixture"
		frame["resource_type"] = "customer"
		streamed, _ := json.Marshal(frame)
		got, err := projectListenEnvelope(streamed, eventType)
		if err != nil || !bytes.Equal(raw, got) {
			t.Fatalf("%s: projected=%s want=%s err=%v", eventType, got, raw, err)
		}
	}
}

func TestListenForwardsRetriesAndHeaders(t *testing.T) {
	secret := "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	for _, scenario := range []struct {
		name               string
		status             int
		transport, timeout bool
		want               int
	}{
		{name: "server failure", status: 503, want: 3},
		{name: "rate limit", status: 429, want: 3},
		{name: "transport", transport: true, want: 3},
		{name: "timeout", timeout: true, want: 3},
		{name: "bad request", status: 400, want: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			app, _, _ := testApp(t, "http://127.0.0.1:9")
			count := 0
			payload := []byte(`{"webhook_event_id":"whev_fixture","data":{"object":{}}}`)
			app.ForwardHTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				count++
				body, _ := io.ReadAll(r.Body)
				if !bytes.Equal(body, payload) || r.Header.Get("Flint-Webhook-Endpoint-Id") != "whep_custom" {
					t.Errorf("body/header changed: %s %v", body, r.Header)
				}
				if err := webhooksigning.VerifyStandardWebhook(secret, r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), body, time.Now(), time.Minute); err != nil {
					t.Error(err)
				}
				if count < scenario.want && scenario.transport {
					return nil, errors.New("connection reset")
				}
				if count < scenario.want && scenario.timeout {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				status := scenario.status
				if count == scenario.want && scenario.want > 1 {
					status = 204
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})}
			started := time.Now()
			result := app.forwardListenPayload(context.Background(), 20*time.Millisecond, "http://127.0.0.1:1/hook", secret, listenWebhookEvent{WebhookEventID: "whev_fixture", Payload: payload, EndpointID: "whep_custom"})
			if count != scenario.want || result.attempts != scenario.want || result.delivered() != (scenario.want > 1) {
				t.Fatalf("count=%d result=%+v", count, result)
			}
			if scenario.want == 3 && time.Since(started) < 3*time.Second {
				t.Fatal("backoffs did not total 1s and 2s")
			}
		})
	}
}

func TestListenForwardCancellationStopsRetries(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	count := 0
	app.ForwardHTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) { count++; return nil, errors.New("connection reset") })}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result := app.forwardListenPayload(ctx, time.Second, "http://127.0.0.1:1/hook", "whsec_fixture", listenWebhookEvent{Payload: []byte(`{}`)})
	if count != 1 || !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("count=%d result=%+v", count, result)
	}
}

func TestListenSuppliedSecretsAreRedacted(t *testing.T) {
	for _, source := range []string{"flag", "environment", "saved", "generated", "temporary"} {
		for _, output := range []string{"json", "ndjson"} {
			app, out, _ := testApp(t, "http://127.0.0.1:9")
			secret := listenSecret{value: "secret-fixture-value", source: source}
			opts := Options{Output: output, Raw: map[string][]string{"event-type": {"customer.created,order.paid", "new.event"}}}
			if err := app.writeListenStarted(opts, "http://localhost/hook", secret); err != nil {
				t.Fatal(err)
			}
			listener := findListenRecord(t, out.String(), "listener")
			redact := source == "flag" || source == "environment"
			if strings.Contains(out.String(), secret.value) == redact || strings.Contains(listenBanner(opts, "http://localhost/hook", secret), secret.value) == redact {
				t.Fatalf("source=%s output=%s", source, out)
			}
			if listener["webhook_endpoint_id"] != "whep_cli_listen" || !reflect.DeepEqual(listener["event_type"], []any{"customer.created", "order.paid", "new.event"}) {
				t.Fatal(listener)
			}
		}
	}
}

func TestListenMultipleTypesAndServerRenameHint(t *testing.T) {
	gotFilter := ""
	server := listenAPI(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		gotFilter = r.URL.Query().Get("event_type")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":"INVALID_REQUEST","param":"event_type","message":"checkout_session.completed was renamed to checkout_session.closed"}}`)
	})
	app, out, _ := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--event-type", "checkout_session.completed,new.future_type", "--event-type", "customer.created,checkout_session.completed", "--for", "1s", "--output", "json"})
	if exit != ExitUsage && exit != ExitAPI {
		t.Fatalf("exit=%d out=%s", exit, out)
	}
	if gotFilter != "checkout_session.completed,new.future_type,customer.created" || !strings.Contains(out.String(), "was renamed to checkout_session.closed") {
		t.Fatalf("filter=%s out=%s", gotFilter, out)
	}
}
