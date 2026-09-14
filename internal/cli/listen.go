package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	apispec "github.com/flint-pay/flint-cli/internal/spec"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	webhooksigning "github.com/flint-pay/flint-cli/internal/webhooksigning"
)

const (
	listenMaxSSELineBytes = 16 << 20
	listenUserAgent       = "Flint-Webhooks/1.0"
)

var errListenMaxEventsReached = errors.New("maximum webhook event count reached")

type listenSSEFrame struct {
	Event string
	ID    string
	Data  []byte
}

type listenWebhookEvent struct {
	WebhookEventID string          `json:"webhook_event_id"`
	EventType      string          `json:"event_type"`
	Payload        json.RawMessage `json:"payload"`
}

type listenStreamState struct {
	cursor    string
	processed int
	events    int
	withheld  int
}

type listenOpenResult struct {
	resp *http.Response
	err  error
}

type listenCancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *listenCancelOnClose) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}

type listenActivityReader struct {
	reader   io.Reader
	activity chan<- struct{}
}

func (r listenActivityReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n > 0 {
		select {
		case r.activity <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (a *App) executeListen(ctx context.Context, _ *Command, opts Options, req preparedRequest, key, baseURL string) (any, *CLIError) {
	if opts.Output == "json" && opts.MaxEvents == 0 && opts.For == 0 {
		return nil, usageError("STREAM_BOUND_REQUIRED", "JSON streaming requires --max-events or --for so automation cannot hang indefinitely. Use --output ndjson for an intentionally unbounded stream.", "for")
	}
	forwardTo := lastRawOption(opts, "forward-to")
	if _, err := validateLocalForwardURL(forwardTo); err != nil {
		return nil, usageError("INVALID_FORWARD_URL", err.Error(), "forward_to")
	}
	secret, err := webhooksigning.GenerateSecret()
	if err != nil {
		return nil, networkError("WEBHOOK_SECRET_GENERATION_FAILED", "Could not generate the local webhook signing secret.", err)
	}
	if e := a.writeListenStarted(opts, forwardTo, secret); e != nil {
		return nil, e
	}

	streamPath, initialCursor, e := listenStreamPath(req.Path)
	if e != nil {
		return nil, e
	}
	state := &listenStreamState{cursor: initialCursor}
	returnWithCheckpoint := func(primary *CLIError) (any, *CLIError) {
		if checkpointErr := a.writeListenCheckpoint(opts, state); checkpointErr != nil {
			return nil, checkpointErr
		}
		return nil, primary
	}
	streamCtx := ctx
	cancel := func() {}
	if opts.For > 0 {
		streamCtx, cancel = context.WithTimeout(ctx, opts.For)
	}
	defer cancel()

	reconnects := 0
	for {
		if streamCtx.Err() != nil {
			if checkpointErr := a.writeListenCheckpoint(opts, state); checkpointErr != nil {
				return nil, checkpointErr
			}
			return outputHandled{}, nil
		}
		resp, retryable, openErr := a.openListenStream(streamCtx, baseURL, key, streamPath, state.cursor, opts.Timeout, opts.Debug)
		if openErr != nil {
			if streamCtx.Err() != nil {
				continue
			}
			if !retryable {
				return returnWithCheckpoint(openErr)
			}
			reconnects++
			if !sleepContext(streamCtx, listenReconnectDelay(reconnects)) {
				continue
			}
			continue
		}
		connectedAt := time.Now()
		processedBefore := state.processed
		consumeErr := a.consumeListenStreamWithIdleTimeout(streamCtx, resp.Body, opts, forwardTo, secret, state)
		if state.processed > processedBefore || time.Since(connectedAt) >= 2*listenReconnectDelay(5) {
			reconnects = 0
		}
		if consumeErr != nil {
			if consumeErr.Code == "STREAM_READ_FAILED" {
				reconnects++
				if !sleepContext(streamCtx, listenReconnectDelay(reconnects)) {
					continue
				}
				continue
			}
			return returnWithCheckpoint(consumeErr)
		}
		if opts.MaxEvents > 0 && state.processed >= opts.MaxEvents {
			if checkpointErr := a.writeListenCheckpoint(opts, state); checkpointErr != nil {
				return nil, checkpointErr
			}
			return outputHandled{}, nil
		}
		if streamCtx.Err() != nil {
			continue
		}
		reconnects++
		if !sleepContext(streamCtx, listenReconnectDelay(reconnects)) {
			continue
		}
	}
}

func lastRawOption(opts Options, name string) string {
	values := opts.Raw[name]
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[len(values)-1])
}

func listenStreamPath(path string) (string, string, *CLIError) {
	parsed, err := url.Parse(path)
	if err != nil {
		return "", "", usageError("INVALID_CURSOR", "Could not parse the webhook stream cursor.", "cursor")
	}
	query := parsed.Query()
	cursor := strings.TrimSpace(query.Get("after_event_id"))
	query.Del("after_event_id")
	parsed.RawQuery = query.Encode()
	return parsed.String(), cursor, nil
}

func (a *App) openListenStream(ctx context.Context, baseURL, key, path, cursor string, timeout time.Duration, debug bool) (*http.Response, bool, *CLIError) {
	client := a.HTTPClient
	if client == nil {
		// The opening timer below enforces this stream's header timeout.
		client = defaultAPIHTTPClient
	} else if client.CheckRedirect == nil {
		clone := *client
		clone.CheckRedirect = rejectAPIRedirect
		client = &clone
	}
	requestCtx, cancelRequest := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		cancelRequest()
		return nil, false, usageError("INVALID_REQUEST_URL", err.Error(), "path")
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "flintpay-cli/"+a.Info.Version)
	req.Header.Set("X-Flint-CLI-Version", a.Info.Version)
	req.Header.Set("Flint-Version", apispec.APIVersion())
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	if debug {
		fmt.Fprintf(a.Stderr, "debug: GET %s (webhook event stream)\n", path)
	}
	result := make(chan listenOpenResult)
	abandoned := make(chan struct{})
	go func() {
		resp, err := client.Do(req)
		select {
		case result <- listenOpenResult{resp: resp, err: err}:
		case <-abandoned:
			if resp != nil {
				_ = resp.Body.Close()
			}
		}
	}()
	openDeadline := time.Now().Add(timeout)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var open listenOpenResult
	select {
	case open = <-result:
	case <-timer.C:
		cancelRequest()
		close(abandoned)
		return nil, false, networkError("REQUEST_TIMEOUT", "Timed out waiting for the webhook event stream to open.", context.DeadlineExceeded)
	case <-ctx.Done():
		cancelRequest()
		close(abandoned)
		return nil, true, networkError("REQUEST_TIMEOUT", "The webhook event stream closed.", ctx.Err())
	}
	resp, err := open.resp, open.err
	if err != nil {
		cancelRequest()
		if ctx.Err() != nil {
			return nil, true, networkError("REQUEST_TIMEOUT", "The webhook event stream closed.", ctx.Err())
		}
		return nil, true, networkError("NETWORK_ERROR", "Could not connect to the webhook event stream.", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Failed responses are not streams. Keep their body reads within the
		// opening deadline, including time already spent waiting for headers.
		bodyTimer := time.AfterFunc(time.Until(openDeadline), cancelRequest)
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		bodyTimer.Stop()
		resp.Body.Close()
		requestErr := requestCtx.Err()
		cancelRequest()
		if readErr != nil {
			if requestErr != nil {
				return nil, retryableStatus(resp.StatusCode), networkError("REQUEST_TIMEOUT", "Timed out reading the webhook stream error response.", requestErr)
			}
			return nil, retryableStatus(resp.StatusCode), networkError("RESPONSE_READ_FAILED", "The Flint response could not be read.", readErr)
		}
		var value any
		_ = json.Unmarshal(raw, &value)
		apiErr := apiErrorFromResponse(&apiResponse{Status: resp.StatusCode, Headers: resp.Header.Clone(), Raw: raw, Value: value})
		return nil, retryableStatus(resp.StatusCode), apiErr
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		resp.Body.Close()
		cancelRequest()
		return nil, false, invalidResponseError("INVALID_STREAM_CONTENT_TYPE", "Flint did not return an SSE webhook event stream.", err)
	}
	resp.Body = &listenCancelOnClose{ReadCloser: resp.Body, cancel: cancelRequest}
	return resp, false, nil
}

func (a *App) consumeListenStreamWithIdleTimeout(ctx context.Context, body io.ReadCloser, opts Options, forwardTo, secret string, state *listenStreamState) *CLIError {
	activity := make(chan struct{}, 1)
	result := make(chan *CLIError, 1)
	consumerCtx, cancelConsumer := context.WithCancel(ctx)
	defer cancelConsumer()
	go func() {
		result <- a.consumeListenStream(consumerCtx, listenActivityReader{reader: body, activity: activity}, opts, forwardTo, secret, state)
	}()
	stopConsumer := func() {
		cancelConsumer()
		_ = body.Close()
		// The consumer owns cursor and output writes. Join it before the caller
		// writes a checkpoint or returns to avoid post-checkpoint mutations.
		<-result
	}
	timer := time.NewTimer(opts.Timeout)
	defer timer.Stop()
	for {
		select {
		case streamErr := <-result:
			cancelConsumer()
			_ = body.Close()
			return streamErr
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(opts.Timeout)
		case <-timer.C:
			stopConsumer()
			return networkError("REQUEST_TIMEOUT", "The webhook event stream stopped sending data.", context.DeadlineExceeded)
		case <-ctx.Done():
			stopConsumer()
			return nil
		}
	}
}

func (a *App) consumeListenStream(ctx context.Context, body io.Reader, opts Options, forwardTo, secret string, state *listenStreamState) *CLIError {
	err := readListenSSE(body, func(frame listenSSEFrame) error {
		switch frame.Event {
		case "ready":
			var ready struct {
				Cursor string `json:"cursor"`
			}
			if err := json.Unmarshal(frame.Data, &ready); err != nil {
				return invalidResponseError("INVALID_STREAM_EVENT", "Flint returned an invalid ready stream event.", err)
			}
			if strings.TrimSpace(ready.Cursor) != "" {
				state.cursor = strings.TrimSpace(ready.Cursor)
			}
			if writeErr := a.writeListenRecord(opts, map[string]any{"type": "ready", "cursor": state.cursor}, ""); writeErr != nil {
				return writeErr
			}
			return nil
		case "gap":
			var gap struct {
				Reason          string `json:"reason"`
				RequestedCursor string `json:"requested_cursor"`
				ResumeCursor    string `json:"resume_cursor"`
			}
			if err := json.Unmarshal(frame.Data, &gap); err != nil {
				return invalidResponseError("INVALID_STREAM_EVENT", "Flint returned an invalid gap stream event.", err)
			}
			state.cursor = strings.TrimSpace(gap.ResumeCursor)
			human := fmt.Sprintf("Stream gap: %s. Resuming from %s", gap.Reason, state.cursor)
			if writeErr := a.writeListenRecord(opts, map[string]any{"type": "gap", "reason": gap.Reason, "requested_cursor": gap.RequestedCursor, "resume_cursor": state.cursor}, human); writeErr != nil {
				return writeErr
			}
			return nil
		case "disconnect":
			var disconnect struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(frame.Data, &disconnect); err != nil {
				return invalidResponseError("INVALID_STREAM_EVENT", "Flint returned an invalid disconnect stream event.", err)
			}
			if writeErr := a.writeListenRecord(opts, map[string]any{"type": "disconnect", "reason": disconnect.Reason, "cursor": state.cursor}, "Stream disconnected: "+disconnect.Reason+". Reconnecting"); writeErr != nil {
				return writeErr
			}
			return nil
		case "withheld":
			var withheld struct {
				WebhookEventID string   `json:"webhook_event_id"`
				EventType      string   `json:"event_type"`
				Reason         string   `json:"reason"`
				ResourceType   string   `json:"resource_type"`
				RequiredScopes []string `json:"required_scopes"`
			}
			if err := json.Unmarshal(frame.Data, &withheld); err != nil {
				return invalidResponseError("INVALID_STREAM_EVENT", "Flint returned an invalid withheld stream event.", err)
			}
			withheld.WebhookEventID = strings.TrimSpace(withheld.WebhookEventID)
			if withheld.WebhookEventID == "" || frame.ID == "" || frame.ID != withheld.WebhookEventID {
				return invalidResponseError("INVALID_STREAM_EVENT_ID", "The withheld stream event ID did not match its payload.", nil)
			}
			state.cursor = withheld.WebhookEventID
			state.processed++
			state.withheld++
			record := map[string]any{
				"type":             "withheld",
				"webhook_event_id": withheld.WebhookEventID,
				"event_type":       withheld.EventType,
				"reason":           withheld.Reason,
				"resource_type":    withheld.ResourceType,
				"required_scopes":  withheld.RequiredScopes,
			}
			human := fmt.Sprintf("Withheld %s %s: %s", withheld.EventType, withheld.WebhookEventID, withheld.Reason)
			if len(withheld.RequiredScopes) > 0 {
				human += ". Required scope: " + strings.Join(withheld.RequiredScopes, " or ")
			}
			if writeErr := a.writeListenRecord(opts, record, human); writeErr != nil {
				return writeErr
			}
			if opts.MaxEvents > 0 && state.processed >= opts.MaxEvents {
				return errListenMaxEventsReached
			}
			return nil
		case "webhook.event":
			if eventErr := a.consumeListenWebhookEvent(ctx, frame, opts, forwardTo, secret, state); eventErr != nil {
				return eventErr
			}
			if opts.MaxEvents > 0 && state.processed >= opts.MaxEvents {
				return errListenMaxEventsReached
			}
			return nil
		default:
			return nil
		}
	})
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, errListenMaxEventsReached) || ctx.Err() != nil {
		return nil
	}
	var cliErr *CLIError
	if errors.As(err, &cliErr) {
		return cliErr
	}
	return networkError("STREAM_READ_FAILED", "The webhook event stream could not be read.", err)
}

func (a *App) consumeListenWebhookEvent(ctx context.Context, frame listenSSEFrame, opts Options, forwardTo, secret string, state *listenStreamState) *CLIError {
	var event listenWebhookEvent
	if err := json.Unmarshal(frame.Data, &event); err != nil {
		return invalidResponseError("INVALID_STREAM_EVENT", "Flint returned an invalid webhook event.", err)
	}
	event.WebhookEventID = strings.TrimSpace(event.WebhookEventID)
	if event.WebhookEventID == "" || frame.ID == "" || frame.ID != event.WebhookEventID {
		return invalidResponseError("INVALID_STREAM_EVENT_ID", "The webhook stream event ID did not match its payload.", nil)
	}
	if len(event.Payload) == 0 || bytes.Equal(bytes.TrimSpace(event.Payload), []byte("null")) {
		e := cliError(ExitAuth, "authorization_error", "WEBHOOK_PAYLOAD_NOT_VISIBLE", "This API key can read webhook event metadata but cannot read the underlying resource payload. Use a key with read access to the event's resource type.")
		e.Param = "api_key"
		return e
	}
	if !json.Valid(event.Payload) {
		return invalidResponseError("INVALID_STREAM_PAYLOAD", "The webhook stream payload is not valid JSON.", nil)
	}
	result := a.forwardListenPayload(ctx, opts.Timeout, forwardTo, secret, event)
	state.processed++
	state.events++
	state.cursor = event.WebhookEventID
	record := map[string]any{
		"type":             "forward",
		"webhook_event_id": event.WebhookEventID,
		"event_type":       event.EventType,
		"status_code":      result.statusCode,
		"latency_ms":       result.latency.Milliseconds(),
	}
	humanStatus := fmt.Sprintf("%s %s -> HTTP %d (%s)", event.EventType, event.WebhookEventID, result.statusCode, result.latency.Round(time.Millisecond))
	if result.err != nil {
		record["error"] = result.err.Error()
		humanStatus = fmt.Sprintf("%s %s -> forwarding failed (%s): %s", event.EventType, event.WebhookEventID, result.latency.Round(time.Millisecond), result.err.Error())
	}
	return a.writeListenRecord(opts, record, humanStatus)
}

type listenForwardResult struct {
	statusCode int
	latency    time.Duration
	err        error
}

func (a *App) forwardListenPayload(ctx context.Context, timeout time.Duration, forwardTo, secret string, event listenWebhookEvent) listenForwardResult {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	timestamp := a.Now().Unix()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, forwardTo, bytes.NewReader(event.Payload))
	if err != nil {
		return listenForwardResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", listenUserAgent)
	req.Header.Set("X-Flint-Signature", webhooksigning.LegacySignatureHeader(timestamp, event.Payload, secret))
	req.Header.Set("X-Flint-Webhook-ID", event.WebhookEventID)
	req.Header.Set("X-Flint-Event-Type", event.EventType)
	req.Header.Set("webhook-id", event.WebhookEventID)
	req.Header.Set("webhook-timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("webhook-signature", webhooksigning.StandardWebhookSignatureHeader(event.WebhookEventID, timestamp, event.Payload, secret))

	client := a.ForwardHTTPClient
	if client == nil {
		client = newLocalForwardHTTPClient(timeout)
		// This transport pins a loopback destination for one delivery. Close
		// its idle connection after the response body has been closed.
		defer client.CloseIdleConnections()
	} else if client.CheckRedirect == nil {
		clone := *client
		clone.CheckRedirect = rejectLocalForwardRedirect
		client = &clone
	}
	started := a.Now()
	resp, err := client.Do(req)
	latency := a.Now().Sub(started)
	if latency < 0 {
		latency = 0
	}
	if err != nil {
		return listenForwardResult{latency: latency, err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return listenForwardResult{statusCode: resp.StatusCode, latency: latency}
}

func validateLocalForwardURL(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return nil, fmt.Errorf("--forward-to must be an absolute loopback HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("--forward-to must not contain credentials or a fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("--forward-to must use HTTP or HTTPS")
	}
	hostname := strings.ToLower(parsed.Hostname())
	ip := net.ParseIP(hostname)
	if hostname != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("--forward-to must use localhost or a loopback IP address")
	}
	return parsed, nil
}

func newLocalForwardHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: min(timeout, 10*time.Second), KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:               nil,
		TLSHandshakeTimeout: min(timeout, 10*time.Second),
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			if len(addresses) == 0 {
				return nil, fmt.Errorf("local forwarding target did not resolve")
			}
			return dialLoopbackAddresses(ctx, network, port, addresses, dialer.DialContext)
		},
	}
	return &http.Client{Timeout: timeout, Transport: transport, CheckRedirect: rejectLocalForwardRedirect}
}

func dialLoopbackAddresses(ctx context.Context, network, port string, addresses []netip.Addr, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	for _, address := range addresses {
		if !address.IsLoopback() {
			return nil, fmt.Errorf("local forwarding target resolved outside loopback")
		}
	}
	var lastErr error
	for _, resolved := range addresses {
		connection, dialErr := dial(ctx, network, net.JoinHostPort(resolved.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("could not connect to any loopback address: %w", lastErr)
}

func rejectLocalForwardRedirect(*http.Request, []*http.Request) error {
	// A redirect could send the ephemeral signing secret and signed payload to a
	// different origin, so local forwarding always reports the redirect itself.
	return http.ErrUseLastResponse
}

func readListenSSE(reader io.Reader, handle func(listenSSEFrame) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), listenMaxSSELineBytes)
	frame := listenSSEFrame{}
	dataLines := make([]string, 0, 1)
	dispatch := func() error {
		if len(dataLines) == 0 {
			frame = listenSSEFrame{}
			return nil
		}
		frame.Data = []byte(strings.Join(dataLines, "\n"))
		if frame.Event == "" {
			frame.Event = "message"
		}
		if err := handle(frame); err != nil {
			return err
		}
		frame = listenSSEFrame{}
		dataLines = dataLines[:0]
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		}
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "event":
			frame.Event = value
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				frame.ID = value
			}
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := dispatch(); err != nil {
		return err
	}
	return io.EOF
}

func listenReconnectDelay(attempt int) time.Duration {
	delay := 250 * time.Millisecond
	for i := 1; i < attempt && delay < 5*time.Second; i++ {
		delay *= 2
	}
	return min(delay, 5*time.Second)
}

func (a *App) writeListenStarted(opts Options, forwardTo, secret string) *CLIError {
	if opts.Output == "json" || opts.Output == "ndjson" {
		return a.writeListenRecord(opts, map[string]any{"type": "listener", "forward_to": forwardTo, "signing_secret": secret}, "")
	}
	if _, err := fmt.Fprintf(a.Stdout, "Webhook signing secret: %s\nForwarding to %s\n", secret, forwardTo); err != nil {
		return networkError("OUTPUT_WRITE_FAILED", "Could not write listener output.", err)
	}
	return nil
}

func (a *App) writeListenRecord(opts Options, record map[string]any, human string) *CLIError {
	if opts.Output == "json" || opts.Output == "ndjson" {
		if err := writeJSON(a.Stdout, record); err != nil {
			return networkError("OUTPUT_WRITE_FAILED", "Could not write listener output.", err)
		}
		return nil
	}
	if human != "" && !opts.Quiet {
		if _, err := fmt.Fprintln(a.Stdout, human); err != nil {
			return networkError("OUTPUT_WRITE_FAILED", "Could not write listener output.", err)
		}
	}
	return nil
}

func (a *App) writeListenCheckpoint(opts Options, state *listenStreamState) *CLIError {
	record := map[string]any{"type": "checkpoint", "cursor": state.cursor, "processed": state.processed, "events": state.events, "withheld": state.withheld}
	if opts.Output == "json" || opts.Output == "ndjson" {
		return a.writeListenRecord(opts, record, "")
	}
	if opts.Quiet {
		return nil
	}
	human := fmt.Sprintf("Checkpoint: cursor=%s processed=%d forwarded=%d withheld=%d", state.cursor, state.processed, state.events, state.withheld)
	return a.writeListenRecord(opts, record, human)
}
