package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	apispec "github.com/flint-pay/flint-cli/internal/spec"
	webhooksigning "github.com/flint-pay/flint-cli/internal/webhooksigning"
)

const (
	listenMaxSSELineBytes = 16 << 20
	listenUserAgent       = "Flint-Webhooks/1.0"
	// The generated signing secret lives in its own file beside config.json.
	// Older CLI versions parse config.json strictly and reject unknown fields.
	listenSecretFile = "listen-signing-secret"
	listenSecretEnv  = "FLINT_WEBHOOK_SECRET"
	// A listener running in a container reaches a handler on the Docker
	// Desktop host through this name.
	listenDockerHost           = "host.docker.internal"
	listenResponseExcerptBytes = 512
	listenPayloadSummaryRunes  = 200
)

var (
	errListenMaxEventsReached   = errors.New("maximum webhook event count reached")
	errListenSavedSecretInvalid = errors.New("the saved webhook signing secret is invalid")
)

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
	forwarded int
	failed    int
	withheld  int
	// replayCursor is the cursor before the first failed delivery. Resuming
	// there delivers that event and every later one again.
	replayCursor string
	firstFailed  string
	connected    bool
	disconnect   string
	// banner is the human ready text, printed once the stream is open.
	banner    string
	announced bool
	// The consumer sets these around each local delivery. The stream is not
	// read meanwhile, so the idle timer must not count that time.
	forwarding   atomic.Bool
	forwardEnded atomic.Int64
}

// deliveryGrace is how much longer the idle timer must wait because a local
// delivery is running or finished less than one timeout ago.
func (s *listenStreamState) deliveryGrace(timeout time.Duration) time.Duration {
	if s.forwarding.Load() {
		return timeout
	}
	if ended := s.forwardEnded.Load(); ended != 0 {
		return timeout - time.Since(time.Unix(0, ended))
	}
	return 0
}

type listenSecret struct {
	value string
	// source is flag, environment, saved, generated, or temporary.
	source  string
	warning string
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
	if lastRawOption(opts, "print-secret") == "true" {
		secret, e := a.listenSigningSecret(opts, true)
		if e != nil {
			return nil, e
		}
		return a.writeListenSecret(opts, secret)
	}
	if opts.Output == "json" && opts.MaxEvents == 0 && opts.For == 0 {
		return nil, usageError("STREAM_BOUND_REQUIRED", "JSON streaming requires --max-events or --for so automation cannot hang indefinitely. Use --output ndjson for an intentionally unbounded stream.", "for")
	}
	if e := validateListenEventType(opts.Raw["event-type"]); e != nil {
		return nil, e
	}
	streamPath, initialCursor, e := listenStreamPath(req.Path)
	if e != nil {
		return nil, e
	}
	forwardTo := lastRawOption(opts, "forward-to")
	var secret listenSecret
	if len(opts.Raw["forward-to"]) > 0 {
		if _, err := validateLocalForwardURL(forwardTo); err != nil {
			return nil, usageError("INVALID_FORWARD_URL", err.Error(), "forward_to")
		}
		if secret, e = a.listenSigningSecret(opts, false); e != nil {
			return nil, e
		}
	}
	if e := a.writeListenStarted(opts, forwardTo, secret); e != nil {
		return nil, e
	}

	state := &listenStreamState{cursor: initialCursor, banner: listenBanner(opts, forwardTo, secret)}
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
	var lastFailure *CLIError
	// Every reconnect is reported, so a stream that keeps failing is never a
	// silent wait.
	reconnect := func(cause *CLIError) *CLIError {
		reconnects++
		lastFailure = cause
		delay := listenReconnectDelay(reconnects)
		if e := a.writeListenReconnect(opts, state, cause, reconnects, delay); e != nil {
			return e
		}
		sleepContext(streamCtx, delay)
		return nil
	}
	for {
		if streamCtx.Err() != nil {
			return a.finishListen(ctx, opts, state, lastFailure)
		}
		resp, retryable, openErr := a.openListenStream(streamCtx, baseURL, key, streamPath, state.cursor, opts.Timeout, opts.Debug)
		if openErr != nil {
			if streamCtx.Err() != nil {
				continue
			}
			if !retryable {
				return returnWithCheckpoint(openErr)
			}
			if e := reconnect(openErr); e != nil {
				return returnWithCheckpoint(e)
			}
			continue
		}
		state.connected = true
		state.disconnect = ""
		connectedAt := time.Now()
		processedBefore := state.processed
		consumeErr := a.consumeListenStreamWithIdleTimeout(streamCtx, resp.Body, opts, forwardTo, secret.value, state)
		if state.processed > processedBefore || time.Since(connectedAt) >= 2*listenReconnectDelay(5) {
			reconnects = 0
		}
		if consumeErr != nil && consumeErr.Code != "STREAM_READ_FAILED" && consumeErr.Code != "STREAM_IDLE_TIMEOUT" {
			return returnWithCheckpoint(consumeErr)
		}
		if opts.MaxEvents > 0 && state.processed >= opts.MaxEvents {
			return a.finishListen(ctx, opts, state, lastFailure)
		}
		if streamCtx.Err() != nil {
			continue
		}
		if consumeErr == nil {
			consumeErr = listenStreamClosedError(state.disconnect)
		}
		if e := reconnect(consumeErr); e != nil {
			return returnWithCheckpoint(e)
		}
	}
}

// finishListen writes the final checkpoint. A run that ended at its bound then
// reports whether it did what was asked: it connected, every delivery
// succeeded, and with both --max-events and --for, every expected event came.
func (a *App) finishListen(ctx context.Context, opts Options, state *listenStreamState, lastFailure *CLIError) (any, *CLIError) {
	if e := a.writeListenCheckpoint(opts, state); e != nil {
		return nil, e
	}
	// Ctrl-C or a canceled MCP call interrupts the run; it is not the bound.
	if ctx.Err() != nil || (opts.MaxEvents == 0 && opts.For == 0) {
		return outputHandled{}, nil
	}
	if !state.connected {
		return nil, listenNotConnectedError(opts, lastFailure)
	}
	if state.failed > 0 {
		return nil, listenForwardFailedError(opts, state)
	}
	if opts.MaxEvents > 0 && opts.For > 0 && state.processed < opts.MaxEvents {
		message := fmt.Sprintf("Received %d of %d expected webhook events before --for %s elapsed.", state.processed, opts.MaxEvents, opts.For)
		if state.cursor != "" {
			message += " Resume after the last one with --cursor " + state.cursor + "."
		}
		e := cliError(ExitWait, "wait_condition_unmet", "EXPECTED_EVENTS_NOT_RECEIVED", message)
		e.Details = map[string]any{"expected": opts.MaxEvents, "received": state.processed, "cursor": state.cursor}
		return nil, e
	}
	return outputHandled{}, nil
}

func listenNotConnectedError(opts Options, last *CLIError) *CLIError {
	message := fmt.Sprintf("The webhook event stream did not connect within --for %s, so no events were received.", opts.For)
	if last != nil {
		message += " Last error: " + listenFailureMessage(last)
	}
	e := networkError("STREAM_NOT_CONNECTED", message, nil)
	if last != nil {
		lastError := map[string]any{"code": last.Code, "message": last.Message}
		if last.HTTPStatus != 0 {
			lastError["http_status"] = last.HTTPStatus
		}
		e.Details = map[string]any{"last_error": lastError}
	}
	return e
}

func listenForwardFailedError(opts Options, state *listenStreamState) *CLIError {
	noun := "events"
	if state.events == 1 {
		noun = "event"
	}
	message := fmt.Sprintf("%d of %d webhook %s could not be delivered to %s. %s", state.failed, state.events, noun, lastRawOption(opts, "forward-to"), listenReplayHint(state))
	e := cliError(ExitAPI, "api_error", "WEBHOOK_FORWARD_FAILED", message)
	e.Param = "forward_to"
	e.Details = map[string]any{"forwarded": state.forwarded, "failed": state.failed, "first_failed_event_id": state.firstFailed, "replay_cursor": state.replayCursor}
	return e
}

func listenReplayHint(state *listenStreamState) string {
	if state.replayCursor != "" {
		return "After fixing the handler, replay from the first failure with --cursor " + state.replayCursor + "."
	}
	// --cursor resumes after an event, and this stream gave no cursor before
	// the first failure, so there is nothing earlier to resume from.
	return "The stream gave no cursor before " + state.firstFailed + ", so it cannot be replayed; trigger that event again after fixing the handler."
}

func listenStreamClosedError(disconnect string) *CLIError {
	message := "Flint closed the webhook event stream."
	if disconnect != "" {
		message = fmt.Sprintf("Flint closed the webhook event stream (%s).", terminalSafe(disconnect))
	}
	return networkError("STREAM_CLOSED", message, nil)
}

// listenFailureMessage is one terminal-safe sentence for a stream failure,
// including the HTTP status or transport reason the error carries.
func listenFailureMessage(e *CLIError) string {
	message := strings.TrimSuffix(strings.TrimSpace(e.Message), ".")
	switch {
	case e.HTTPStatus != 0:
		message = fmt.Sprintf("The webhook event stream returned HTTP %d: %s", e.HTTPStatus, message)
	case e.Cause != nil && !errors.Is(e.Cause, context.DeadlineExceeded):
		message += ": " + strings.TrimSuffix(transportErrorReason(e.Cause), ".")
	}
	return terminalSafe(message) + "."
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
	cursors := query["after_event_id"]
	if len(cursors) > 1 {
		return "", "", usageError("INVALID_CURSOR", "--cursor accepts one webhook event ID.", "cursor")
	}
	cursor := ""
	if len(cursors) == 1 {
		cursor = strings.TrimSpace(cursors[0])
		if e := validateListenCursor(cursor); e != nil {
			return "", "", e
		}
	}
	query.Del("after_event_id")
	parsed.RawQuery = query.Encode()
	return parsed.String(), cursor, nil
}

// validateListenCursor checks the shape of a webhook event ID before it is
// sent as Last-Event-ID.
func validateListenCursor(cursor string) *CLIError {
	id, ok := strings.CutPrefix(cursor, "whev_")
	valid := ok && id != "" && strings.IndexFunc(id, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z')
	}) < 0
	if valid {
		return nil
	}
	return usageError("INVALID_CURSOR", fmt.Sprintf("--cursor must be a webhook event ID that starts with whev_, such as the cursor from a listen checkpoint; got %q.", terminalSafe(cursor)), "cursor")
}

// listenEventTypes lists the values the stream's event_type filter accepts
// in the embedded API contract.
func listenEventTypes() []string {
	operation, ok := openAPIOperationByID("streamWebhookEvents")
	if !ok {
		return nil
	}
	for _, parameter := range operation.Parameters {
		if parameter["name"] != "event_type" {
			continue
		}
		schema, _ := parameter["schema"].(map[string]any)
		values, _ := schema["enum"].([]any)
		types := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				types = append(types, text)
			}
		}
		return types
	}
	return nil
}

func validateListenEventType(values []string) *CLIError {
	if len(values) == 0 {
		return nil
	}
	if len(values) > 1 || strings.Contains(values[0], ",") {
		return usageError("MULTIPLE_EVENT_TYPES", "--event-type accepts one event type because the webhook stream filters on a single type. Omit it to receive every event, or run one flint listen per type.", "event_type")
	}
	value := values[0]
	if strings.TrimSpace(value) == "" {
		return usageError("INVALID_EVENT_TYPE", "--event-type must not be empty.", "event_type")
	}
	known := listenEventTypes()
	if len(known) == 0 || slices.Contains(known, value) {
		return nil
	}
	suggestions := listenEventTypeSuggestions(known, value)
	message := fmt.Sprintf("Unknown webhook event type %q.", terminalSafe(value))
	if len(suggestions) > 0 {
		message += " Did you mean " + suggestions[0] + "?"
	}
	message += " Run flint help events for the event catalog; if this type is newer than this CLI, run flint upgrade."
	e := usageError("INVALID_EVENT_TYPE", message, "event_type")
	e.Details = []map[string]any{{"input": value, "suggestions": suggestions}}
	return e
}

// listenEventTypeSuggestions ranks event types by edit distance. A resource
// name such as payment_intent yields that resource's event types.
func listenEventTypeSuggestions(known []string, input string) []string {
	needle := strings.ToLower(strings.TrimSpace(input))
	var nearby, within []string
	for _, candidate := range known {
		if strings.HasPrefix(candidate, needle+".") {
			within = append(within, candidate)
		}
		if editDistance(needle, candidate) <= max(2, len(needle)/4) {
			nearby = append(nearby, candidate)
		}
	}
	sortStringsByDistance(nearby, needle)
	if len(nearby) == 0 {
		nearby = within
	}
	return nearby[:min(3, len(nearby))]
}

// listenSigningSecret resolves the local signing secret: --webhook-secret,
// then FLINT_WEBHOOK_SECRET, then a secret saved on first use, so a handler
// configured with it keeps verifying signatures across restarts. When the
// secret cannot be saved, listening continues with a temporary one unless
// mustSave is set.
func (a *App) listenSigningSecret(opts Options, mustSave bool) (listenSecret, *CLIError) {
	if values := opts.Raw["webhook-secret"]; len(values) > 0 {
		value := strings.TrimSpace(values[len(values)-1])
		if err := validateListenSecret(value); err != nil {
			return listenSecret{}, usageError("INVALID_WEBHOOK_SECRET", "--webhook-secret "+err.Error(), "webhook_secret")
		}
		return listenSecret{value: value, source: "flag"}, nil
	}
	if value := strings.TrimSpace(os.Getenv(listenSecretEnv)); value != "" {
		if err := validateListenSecret(value); err != nil {
			return listenSecret{}, usageError("INVALID_WEBHOOK_SECRET", listenSecretEnv+" "+err.Error(), "webhook_secret")
		}
		return listenSecret{value: value, source: "environment"}, nil
	}
	value, created, err := a.loadOrCreateListenSecret()
	switch {
	case errors.Is(err, errListenSavedSecretInvalid):
		return listenSecret{}, configError("LISTEN_SECRET_INVALID", terminalSafe(err.Error())+". Delete the file to generate a new secret, or pass --webhook-secret.", err)
	case err != nil && mustSave:
		return listenSecret{}, configError("LISTEN_SECRET_SAVE_FAILED", "Could not save the webhook signing secret: "+terminalSafe(err.Error())+". Pass --webhook-secret or set "+listenSecretEnv+" instead.", err)
	case err != nil:
		temporary, genErr := webhooksigning.GenerateSecret()
		if genErr != nil {
			return listenSecret{}, networkError("WEBHOOK_SECRET_GENERATION_FAILED", "Could not generate the local webhook signing secret.", genErr)
		}
		warning := "Could not save the webhook signing secret (" + terminalSafe(err.Error()) + "), so it changes on the next run. Pass --webhook-secret or set " + listenSecretEnv + " to keep it stable."
		return listenSecret{value: temporary, source: "temporary", warning: warning}, nil
	case created:
		return listenSecret{value: value, source: "generated"}, nil
	}
	return listenSecret{value: value, source: "saved"}, nil
}

// validateListenSecret accepts the Standard Webhooks secrets Flint issues:
// whsec_ followed by base64 of 32 bytes. The value is never echoed.
func validateListenSecret(secret string) error {
	if secret == "" {
		return errors.New("must not be empty")
	}
	if _, err := webhooksigning.StandardWebhookSigningKey(secret); err != nil {
		return fmt.Errorf("is not a valid signing secret (%v). Use whsec_ followed by base64 of 32 bytes, such as the value flint listen --print-secret prints", err)
	}
	return nil
}

func (a *App) loadOrCreateListenSecret() (string, bool, error) {
	dir, err := a.configDirectory()
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(dir, listenSecretFile)
	secret, created := "", false
	// The lock keeps two first runs from saving different secrets.
	err = withLocalFileLock(a.commandContext(), path, func() error {
		raw, readErr := os.ReadFile(path)
		if readErr == nil {
			secret = strings.TrimSpace(string(raw))
			if keyErr := validateListenSecret(secret); keyErr != nil {
				return fmt.Errorf("%w: %s %v", errListenSavedSecretInvalid, path, keyErr)
			}
			return nil
		}
		if !errors.Is(readErr, fs.ErrNotExist) {
			return readErr
		}
		generated, genErr := webhooksigning.GenerateSecret()
		if genErr != nil {
			return genErr
		}
		if writeErr := writeListenSecretFile(path, generated); writeErr != nil {
			return writeErr
		}
		secret, created = generated, true
		return nil
	})
	return secret, created, err
}

func writeListenSecretFile(path, secret string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "listen-secret-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(secret + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (a *App) writeListenSecret(opts Options, secret listenSecret) (any, *CLIError) {
	var err error
	if opts.Output == "json" || opts.Output == "ndjson" {
		err = writeJSON(a.Stdout, map[string]any{"data": map[string]any{"signing_secret": secret.value, "source": secret.source}})
	} else {
		// A bare line lets scripts capture it: SECRET=$(flint listen --print-secret).
		_, err = fmt.Fprintln(a.Stdout, secret.value)
	}
	if err != nil {
		return nil, networkError("OUTPUT_WRITE_FAILED", "Could not write listener output.", err)
	}
	return outputHandled{}, nil
}

func (a *App) openListenStream(ctx context.Context, baseURL, key, path, cursor string, timeout time.Duration, debug bool) (*http.Response, bool, *CLIError) {
	if key != workloadCredential {
		resp, retry, e := a.openListenStreamAttempt(ctx, baseURL, key, path, cursor, timeout, debug)
		if strings.HasPrefix(key, "fwat_") {
			e = redactFederationError(e, key)
		}
		return resp, retry, e
	}
	token, e := a.workloadToken(ctx, baseURL, "")
	if e != nil {
		return nil, false, e
	}
	resp, retry, e := a.openListenStreamAttempt(ctx, baseURL, token, path, cursor, timeout, debug)
	if e != nil && e.HTTPStatus == http.StatusUnauthorized {
		token, e = a.workloadToken(ctx, baseURL, token)
		if e != nil {
			return nil, false, e
		}
		resp, retry, e = a.openListenStreamAttempt(ctx, baseURL, token, path, cursor, timeout, debug)
	}
	return resp, retry, redactFederationError(e, token)
}

func (a *App) openListenStreamAttempt(ctx context.Context, baseURL, key, path, cursor string, timeout time.Duration, debug bool) (*http.Response, bool, *CLIError) {
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
	currentKey, tokenErr := a.oauthRequestToken(ctx, key, baseURL)
	if tokenErr != nil {
		cancelRequest()
		return nil, false, tokenErr
	}
	key = currentKey
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
		return nil, errorRetryable(apiErr), apiErr
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
			// A slow or paused handler holds up reading, not the stream.
			if grace := state.deliveryGrace(opts.Timeout); grace > 0 {
				timer.Reset(grace)
				continue
			}
			stopConsumer()
			return networkError("STREAM_IDLE_TIMEOUT", fmt.Sprintf("The webhook event stream sent no data for %s.", opts.Timeout), context.DeadlineExceeded)
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
			if state.announced && opts.Output == "human" && !opts.Quiet {
				fmt.Fprintln(a.Stderr, "Reconnected to the webhook event stream.")
			}
			if writeErr := a.announceListenReady(opts, state); writeErr != nil {
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
			state.disconnect = disconnect.Reason
			if writeErr := a.writeListenRecord(opts, map[string]any{"type": "disconnect", "reason": disconnect.Reason, "cursor": state.cursor}, "Stream disconnected: "+terminalSafe(disconnect.Reason)+"."); writeErr != nil {
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
			if writeErr := a.announceListenReady(opts, state); writeErr != nil {
				return writeErr
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
	if writeErr := a.announceListenReady(opts, state); writeErr != nil {
		return writeErr
	}
	eventLabel := terminalSafe(event.EventType) + " " + terminalSafe(event.WebhookEventID)
	if len(event.Payload) == 0 || bytes.Equal(bytes.TrimSpace(event.Payload), []byte("null")) {
		// One event the key cannot read must not stop the listener.
		const message = "This API key can read webhook event metadata but not this event's resource payload. Use a key with read access to the event's resource type."
		state.cursor = event.WebhookEventID
		state.processed++
		state.withheld++
		record := map[string]any{"type": "withheld", "webhook_event_id": event.WebhookEventID, "event_type": event.EventType, "reason": "payload_not_visible", "message": message}
		return a.writeListenRecord(opts, record, "Withheld "+eventLabel+": "+message)
	}
	if !json.Valid(event.Payload) {
		return invalidResponseError("INVALID_STREAM_PAYLOAD", "The webhook stream payload is not valid JSON.", nil)
	}
	if forwardTo == "" {
		state.processed++
		state.events++
		state.cursor = event.WebhookEventID
		record := map[string]any{"type": "event", "webhook_event_id": event.WebhookEventID, "event_type": event.EventType, "payload": event.Payload}
		return a.writeListenRecord(opts, record, eventLabel+" "+listenPayloadSummary(event.Payload))
	}
	cursorBefore := state.cursor
	state.forwarding.Store(true)
	result := a.forwardListenPayload(ctx, opts.Timeout, forwardTo, secret, event)
	state.forwardEnded.Store(time.Now().UnixNano())
	state.forwarding.Store(false)
	if result.err != nil && ctx.Err() != nil {
		// The listener stopped mid-delivery. Keep the cursor before this event
		// so a resumed run delivers it again.
		return nil
	}
	state.processed++
	state.events++
	state.cursor = event.WebhookEventID
	delivered := result.delivered()
	if delivered {
		state.forwarded++
	} else {
		state.failed++
		if state.failed == 1 {
			state.replayCursor, state.firstFailed = cursorBefore, event.WebhookEventID
		}
	}
	latency := result.latency.Round(time.Millisecond)
	record := map[string]any{
		"type":             "forward",
		"webhook_event_id": event.WebhookEventID,
		"event_type":       event.EventType,
		"ok":               delivered,
		"status_code":      result.statusCode,
		"latency_ms":       result.latency.Milliseconds(),
	}
	human := fmt.Sprintf("%s -> HTTP %d (%s)", eventLabel, result.statusCode, latency)
	switch {
	case result.err != nil:
		reason := listenForwardErrorReason(result.err, opts.Timeout)
		record["error"] = reason
		human = fmt.Sprintf("%s -> delivery failed (%s): %s", eventLabel, latency, reason)
	case !delivered:
		human += ", delivery failed"
		if result.body != "" {
			record["response_body"] = result.body
			human += ": " + result.body
		}
		if result.bodyTruncated {
			record["response_body_truncated"] = true
			human += "..."
		}
	}
	return a.writeListenRecord(opts, record, human)
}

func listenForwardErrorReason(err error, timeout time.Duration) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return fmt.Sprintf("the handler did not respond within %s (--timeout)", timeout)
	}
	return transportErrorReason(err)
}

// listenPayloadSummary is a short, terminal-safe view of an event's data for
// human output. JSON output carries the whole payload.
func listenPayloadSummary(payload json.RawMessage) string {
	body := []byte(payload)
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(payload, &envelope) == nil && len(envelope.Data) > 0 && !bytes.Equal(envelope.Data, []byte("null")) {
		body = envelope.Data
	}
	var compact bytes.Buffer
	if json.Compact(&compact, body) != nil {
		return ""
	}
	text := terminalSafe(compact.String())
	if utf8.RuneCountInString(text) > listenPayloadSummaryRunes {
		text = string([]rune(text)[:listenPayloadSummaryRunes]) + "..."
	}
	return text
}

type listenForwardResult struct {
	statusCode    int
	latency       time.Duration
	err           error
	body          string
	bodyTruncated bool
}

func (r listenForwardResult) delivered() bool {
	return r.err == nil && r.statusCode >= 200 && r.statusCode < 300
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
	req.Header.Set("webhook-id", event.WebhookEventID)
	req.Header.Set("webhook-timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("webhook-signature", webhooksigning.StandardWebhookSignatureHeader(event.WebhookEventID, timestamp, event.Payload, secret))

	client := a.ForwardHTTPClient
	if client == nil {
		client = newLocalForwardHTTPClient(timeout)
		// This transport pins a local destination for one delivery. Close
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
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, listenResponseExcerptBytes+1))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	result := listenForwardResult{statusCode: resp.StatusCode, latency: latency}
	if !result.delivered() {
		result.body, result.bodyTruncated = listenResponseExcerpt(excerpt)
	}
	return result
}

// listenResponseExcerpt keeps the start of a handler's error response on one
// terminal-safe line so failures show what the handler said.
func listenResponseExcerpt(raw []byte) (string, bool) {
	truncated := len(raw) > listenResponseExcerptBytes
	if truncated {
		raw = raw[:listenResponseExcerptBytes]
		// Drop a rune the cut split rather than render a replacement for it.
		for i := 0; i < utf8.UTFMax-1 && len(raw) > 0 && !utf8.Valid(raw); i++ {
			raw = raw[:len(raw)-1]
		}
	}
	text := strings.Join(strings.Fields(strings.ToValidUTF8(string(raw), "\uFFFD")), " ")
	return terminalSafe(text), truncated
}

func validateLocalForwardURL(value string) (*url.URL, error) {
	const allowed = "localhost, a loopback IP address such as 127.0.0.1, or " + listenDockerHost
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return nil, fmt.Errorf("--forward-to must be an absolute HTTP or HTTPS URL on %s, such as http://localhost:8080/webhooks", allowed)
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("--forward-to must not contain credentials or a fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("--forward-to must use HTTP or HTTPS")
	}
	hostname := strings.ToLower(parsed.Hostname())
	ip := net.ParseIP(hostname)
	if hostname != "localhost" && hostname != listenDockerHost && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("--forward-to must use %s. Webhook payloads contain customer data, so flint listen does not forward them to other hosts", allowed)
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
			if strings.EqualFold(host, listenDockerHost) {
				return dialDockerHostAddresses(ctx, network, port, addresses, dialer.DialContext)
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
	return dialFirstAddress(ctx, network, port, addresses, dial)
}

// dialDockerHostAddresses reaches the Docker Desktop host from a container,
// where host.docker.internal resolves to a private gateway address. A public
// address would send customer data off the machine, so it is refused.
func dialDockerHostAddresses(ctx context.Context, network, port string, addresses []netip.Addr, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	for _, address := range addresses {
		if address = address.Unmap(); !address.IsLoopback() && !address.IsPrivate() {
			return nil, fmt.Errorf("%s resolved to %s, which is not a private or loopback address", listenDockerHost, address)
		}
	}
	return dialFirstAddress(ctx, network, port, addresses, dial)
}

func dialFirstAddress(ctx context.Context, network, port string, addresses []netip.Addr, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
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
	return nil, fmt.Errorf("could not connect to any resolved local address: %w", lastErr)
}

func rejectLocalForwardRedirect(*http.Request, []*http.Request) error {
	// A redirect could send the signed payload to a different origin, so
	// local forwarding always reports the redirect itself.
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

func (a *App) writeListenStarted(opts Options, forwardTo string, secret listenSecret) *CLIError {
	if opts.Output == "json" || opts.Output == "ndjson" {
		record := map[string]any{"type": "listener", "mode": "print"}
		if forwardTo != "" {
			record = map[string]any{"type": "listener", "mode": "forward", "forward_to": forwardTo, "signing_secret": secret.value, "signing_secret_source": secret.source}
		}
		if eventType := lastRawOption(opts, "event-type"); eventType != "" {
			record["event_type"] = eventType
		}
		if secret.warning != "" {
			record["warning"] = secret.warning
		}
		return a.writeListenRecord(opts, record, "")
	}
	// Human output waits for the stream's ready record, so it never reports
	// a listener that has not connected.
	if secret.warning != "" && !opts.Quiet {
		fmt.Fprintln(a.Stderr, "warning: "+secret.warning)
	}
	return nil
}

// listenBanner is the human ready text: what is being received, where it goes,
// how the run stops, and the secret a handler must verify signatures with.
func listenBanner(opts Options, forwardTo string, secret listenSecret) string {
	events := "webhook events"
	if eventType := lastRawOption(opts, "event-type"); eventType != "" {
		events = eventType + " events"
	}
	var banner strings.Builder
	if forwardTo == "" {
		fmt.Fprintf(&banner, "Ready. Printing %s; pass --forward-to URL to deliver them to a local handler.", events)
	} else {
		fmt.Fprintf(&banner, "Ready. Forwarding %s to %s.", events, forwardTo)
	}
	var bounds []string
	if opts.MaxEvents == 1 {
		bounds = append(bounds, "1 event")
	} else if opts.MaxEvents > 1 {
		bounds = append(bounds, fmt.Sprintf("%d events", opts.MaxEvents))
	}
	if opts.For > 0 {
		bounds = append(bounds, opts.For.String())
	}
	if len(bounds) == 0 {
		banner.WriteString(" Press Ctrl-C to stop.")
	} else {
		banner.WriteString(" Stops after " + strings.Join(bounds, " or ") + "; press Ctrl-C to stop sooner.")
	}
	if forwardTo != "" {
		source := map[string]string{
			"flag":        "from --webhook-secret",
			"environment": "from " + listenSecretEnv,
			"saved":       "saved; reused across runs",
			"generated":   "new; saved and reused by later runs",
			"temporary":   "temporary; could not be saved",
		}[secret.source]
		fmt.Fprintf(&banner, "\nWebhook signing secret: %s (%s)", secret.value, source)
	}
	return banner.String()
}

// announceListenReady prints the human banner once. Flint sends a ready record
// first, but an event without one still shows the stream is open.
func (a *App) announceListenReady(opts Options, state *listenStreamState) *CLIError {
	if state.announced || state.banner == "" || opts.Output == "json" || opts.Output == "ndjson" {
		return nil
	}
	state.announced = true
	if _, err := fmt.Fprintln(a.Stdout, state.banner); err != nil {
		return networkError("OUTPUT_WRITE_FAILED", "Could not write listener output.", err)
	}
	return nil
}

func (a *App) writeListenReconnect(opts Options, state *listenStreamState, cause *CLIError, attempt int, delay time.Duration) *CLIError {
	message := listenFailureMessage(cause)
	if opts.Output == "json" || opts.Output == "ndjson" {
		record := map[string]any{"type": "reconnect", "reason": cause.Code, "message": message, "attempt": attempt, "retry_in_ms": delay.Milliseconds(), "cursor": state.cursor}
		if cause.HTTPStatus != 0 {
			record["http_status"] = cause.HTTPStatus
		}
		return a.writeListenRecord(opts, record, "")
	}
	if !opts.Quiet {
		fmt.Fprintf(a.Stderr, "%s Reconnecting in %s (attempt %d).\n", message, delay, attempt)
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
	record := map[string]any{"type": "checkpoint", "cursor": state.cursor, "processed": state.processed, "events": state.events, "forwarded": state.forwarded, "failed": state.failed, "withheld": state.withheld}
	if state.failed > 0 && state.replayCursor != "" {
		record["replay_cursor"] = state.replayCursor
	}
	if opts.Output == "json" || opts.Output == "ndjson" {
		return a.writeListenRecord(opts, record, "")
	}
	if opts.Quiet {
		return nil
	}
	forwardTo := lastRawOption(opts, "forward-to")
	human := fmt.Sprintf("Checkpoint: cursor=%s processed=%d events=%d withheld=%d", state.cursor, state.processed, state.events, state.withheld)
	if forwardTo != "" {
		human = fmt.Sprintf("Checkpoint: cursor=%s processed=%d forwarded=%d failed=%d withheld=%d", state.cursor, state.processed, state.forwarded, state.failed, state.withheld)
	}
	if state.failed > 0 {
		noun := "deliveries"
		if state.failed == 1 {
			noun = "delivery"
		}
		hint := listenReplayHint(state)
		if state.replayCursor != "" {
			replay := "flint listen --forward-to " + forwardTo + " --cursor " + state.replayCursor
			if eventType := lastRawOption(opts, "event-type"); eventType != "" {
				replay += " --event-type " + eventType
			}
			hint = "After fixing the handler, replay from the first failure with: " + replay
		}
		human += fmt.Sprintf("\n%d %s failed. %s", state.failed, noun, hint)
	}
	return a.writeListenRecord(opts, record, human)
}
