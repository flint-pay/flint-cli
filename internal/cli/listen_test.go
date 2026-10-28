package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	webhooksigning "github.com/flint-pay/flint-cli/internal/webhooksigning"
)

type listenRoundTripFunc func(*http.Request) (*http.Response, error)

func (f listenRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type listenTerminalErrorReader struct {
	reader *strings.Reader
	err    error
}

func (r *listenTerminalErrorReader) Read(buffer []byte) (int, error) {
	if r.reader.Len() > 0 {
		return r.reader.Read(buffer)
	}
	if r.err != nil {
		err := r.err
		r.err = nil
		return 0, err
	}
	return 0, io.EOF
}

func TestValidateLocalForwardURL(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "localhost", value: "http://localhost:8080/webhooks", valid: true},
		{name: "IPv4 loopback", value: "http://127.0.0.1:8080/webhooks?source=flint", valid: true},
		{name: "IPv6 loopback", value: "https://[::1]:8443/webhooks", valid: true},
		{name: "Docker Desktop host", value: "http://host.docker.internal:8080/webhooks", valid: true},
		{name: "Docker host lookalike", value: "http://host.docker.internal.example.com/webhooks", valid: false},
		{name: "remote HTTPS", value: "https://example.com/webhooks", valid: false},
		{name: "remote HTTP", value: "http://192.0.2.1/webhooks", valid: false},
		{name: "localhost lookalike", value: "http://localhost.example.com/webhooks", valid: false},
		{name: "credentials", value: "http://user:secret@localhost:8080/webhooks", valid: false},
		{name: "fragment", value: "http://localhost:8080/webhooks#secret", valid: false},
		{name: "file", value: "file:///tmp/webhook", valid: false},
		{name: "relative", value: "/webhooks", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateLocalForwardURL(test.value)
			if test.valid && err != nil {
				t.Fatalf("validateLocalForwardURL(%q): %v", test.value, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("validateLocalForwardURL(%q) unexpectedly succeeded", test.value)
			}
		})
	}
}

func TestReadListenSSEPreservesDataBytesAndJoinsMultilineData(t *testing.T) {
	input := "id: whev_123\r\nevent: webhook.event\r\ndata: {\"first\":1,\r\ndata: \"second\":2}\r\n\r\n"
	var frame listenSSEFrame
	err := readListenSSE(strings.NewReader(input), func(item listenSSEFrame) error {
		frame = item
		return nil
	})
	if err != io.EOF {
		t.Fatalf("readListenSSE error = %v", err)
	}
	if frame.ID != "whev_123" || frame.Event != "webhook.event" || string(frame.Data) != "{\"first\":1,\n\"second\":2}" {
		t.Fatalf("frame = %#v", frame)
	}
}

func TestReadListenSSEDispatchesConsecutiveFrames(t *testing.T) {
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	input := "event: ready\ndata: {\"cursor\":\"\"}\n\n" +
		fmt.Sprintf("id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"payment_intent.succeeded\",\"payload\":{\"status\":\"succeeded\"}}\n\n", eventID, eventID)
	var frames []listenSSEFrame
	err := readListenSSE(strings.NewReader(input), func(frame listenSSEFrame) error {
		frames = append(frames, frame)
		return nil
	})
	if err != io.EOF {
		t.Fatalf("readListenSSE error = %v", err)
	}
	if len(frames) != 2 || frames[0].Event != "ready" || frames[1].Event != "webhook.event" || frames[1].ID != eventID {
		t.Fatalf("frames = %#v", frames)
	}
}

func TestConsumeListenStreamCountsEventAfterReady(t *testing.T) {
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()
	input := "event: ready\ndata: {\"cursor\":\"\"}\n\n" +
		fmt.Sprintf("id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"payment_intent.succeeded\",\"payload\":{\"status\":\"succeeded\"}}\n\n", eventID, eventID)
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Now: time.Now}
	state := &listenStreamState{}
	if err := app.consumeListenStream(context.Background(), strings.NewReader(input), Options{Output: "ndjson", Timeout: time.Second}, local.URL, "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", state); err != nil {
		t.Fatalf("consumeListenStream error = %#v", err)
	}
	if state.processed != 1 || state.events != 1 || state.cursor != eventID {
		t.Fatalf("state = %#v", state)
	}
}

func TestConsumeListenStreamAdvancesPastWithheldEvent(t *testing.T) {
	const withheldID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	const visibleID = "whev_02ABCDEFGHIJKLMNOPQRSTUVWX"
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()
	input := fmt.Sprintf("id: %s\nevent: withheld\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"order.paid\",\"reason\":\"missing_resource_scope\",\"resource_type\":\"order\",\"required_scopes\":[\"commerce.orders.read\"]}\n\n", withheldID, withheldID) +
		fmt.Sprintf("id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"payment_intent.succeeded\",\"payload\":{\"status\":\"succeeded\"}}\n\n", visibleID, visibleID)
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: &bytes.Buffer{}, Now: time.Now}
	state := &listenStreamState{}
	if err := app.consumeListenStream(context.Background(), strings.NewReader(input), Options{Output: "ndjson", Timeout: time.Second}, local.URL, "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", state); err != nil {
		t.Fatalf("consumeListenStream error = %#v", err)
	}
	if state.processed != 2 || state.withheld != 1 || state.events != 1 || state.cursor != visibleID {
		t.Fatalf("state = %#v", state)
	}
	if !strings.Contains(stdout.String(), `"type":"withheld"`) || !strings.Contains(stdout.String(), `"required_scopes":["commerce.orders.read"]`) {
		t.Fatalf("withheld output = %s", stdout)
	}
}

func TestListenMaxEventsCountsWithheldRecords(t *testing.T) {
	const withheldID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	streamClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		defer close(streamClosed)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "id: %s\nevent: withheld\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"order.paid\",\"reason\":\"missing_resource_scope\",\"resource_type\":\"order\",\"required_scopes\":[\"commerce.orders.read\"]}\n\n", withheldID, withheldID)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--max-events", "1", "--output", "json"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	select {
	case <-streamClosed:
	case <-time.After(time.Second):
		t.Fatal("listener did not close after the bounded withheld record")
	}
	if !strings.Contains(stdout.String(), `"processed":1`) || !strings.Contains(stdout.String(), `"withheld":1`) {
		t.Fatalf("checkpoint output = %s", stdout)
	}
}

func TestListenReconnectsWithLastEventID(t *testing.T) {
	const firstID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	const secondID = "whev_02ABCDEFGHIJKLMNOPQRSTUVWX"
	var mu sync.Mutex
	var bodies []string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()

	connections := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		connections++
		w.Header().Set("Content-Type", "text/event-stream")
		switch connections {
		case 1:
			if got := r.Header.Get("Last-Event-ID"); got != "" {
				t.Errorf("first Last-Event-ID = %q", got)
			}
			fmt.Fprintf(w, "id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"payment_intent.created\",\"payload\":{\"sequence\":1}}\n\n", firstID, firstID)
		case 2:
			if got := r.Header.Get("Last-Event-ID"); got != firstID {
				t.Errorf("second Last-Event-ID = %q, want %q", got, firstID)
			}
			fmt.Fprintf(w, "id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"payment_intent.succeeded\",\"payload\":{\"sequence\":2}}\n\n", secondID, secondID)
		default:
			t.Errorf("unexpected stream connection %d", connections)
		}
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "2", "--output", "ndjson"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	mu.Lock()
	gotBodies := append([]string(nil), bodies...)
	mu.Unlock()
	if len(gotBodies) != 2 || gotBodies[0] != `{"sequence":1}` || gotBodies[1] != `{"sequence":2}` {
		t.Fatalf("forwarded bodies = %#v", gotBodies)
	}
	if !strings.Contains(stdout.String(), `"cursor":"`+secondID+`"`) {
		t.Fatalf("checkpoint missing second cursor: %s", stdout)
	}
}

func TestListenMaxEventsStopsAnOpenStream(t *testing.T) {
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()
	streamClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		defer close(streamClosed)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"payment_intent.succeeded\",\"payload\":{}}\n\n", eventID, eventID)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "1", "--output", "ndjson"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	select {
	case <-streamClosed:
	case <-time.After(time.Second):
		t.Fatal("listener did not close the open SSE request after reaching --max-events")
	}
	if !strings.Contains(stdout.String(), `"type":"checkpoint"`) || !strings.Contains(stdout.String(), `"events":1`) {
		t.Fatalf("checkpoint output = %s", stdout)
	}
}

func TestListenReconnectsAfterTransientStreamReadError(t *testing.T) {
	const firstID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	const secondID = "whev_02ABCDEFGHIJKLMNOPQRSTUVWX"
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()

	var mu sync.Mutex
	streamCalls := 0
	lastEventIDs := []string{}
	transport := listenRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/developer/auth-context" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(authContextJSON("sandbox"))),
				Request:    req,
			}, nil
		}
		mu.Lock()
		defer mu.Unlock()
		streamCalls++
		lastEventIDs = append(lastEventIDs, req.Header.Get("Last-Event-ID"))
		var body io.ReadCloser
		if streamCalls == 1 {
			frame := fmt.Sprintf("id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"payment_intent.created\",\"payload\":{\"sequence\":1}}\n\n", firstID, firstID)
			body = io.NopCloser(&listenTerminalErrorReader{reader: strings.NewReader(frame), err: errors.New("connection reset")})
		} else {
			frame := fmt.Sprintf("id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"payment_intent.succeeded\",\"payload\":{\"sequence\":2}}\n\n", secondID, secondID)
			body = io.NopCloser(strings.NewReader(frame))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    req,
		}, nil
	})

	app, stdout, stderr := testApp(t, "https://api.example.test")
	app.HTTPClient = &http.Client{Transport: transport}
	exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "2", "--output", "ndjson"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if streamCalls != 2 {
		t.Fatalf("stream calls = %d, want 2", streamCalls)
	}
	if len(lastEventIDs) != 2 || lastEventIDs[0] != "" || lastEventIDs[1] != firstID {
		t.Fatalf("Last-Event-ID values = %#v", lastEventIDs)
	}
	if !strings.Contains(stdout.String(), `"type":"checkpoint"`) || !strings.Contains(stdout.String(), `"cursor":"`+secondID+`"`) {
		t.Fatalf("checkpoint output = %s", stdout)
	}
}

func TestListenTimeoutBoundsStreamResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	app, _, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--timeout", "25ms"})
	if exit != ExitNetwork || !strings.Contains(stderr.String(), "Timed out waiting") {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
}

func TestOpenListenStreamTimeoutDoesNotWaitForTransportCancellation(t *testing.T) {
	release := make(chan struct{})
	transport := listenRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})
	app := &App{HTTPClient: &http.Client{Transport: transport}, Stderr: io.Discard, Info: BuildInfo{Version: "test"}}
	started := time.Now()
	_, retryable, streamErr := app.openListenStream(context.Background(), "https://api.example.test", "flint_test_test", "/v1/webhook-events/stream", "", 25*time.Millisecond, false)
	close(release)
	if streamErr == nil || streamErr.Code != "REQUEST_TIMEOUT" || retryable {
		t.Fatalf("open error = %#v, retryable = %t", streamErr, retryable)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("stream open timeout took %s", elapsed)
	}
}

func TestListenReconnectsWhenStreamGoesIdle(t *testing.T) {
	var connections atomic.Int32
	server := listenAPI(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		connections.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--timeout", "50ms", "--for", "600ms", "--output", "ndjson"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if connections.Load() < 2 {
		t.Fatalf("stream connections = %d, want a reconnect after the idle timeout", connections.Load())
	}
	reconnect := findListenRecord(t, stdout.String(), "reconnect")
	if reconnect["reason"] != "STREAM_IDLE_TIMEOUT" || reconnect["attempt"] != float64(1) || !strings.Contains(fmt.Sprint(reconnect["message"]), "sent no data for 50ms") {
		t.Fatalf("reconnect record = %#v", reconnect)
	}
}

func TestListenReportsCursorGapAndUsesResumeCursor(t *testing.T) {
	const resumeID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: gap\ndata: {\"reason\":\"cursor_unavailable\",\"requested_cursor\":\"whev_old\",\"resume_cursor\":\"%s\"}\n\n", resumeID)
		fmt.Fprintf(w, "event: ready\ndata: {\"cursor\":\"%s\"}\n\n", resumeID)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--for", "25ms", "--output", "ndjson"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout.String(), `"type":"gap"`) || !strings.Contains(stdout.String(), `"cursor":"`+resumeID+`"`) {
		t.Fatalf("gap/checkpoint output = %s", stdout)
	}
}

func TestListenWithholdsInvisiblePayloadAndContinues(t *testing.T) {
	const hiddenID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	const visibleID = "whev_02ABCDEFGHIJKLMNOPQRSTUVWX"
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()
	server := listenAPI(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"order.paid\",\"payload\":null}\n\n", hiddenID, hiddenID)
		fmt.Fprint(w, listenEventFrame(visibleID, "payment_intent.succeeded", `{"status":"succeeded"}`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "2", "--output", "ndjson"})
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	withheld := findListenRecord(t, stdout.String(), "withheld")
	if withheld["webhook_event_id"] != hiddenID || withheld["reason"] != "payload_not_visible" || !strings.Contains(fmt.Sprint(withheld["message"]), "read access") {
		t.Fatalf("withheld record = %#v", withheld)
	}
	checkpoint := findListenRecord(t, stdout.String(), "checkpoint")
	if checkpoint["withheld"] != float64(1) || checkpoint["forwarded"] != float64(1) || checkpoint["cursor"] != visibleID {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
}

func TestLocalForwardingDoesNotFollowRedirects(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	app := &App{Now: time.Now}
	result := app.forwardListenPayload(context.Background(), time.Second, redirect.URL, "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", listenWebhookEvent{
		WebhookEventID: "whev_01ABCDEFGHIJKLMNOPQRSTUVWX",
		EventType:      "payment_intent.succeeded",
		Payload:        []byte(`{"status":"succeeded"}`),
	})
	if result.err != nil || result.statusCode != http.StatusTemporaryRedirect || reached {
		t.Fatalf("result=%+v reached=%t", result, reached)
	}
}

func TestListenRejectsMismatchedSSEEventID(t *testing.T) {
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Now: time.Now}
	state := &listenStreamState{}
	body := strings.NewReader("id: whev_one\nevent: webhook.event\ndata: {\"webhook_event_id\":\"whev_two\",\"event_type\":\"order.paid\",\"payload\":{}}\n\n")
	err := app.consumeListenStream(context.Background(), body, Options{}, "http://127.0.0.1:1/webhooks", "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", state)
	if err == nil || err.Code != "INVALID_STREAM_EVENT_ID" || err.ExitCode != ExitAPI {
		t.Fatalf("error = %#v", err)
	}
}

func TestListenErrorCatalogMatchesInvalidResponseExitCode(t *testing.T) {
	t.Parallel()

	catalog := errorCatalog()
	entries, ok := catalog["listen_codes"].([]map[string]any)
	if !ok {
		t.Fatalf("listen error catalog type = %T", catalog["listen_codes"])
	}
	expected := map[string]bool{
		"INVALID_STREAM_CONTENT_TYPE": false,
		"INVALID_STREAM_EVENT":        false,
		"INVALID_STREAM_EVENT_ID":     false,
		"INVALID_STREAM_PAYLOAD":      false,
	}
	for _, entry := range entries {
		code, _ := entry["code"].(string)
		if _, exists := expected[code]; !exists {
			continue
		}
		if entry["exit_code"] != ExitAPI {
			t.Fatalf("%s exit code = %#v, want %d", code, entry["exit_code"], ExitAPI)
		}
		expected[code] = true
	}
	for code, found := range expected {
		if !found {
			t.Fatalf("listen error catalog is missing %s", code)
		}
	}
}

// listenAPI serves the auth context and hands each stream request, with its
// 1-based connection number, to stream.
func listenAPI(t *testing.T, stream func(w http.ResponseWriter, r *http.Request, connection int)) *httptest.Server {
	t.Helper()
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		stream(w, r, int(connections.Add(1)))
	}))
	t.Cleanup(server.Close)
	return server
}

func listenEventFrame(id, eventType, payload string) string {
	return fmt.Sprintf("id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"%s\",\"payload\":%s}\n\n", id, id, eventType, payload)
}

func listenRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	var records []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("invalid listener JSON line %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	return records
}

func findListenRecord(t *testing.T, output, recordType string) map[string]any {
	t.Helper()
	for _, record := range listenRecords(t, output) {
		if record["type"] == recordType {
			return record
		}
	}
	t.Fatalf("no %s record in output: %s", recordType, output)
	return nil
}

func listenErrorRecord(t *testing.T, output string) map[string]any {
	t.Helper()
	for _, record := range listenRecords(t, output) {
		if failure, ok := record["error"].(map[string]any); ok {
			return failure
		}
	}
	t.Fatalf("no error record in output: %s", output)
	return nil
}

func TestValidateLocalForwardURLNamesAllowedHosts(t *testing.T) {
	_, err := validateLocalForwardURL("https://example.com/webhooks")
	if err == nil || !strings.Contains(err.Error(), "localhost") || !strings.Contains(err.Error(), "loopback") || !strings.Contains(err.Error(), "host.docker.internal") {
		t.Fatalf("error = %v", err)
	}
}

func TestDockerHostForwardingStaysOnPrivateAddresses(t *testing.T) {
	var dialed []string
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return nil, errors.New("refused")
	}
	if _, err := dialDockerHostAddresses(context.Background(), "tcp", "8080", []netip.Addr{netip.MustParseAddr("192.168.65.254"), netip.MustParseAddr("203.0.113.10")}, dial); err == nil || !strings.Contains(err.Error(), "203.0.113.10") || len(dialed) != 0 {
		t.Fatalf("public address error = %v, dialed = %#v", err, dialed)
	}
	if _, err := dialDockerHostAddresses(context.Background(), "tcp", "8080", []netip.Addr{netip.MustParseAddr("192.168.65.254"), netip.MustParseAddr("::ffff:172.17.0.1")}, dial); err == nil || len(dialed) != 2 {
		t.Fatalf("private addresses error = %v, dialed = %#v", err, dialed)
	}
	if _, err := dialLoopbackAddresses(context.Background(), "tcp", "8080", []netip.Addr{netip.MustParseAddr("192.168.65.254")}, dial); err == nil || len(dialed) != 2 {
		t.Fatalf("a private address passed the loopback-only check: %v", err)
	}
}

func TestListenSavesSigningSecretOutsideConfig(t *testing.T) {
	t.Setenv(listenSecretEnv, "")
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	server := listenAPI(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"\"}\n\n")
		fmt.Fprint(w, listenEventFrame(eventID, "payment_intent.succeeded", `{"status":"succeeded"}`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	app, stdout, stderr := testApp(t, server.URL)
	printSecret := func(args ...string) map[string]any {
		t.Helper()
		stdout.Reset()
		if exit := app.Run(append([]string{"listen", "--print-secret", "--output", "json"}, args...)); exit != ExitOK || stderr.Len() != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatalf("print-secret output %q: %v", stdout, err)
		}
		return envelope.Data
	}
	first := printSecret()
	secret, _ := first["signing_secret"].(string)
	if _, err := webhooksigning.StandardWebhookSigningKey(secret); err != nil || first["source"] != "generated" {
		t.Fatalf("first secret = %#v: %v", first, err)
	}
	if second := printSecret(); second["signing_secret"] != secret || second["source"] != "saved" {
		t.Fatalf("second secret = %#v, want the saved %q", second, secret)
	}
	path := filepath.Join(app.ConfigDir, listenSecretFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %v", info.Mode().Perm())
	}
	if config, err := os.ReadFile(filepath.Join(app.ConfigDir, "config.json")); err == nil && strings.Contains(string(config), secret) {
		t.Fatal("the signing secret was written to config.json")
	}

	stdout.Reset()
	if exit := app.Run([]string{"listen", "--print-secret"}); exit != ExitOK || stdout.String() != secret+"\n" {
		t.Fatalf("human print-secret exit=%d output=%q", exit, stdout)
	}

	var signatureValid atomic.Bool
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Forwarding always uses the local HMAC secret, without a JWT or API credential.
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Error("forwarded request unexpectedly contains Authorization")
		}
		err := webhooksigning.VerifyStandardWebhook(secret, r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), body, time.Now(), time.Minute)
		signatureValid.Store(err == nil)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()
	stdout.Reset()
	if exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "1", "--output", "ndjson"}); exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	listener := findListenRecord(t, stdout.String(), "listener")
	if listener["signing_secret"] != secret || listener["signing_secret_source"] != "saved" || !signatureValid.Load() {
		t.Fatalf("listener = %#v, signature valid = %t", listener, signatureValid.Load())
	}
}

func TestListenWebhookSecretFlagAndEnvironment(t *testing.T) {
	flagSecret, err := webhooksigning.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	environmentSecret, err := webhooksigning.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(listenSecretEnv, environmentSecret)
	server := listenAPI(t, func(http.ResponseWriter, *http.Request, int) {
		t.Error("--print-secret opened the stream")
	})
	app, stdout, _ := testApp(t, server.URL)
	for _, test := range []struct {
		args   []string
		secret string
		source string
	}{
		{secret: environmentSecret, source: "environment"},
		{args: []string{"--webhook-secret", flagSecret}, secret: flagSecret, source: "flag"},
	} {
		stdout.Reset()
		if exit := app.Run(append([]string{"listen", "--print-secret", "--output", "json"}, test.args...)); exit != ExitOK {
			t.Fatalf("exit=%d output=%s", exit, stdout)
		}
		want := fmt.Sprintf(`{"data":{"signing_secret":%q,"source":%q}}`, test.secret, test.source)
		if strings.TrimSpace(stdout.String()) != want {
			t.Fatalf("output = %s, want %s", stdout, want)
		}
	}
	if _, err := os.Stat(filepath.Join(app.ConfigDir, listenSecretFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a supplied secret was saved: %v", err)
	}

	const invalid = "whsec_c2hvcnQ="
	stdout.Reset()
	if exit := app.Run([]string{"listen", "--print-secret", "--webhook-secret", invalid, "--output", "json"}); exit != ExitUsage || !strings.Contains(stdout.String(), `"code":"INVALID_WEBHOOK_SECRET"`) || strings.Contains(stdout.String(), invalid) {
		t.Fatalf("invalid flag secret exit=%d output=%s", exit, stdout)
	}
	t.Setenv(listenSecretEnv, "not-a-secret")
	stdout.Reset()
	if exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--for", "1s", "--output", "json"}); exit != ExitUsage || !strings.Contains(stdout.String(), listenSecretEnv+" is not a valid signing secret") || strings.Contains(stdout.String(), "not-a-secret") {
		t.Fatalf("invalid environment secret exit=%d output=%s", exit, stdout)
	}
}

func TestListenRejectsInvalidSavedSecret(t *testing.T) {
	t.Setenv(listenSecretEnv, "")
	server := listenAPI(t, func(http.ResponseWriter, *http.Request, int) {
		t.Error("an invalid saved secret opened the stream")
	})
	app, stdout, _ := testApp(t, server.URL)
	if err := os.WriteFile(filepath.Join(app.ConfigDir, listenSecretFile), []byte("hand-edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if exit := app.Run([]string{"listen", "--print-secret", "--output", "json"}); exit != ExitAuth || !strings.Contains(stdout.String(), `"code":"LISTEN_SECRET_INVALID"`) || !strings.Contains(stdout.String(), "Delete the file") {
		t.Fatalf("exit=%d output=%s", exit, stdout)
	}
}

func TestListenSecretFallsBackWhenItCannotBeSaved(t *testing.T) {
	t.Setenv(listenSecretEnv, "")
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{ConfigDir: blocker}
	secret, e := app.listenSigningSecret(Options{Raw: map[string][]string{}}, false)
	if e != nil || secret.source != "temporary" || !strings.Contains(secret.warning, "changes on the next run") {
		t.Fatalf("secret = %#v, error = %#v", secret, e)
	}
	if _, err := webhooksigning.StandardWebhookSigningKey(secret.value); err != nil {
		t.Fatalf("temporary secret is invalid: %v", err)
	}
	if _, e := app.listenSigningSecret(Options{Raw: map[string][]string{}}, true); e == nil || e.Code != "LISTEN_SECRET_SAVE_FAILED" {
		t.Fatalf("--print-secret with an unsavable secret = %#v", e)
	}
}

func TestListenReportsFailedForwardsAndExitsNonZero(t *testing.T) {
	const readyCursor = "whev_00ABCDEFGHIJKLMNOPQRSTUVWX"
	const firstID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	const secondID = "whev_02ABCDEFGHIJKLMNOPQRSTUVWX"
	var requests atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1)%2 == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "boom:\n\x1b[31m signature mismatch "+strings.Repeat("x", 600))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()
	server := listenAPI(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: ready\ndata: {\"cursor\":%q}\n\n", readyCursor)
		fmt.Fprint(w, listenEventFrame(firstID, "payment_intent.succeeded", `{"sequence":1}`))
		fmt.Fprint(w, listenEventFrame(secondID, "payment_intent.succeeded", `{"sequence":2}`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "2", "--output", "ndjson"})
	if exit != ExitAPI || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	failed := findListenRecord(t, stdout.String(), "forward")
	body, _ := failed["response_body"].(string)
	if failed["ok"] != false || failed["status_code"] != float64(500) || failed["response_body_truncated"] != true || !strings.HasPrefix(body, "boom: [31m signature mismatch x") || len(body) > listenResponseExcerptBytes || strings.ContainsAny(body, "\x1b\n") {
		t.Fatalf("failed forward record = %#v", failed)
	}
	checkpoint := findListenRecord(t, stdout.String(), "checkpoint")
	if checkpoint["events"] != float64(2) || checkpoint["forwarded"] != float64(1) || checkpoint["failed"] != float64(1) || checkpoint["replay_cursor"] != readyCursor || checkpoint["cursor"] != secondID {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
	failure := listenErrorRecord(t, stdout.String())
	if failure["code"] != "WEBHOOK_FORWARD_FAILED" || !strings.Contains(fmt.Sprint(failure["message"]), "--cursor "+readyCursor) {
		t.Fatalf("error = %#v", failure)
	}

	stdout.Reset()
	exit = app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "2"})
	human := stdout.String()
	if exit != ExitAPI || !strings.Contains(human, "-> HTTP 500") || !strings.Contains(human, "delivery failed: boom:") || !strings.Contains(human, "forwarded=1 failed=1") || !strings.Contains(human, "flint listen --forward-to "+local.URL+" --cursor "+readyCursor) {
		t.Fatalf("human exit=%d stdout=%s", exit, human)
	}
	if !strings.Contains(stderr.String(), "1 of 2 webhook events could not be delivered") {
		t.Fatalf("human stderr = %s", stderr)
	}
}

func TestListenResponseExcerptIsBoundedAndTerminalSafe(t *testing.T) {
	text, truncated := listenResponseExcerpt([]byte("line one\r\n\x1b[2J\tline\x00two"))
	if text != "line one [2J linetwo" || truncated {
		t.Fatalf("excerpt = %q, truncated = %t", text, truncated)
	}
	// One ASCII byte shifts the multi-byte runes so the cut splits one.
	long := []byte("x" + strings.Repeat("é", 300))
	text, truncated = listenResponseExcerpt(long[:listenResponseExcerptBytes+1])
	if !truncated || !utf8.ValidString(text) || strings.ContainsRune(text, utf8.RuneError) || len(text) != listenResponseExcerptBytes-1 {
		t.Fatalf("excerpt = %q (%d bytes), truncated = %t", text, len(text), truncated)
	}
}

func TestListenSlowHandlerDoesNotCountAsStreamIdle(t *testing.T) {
	const firstID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	const secondID = "whev_02ABCDEFGHIJKLMNOPQRSTUVWX"
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		fmt.Fprint(writer, listenEventFrame(firstID, "order.created", `{"sequence":1}`))
		// The first delivery takes 300ms, then the stream stays quiet for
		// less than one timeout. Only the delivery time pushes it past 400ms.
		time.Sleep(600 * time.Millisecond)
		fmt.Fprint(writer, listenEventFrame(secondID, "order.created", `{"sequence":2}`))
		writer.Close()
	}()
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Now: time.Now}
	state := &listenStreamState{}
	opts := Options{Output: "ndjson", Timeout: 400 * time.Millisecond}
	if err := app.consumeListenStreamWithIdleTimeout(context.Background(), reader, opts, local.URL, "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", state); err != nil {
		t.Fatalf("stream error = %#v", err)
	}
	if state.forwarded != 2 || state.cursor != secondID {
		t.Fatalf("forwarded=%d cursor=%s", state.forwarded, state.cursor)
	}
}

func TestListenHandlerSlowerThanTimeoutDoesNotStopListener(t *testing.T) {
	const readyCursor = "whev_00ABCDEFGHIJKLMNOPQRSTUVWX"
	const firstID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	const secondID = "whev_02ABCDEFGHIJKLMNOPQRSTUVWX"
	firstDeliveryAbandoned := make(chan struct{})
	var deliveries atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if deliveries.Add(1) == 1 {
			// A paused handler: it answers only after the listener gives up.
			// The server notices the disconnect only once the body is read.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			close(firstDeliveryAbandoned)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()
	server := listenAPI(t, func(w http.ResponseWriter, r *http.Request, connection int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: ready\ndata: {\"cursor\":%q}\n\n", readyCursor)
		if connection == 1 {
			fmt.Fprint(w, listenEventFrame(firstID, "order.created", `{"sequence":1}`))
			w.(http.Flusher).Flush()
			select {
			case <-firstDeliveryAbandoned:
			case <-r.Context().Done():
				return
			}
			time.Sleep(100 * time.Millisecond)
			fmt.Fprint(w, listenEventFrame(secondID, "order.created", `{"sequence":2}`))
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "2", "--timeout", "300ms", "--output", "ndjson"})
	if exit != ExitAPI || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if strings.Contains(stdout.String(), `"type":"reconnect"`) {
		t.Fatalf("a slow delivery was treated as an idle stream: %s", stdout)
	}
	slow := findListenRecord(t, stdout.String(), "forward")
	if slow["ok"] != false || !strings.Contains(fmt.Sprint(slow["error"]), "did not respond within 300ms") {
		t.Fatalf("slow forward record = %#v", slow)
	}
	checkpoint := findListenRecord(t, stdout.String(), "checkpoint")
	if checkpoint["forwarded"] != float64(1) || checkpoint["failed"] != float64(1) || checkpoint["cursor"] != secondID {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
}

func TestOpenListenStreamUsesFlintRetryDecision(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantRetry bool
	}{
		{"none", `{"error":{"code":"INTERNAL_ERROR","remediation":{"retry":"none"}}}`, false},
		{"after_refresh", `{"error":{"code":"INTERNAL_ERROR","remediation":{"retry":"after_refresh"}}}`, false},
		{"same_request", `{"error":{"code":"INTERNAL_ERROR","remediation":{"retry":"same_request"}}}`, true},
		{"missing_retry", `{"error":{"code":"INTERNAL_ERROR"}}`, false},
		{"proxy", `<html>Internal Server Error</html>`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{HTTPClient: &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}}
			resp, retryable, err := app.openListenStream(t.Context(), "https://api.example.test", "", "/v1/webhook-events/stream", "", time.Second, false)
			if resp != nil || err == nil || err.HTTPStatus != 500 || retryable != tc.wantRetry {
				t.Fatalf("response=%v retryable=%t error=%+v", resp, retryable, err)
			}
		})
	}
}

func TestListenReportsRetryableStreamFailures(t *testing.T) {
	server := listenAPI(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"type":"unavailable_error","code":"SERVICE_UNAVAILABLE","message":"Webhook streaming is temporarily unavailable.","remediation":{"retry":"same_request"}}}`)
	})

	app, stdout, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--for", "400ms", "--output", "ndjson"})
	if exit != ExitNetwork || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	reconnect := findListenRecord(t, stdout.String(), "reconnect")
	if reconnect["reason"] != "SERVICE_UNAVAILABLE" || reconnect["http_status"] != float64(503) || reconnect["attempt"] != float64(1) || reconnect["retry_in_ms"] != float64(250) {
		t.Fatalf("reconnect record = %#v", reconnect)
	}
	failure := listenErrorRecord(t, stdout.String())
	details, _ := failure["details"].(map[string]any)
	lastError, _ := details["last_error"].(map[string]any)
	if failure["code"] != "STREAM_NOT_CONNECTED" || lastError["code"] != "SERVICE_UNAVAILABLE" || !strings.Contains(fmt.Sprint(failure["message"]), "HTTP 503") {
		t.Fatalf("error = %#v", failure)
	}

	stdout.Reset()
	exit = app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--for", "400ms"})
	if exit != ExitNetwork || !strings.Contains(stderr.String(), "returned HTTP 503: Webhook streaming is temporarily unavailable. Reconnecting in 250ms (attempt 1).") || !strings.Contains(stderr.String(), "did not connect within --for 400ms") {
		t.Fatalf("human exit=%d stderr=%s", exit, stderr)
	}
	if strings.Contains(stdout.String(), "Ready") || strings.Contains(stdout.String(), "whsec_") {
		t.Fatalf("human output announced a listener that never connected: %s", stdout)
	}
}

func TestListenBoundedRunReportsMissingEvents(t *testing.T) {
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()
	server := listenAPI(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"\"}\n\n")
		fmt.Fprint(w, listenEventFrame(eventID, "order.created", `{"sequence":1}`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	app, stdout, _ := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "2", "--for", "200ms", "--output", "json"})
	failure := listenErrorRecord(t, stdout.String())
	if exit != ExitWait || failure["code"] != "EXPECTED_EVENTS_NOT_RECEIVED" || !strings.Contains(fmt.Sprint(failure["message"]), "Received 1 of 2 expected webhook events") || !strings.Contains(fmt.Sprint(failure["message"]), "--cursor "+eventID) {
		t.Fatalf("exit=%d output=%s", exit, stdout)
	}

	stdout.Reset()
	if exit := app.Run([]string{"listen", "--forward-to", local.URL, "--for", "200ms", "--output", "json"}); exit != ExitOK {
		t.Fatalf("--for alone exit=%d output=%s", exit, stdout)
	}

	// An explicit MCP event count is an expectation, including exactly 100.
	stdout.Reset()
	app.Context = context.WithValue(context.Background(), mcpSessionKey{}, newMCPSession(nil))
	if exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", fmt.Sprint(mcpMaxStreamEvents), "--for", "200ms", "--output", "json"}); exit != ExitWait {
		t.Fatalf("MCP explicit count exit=%d output=%s", exit, stdout)
	}
}

func TestListenWithoutForwardToPrintsEvents(t *testing.T) {
	t.Setenv(listenSecretEnv, "")
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	payload := `{"data":{"customer":{"customer_id":"cus_1","email":"jane@example.com"}},"event_type":"customer.created"}`
	server := listenAPI(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"\"}\n\n")
		fmt.Fprint(w, listenEventFrame(eventID, "customer.created", payload))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	app, stdout, stderr := testApp(t, server.URL)
	if exit := app.Run([]string{"listen", "--max-events", "1", "--output", "ndjson"}); exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	listener := findListenRecord(t, stdout.String(), "listener")
	if listener["mode"] != "print" || listener["signing_secret"] != nil {
		t.Fatalf("listener = %#v", listener)
	}
	event := findListenRecord(t, stdout.String(), "event")
	if raw, _ := json.Marshal(event["payload"]); event["webhook_event_id"] != eventID || event["event_type"] != "customer.created" || !strings.Contains(string(raw), `"customer_id":"cus_1"`) {
		t.Fatalf("event record = %#v", event)
	}
	if checkpoint := findListenRecord(t, stdout.String(), "checkpoint"); checkpoint["events"] != float64(1) || checkpoint["forwarded"] != float64(0) {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
	if _, err := os.Stat(filepath.Join(app.ConfigDir, listenSecretFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("print-only mode created a signing secret: %v", err)
	}

	stdout.Reset()
	if exit := app.Run([]string{"listen", "--max-events", "1"}); exit != ExitOK {
		t.Fatalf("human exit=%d stdout=%s", exit, stdout)
	}
	human := stdout.String()
	if !strings.Contains(human, "Ready. Printing webhook events; pass --forward-to URL") || !strings.Contains(human, `customer.created `+eventID+` {"customer":{"customer_id":"cus_1","email":"jane@example.com"}}`) || !strings.Contains(human, "processed=1 events=1 withheld=0") || strings.Contains(human, "whsec_") {
		t.Fatalf("human output = %s", human)
	}
}

func TestListenHumanReadyBannerFollowsConnection(t *testing.T) {
	t.Setenv(listenSecretEnv, "")
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer local.Close()
	server := listenAPI(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ready\ndata: {\"cursor\":\"\"}\n\n")
		fmt.Fprint(w, listenEventFrame(eventID, "payment_intent.succeeded", `{"status":"succeeded"}`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	app, stdout, stderr := testApp(t, server.URL)
	if exit := app.Run([]string{"listen", "--forward-to", local.URL, "--max-events", "1"}); exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	output := stdout.String()
	ready := strings.Index(output, "Ready. Forwarding webhook events to "+local.URL+". Stops after 1 event; press Ctrl-C to stop sooner.")
	secret := strings.Index(output, "Webhook signing secret: whsec_")
	forward := strings.Index(output, "payment_intent.succeeded "+eventID+" -> HTTP 204")
	if ready != 0 || secret < 0 || forward < secret || !strings.Contains(output, "(new; saved and reused by later runs)") || !strings.Contains(output, "processed=1 forwarded=1 failed=0 withheld=0") {
		t.Fatalf("human output = %s", output)
	}
}

func TestListenValidatesArgumentsBeforeConnecting(t *testing.T) {
	server := listenAPI(t, func(http.ResponseWriter, *http.Request, int) {
		t.Error("the stream opened despite an invalid argument")
	})
	app, stdout, _ := testApp(t, server.URL)
	for _, test := range []struct {
		args []string
		code string
		text string
	}{
		{args: []string{"--event-type", "payment_intent.succeded"}, code: "INVALID_EVENT_TYPE", text: "Did you mean payment_intent.succeeded?"},
		{args: []string{"--event-type", "payment_intent.succeeded", "--event-type", "payment_intent.created"}, code: "MULTIPLE_EVENT_TYPES", text: "one flint listen per type"},
		{args: []string{"--event-type", "payment_intent.succeeded,payment_intent.created"}, code: "MULTIPLE_EVENT_TYPES", text: "accepts one event type"},
		{args: []string{"--cursor", "bogus"}, code: "INVALID_CURSOR", text: "starts with whev_"},
		{args: []string{"--cursor", "whev_"}, code: "INVALID_CURSOR", text: "starts with whev_"},
		{args: []string{"--forward-to", "https://example.com/webhooks"}, code: "INVALID_FORWARD_URL", text: "host.docker.internal"},
		{args: []string{"--forward-to", " "}, code: "INVALID_FORWARD_URL", text: "absolute HTTP or HTTPS URL"},
	} {
		stdout.Reset()
		exit := app.Run(append(append([]string{"listen"}, test.args...), "--for", "1s", "--output", "json"))
		failure := listenErrorRecord(t, stdout.String())
		if exit != ExitUsage || failure["code"] != test.code || !strings.Contains(fmt.Sprint(failure["message"]), test.text) {
			t.Errorf("%v: exit=%d output=%s", test.args, exit, stdout)
		}
	}
}

func TestListenEventTypeSuggestions(t *testing.T) {
	known := listenEventTypes()
	if len(known) < 100 {
		t.Fatalf("event types from the embedded contract = %d", len(known))
	}
	if got := listenEventTypeSuggestions(known, "Payment_Intent.Succeded"); len(got) == 0 || got[0] != "payment_intent.succeeded" {
		t.Fatalf("typo suggestions = %#v", got)
	}
	got := listenEventTypeSuggestions(known, "customer")
	if len(got) == 0 || !strings.HasPrefix(got[0], "customer.") {
		t.Fatalf("resource suggestions = %#v", got)
	}
}

func TestListenErrorCatalogListsBoundedRunCodes(t *testing.T) {
	entries, _ := errorCatalog()["listen_codes"].([]map[string]any)
	want := map[string]int{
		"WEBHOOK_FORWARD_FAILED":       ExitAPI,
		"EXPECTED_EVENTS_NOT_RECEIVED": ExitWait,
		"STREAM_NOT_CONNECTED":         ExitNetwork,
		"INVALID_EVENT_TYPE":           ExitUsage,
		"MULTIPLE_EVENT_TYPES":         ExitUsage,
		"INVALID_WEBHOOK_SECRET":       ExitUsage,
		"LISTEN_SECRET_INVALID":        ExitAuth,
		"LISTEN_SECRET_SAVE_FAILED":    ExitAuth,
	}
	for _, entry := range entries {
		code, _ := entry["code"].(string)
		if exit, ok := want[code]; ok {
			if entry["exit_code"] != exit {
				t.Errorf("%s exit code = %#v, want %d", code, entry["exit_code"], exit)
			}
			delete(want, code)
		}
		if code == "WEBHOOK_PAYLOAD_NOT_VISIBLE" {
			t.Error("WEBHOOK_PAYLOAD_NOT_VISIBLE no longer ends a listener")
		}
	}
	for code := range want {
		t.Errorf("listen error catalog is missing %s", code)
	}
}
