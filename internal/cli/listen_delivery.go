package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// listenSignedDelivery is deliberately never included in output or history.
type listenSignedDelivery struct {
	EndpointID string            `json:"webhook_endpoint_id"`
	EventID    string            `json:"webhook_event_id"`
	Body       string            `json:"body"`
	Headers    map[string]string `json:"headers"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

func (a *App) signListenDelivery(ctx context.Context, timeout time.Duration, source listenDeliverySource, eventID string) (listenSignedDelivery, *CLIError) {
	body, _ := json.Marshal(map[string]string{"webhook_event_id": eventID})
	path := "/v1/webhook-endpoints/" + url.PathEscape(source.endpointID) + "/sign-delivery"
	// Signing does not dispatch a webhook. Stay on the same event until
	// signing succeeds, a permanent error arrives, or listening ends.
	var retries, configurationRaces, expiredFrames int
	for {
		resp, e := a.doRequestWithin(ctx, timeout, source.baseURL, source.key, http.MethodPost, path, body, "", false)
		if e != nil {
			retry := listenSigningRetryable(e)
			if e.HTTPStatus == http.StatusConflict && e.Code == "CONCURRENT_MODIFICATION" {
				configurationRaces++
				retry = configurationRaces < 3
			} else {
				configurationRaces = 0
			}
			if retry && ctx.Err() == nil {
				retries++
				delay := listenReconnectDelay(retries)
				if e.RetryAfterMs != nil && *e.RetryAfterMs > 0 {
					hint := time.Duration(*e.RetryAfterMs) * time.Millisecond
					if e.HTTPStatus == http.StatusTooManyRequests {
						delay = hint
					} else {
						delay = max(delay, hint)
					}
				}
				if sleepContext(ctx, delay) {
					continue
				}
			}
			// Keep only known-safe metadata. Neither an API error nor a local receiver
			// response is allowed to reflect a signed frame into CLI output.
			safe := cliError(e.ExitCode, e.Type, e.Code, "Could not sign the webhook delivery for this sandbox endpoint.")
			safe.HTTPStatus, safe.RequestID, safe.Param = e.HTTPStatus, e.RequestID, e.Param
			safe.retryable = &retry
			safe.Details = map[string]any{"reason": listenSigningReason(e)}
			if e.HTTPStatus == http.StatusConflict && e.Code == "LIMIT_REACHED" {
				reset := "unknown (see the endpoint's daily quota)"
				if details, ok := e.Details.(map[string]any); ok {
					if items, ok := details["details"].([]any); ok {
						for _, item := range items {
							detail, _ := item.(map[string]any)
							if value, ok := detail["resets_at"].(string); ok {
								if instant, err := time.Parse(time.RFC3339, value); err == nil {
									reset = instant.UTC().Format(time.RFC3339)
								}
							}
						}
					}
				}
				safe.Message = fmt.Sprintf("The endpoint's daily signed-delivery quota is exhausted. resets_at: %s. Resume with the checkpoint cursor after the quota resets.", reset)
			}
			return listenSignedDelivery{}, safe
		}
		var envelope struct {
			Data listenSignedDelivery `json:"data"`
		}
		if json.Unmarshal(resp.Raw, &envelope) != nil || envelope.Data.EndpointID != source.endpointID || envelope.Data.EventID != eventID || envelope.Data.Body == "" || envelope.Data.Headers["Authorization"] == "" || envelope.Data.ExpiresAt.IsZero() {
			return listenSignedDelivery{}, invalidResponseError("INVALID_SIGNED_DELIVERY", "Flint returned an invalid signed delivery.", nil)
		}
		// Re-mint expired frames, including frames delayed by a proxy. Each local
		// delivery retry also mints afresh, so it never reuses an expired JWT.
		if envelope.Data.ExpiresAt.After(a.Now()) {
			return envelope.Data, nil
		}
		retries, configurationRaces = 0, 0
		expiredFrames++
		if expiredFrames == 3 {
			return listenSignedDelivery{}, invalidResponseError("SIGNED_DELIVERY_EXPIRED", "The signed delivery expired before it could be forwarded. Retry listening.", nil)
		}
	}
}

func listenSigningRetryable(e *CLIError) bool {
	if e.Code == "LIMIT_REACHED" || e.Code == "CLIENT_CANCELED" {
		return false
	}
	// Older error envelopes report retryability directly. Unstructured
	// gateway failures can also arrive without a remediation field.
	retryable, _ := e.apiError["retryable"].(bool)
	return retryable || errorRetryable(e) || retryableErrorStatus(e.HTTPStatus)
}

// Only published reason identifiers cross the signed-response boundary.
func listenSigningReason(e *CLIError) string {
	raw, _ := e.Details.(map[string]any)
	reason, _ := raw["reason"].(string)
	if reason == "" {
		if items, ok := raw["details"].([]any); ok {
			for _, item := range items {
				d, _ := item.(map[string]any)
				if value, ok := d["reason"].(string); ok {
					reason = value
					break
				}
			}
		}
	}
	switch reason {
	case "configuration_required", "payload_expired", "jwt_signing_not_enabled", "unsupported_state":
		return reason
	}
	return ""
}
func (a *App) handleListenSigningFailure(ctx context.Context, opts Options, source listenDeliverySource, state *listenStreamState, event listenWebhookEvent, e *CLIError) *CLIError {
	reason, apiReason := "", ""
	switch {
	case e.HTTPStatus == 409 && e.Code == "ACTION_NOT_ALLOWED":
		endpoint, readErr := a.getListenEndpoint(ctx, opts, source, state.cursor)
		if readErr != nil {
			return readErr
		}
		if err := listenEndpointUsability(endpoint, state.cursor); err != nil {
			return err
		}
		if source.endpoint != nil {
			*source.endpoint = endpoint
		}
		apiReason = listenSigningReason(e)
		reason = "not_allowed"
		switch apiReason {
		case "configuration_required":
			reason = "not_subscribed"
		case "payload_expired":
			reason = "payload_expired"
		}
	case e.HTTPStatus == 404 && e.Code == "RESOURCE_NOT_FOUND" && e.Param == "webhook_event_id":
		reason = "event_not_found"
	case e.HTTPStatus == 403 && e.Code == "FORBIDDEN":
		state.cursor = event.WebhookEventID
		state.processed++
		state.withheld++
		return a.writeListenRecord(opts, map[string]any{"type": "withheld", "webhook_event_id": event.WebhookEventID, "event_type": event.EventType, "reason": "payload_not_visible"}, "Withheld "+terminalSafe(event.EventType)+" "+terminalSafe(event.WebhookEventID)+": this credential cannot read the event's resource.")
	default:
		return e
	}
	state.cursor = event.WebhookEventID
	state.skipped++
	record := map[string]any{"type": "skipped", "webhook_event_id": event.WebhookEventID, "event_type": event.EventType, "webhook_endpoint_id": source.endpointID, "reason": reason}
	if apiReason != "" {
		record["api_reason"] = apiReason
	}
	message := "delivery signing is not allowed."
	switch reason {
	case "not_subscribed":
		message = source.endpointID + " doesn't subscribe to this event type."
	case "payload_expired":
		message = "the event payload has expired."
	case "event_not_found":
		message = "the event wasn't found."
	}
	return a.writeListenRecord(opts, record, "Skipped "+terminalSafe(event.EventType)+" "+terminalSafe(event.WebhookEventID)+": "+message)
}
