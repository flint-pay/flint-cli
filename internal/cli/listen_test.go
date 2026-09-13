package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestListenTimeoutBoundsIdleStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	app, _, stderr := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--timeout", "25ms"})
	if exit != ExitNetwork || !strings.Contains(stderr.String(), "stopped sending data") {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
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

func TestListenFailsWhenPayloadIsNotVisible(t *testing.T) {
	const eventID = "whev_01ABCDEFGHIJKLMNOPQRSTUVWX"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "id: %s\nevent: webhook.event\ndata: {\"webhook_event_id\":\"%s\",\"event_type\":\"order.paid\"}\n\n", eventID, eventID)
	}))
	defer server.Close()

	app, stdout, _ := testApp(t, server.URL)
	exit := app.Run([]string{"listen", "--forward-to", "http://127.0.0.1:1/webhooks", "--max-events", "1", "--output", "json"})
	if exit != ExitAuth || !strings.Contains(stdout.String(), `"code":"WEBHOOK_PAYLOAD_NOT_VISIBLE"`) || !strings.Contains(stdout.String(), `"type":"checkpoint"`) {
		t.Fatalf("exit=%d output=%s", exit, stdout)
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
