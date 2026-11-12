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
	// The POST has no business side effects, but each call consumes quota and
	// creates fresh bearer material. Retry only explicit throttling responses.
	for attempt := 1; attempt <= 3; attempt++ {
		resp, e := a.doRequestWithin(ctx, timeout, source.baseURL, source.key, http.MethodPost, path, body, "", false)
		if e != nil {
			if e.HTTPStatus == http.StatusTooManyRequests && attempt < 3 {
				delay := time.Duration(attempt) * time.Second
				if e.RetryAfterMs != nil {
					delay = max(delay, time.Duration(*e.RetryAfterMs)*time.Millisecond)
				}
				if sleepContext(ctx, delay) {
					continue
				}
			}
			// Keep only known-safe metadata. Neither an API error nor a local receiver
			// response is allowed to reflect a signed frame into CLI output.
			safe := cliError(e.ExitCode, e.Type, e.Code, "Could not sign the webhook delivery for this sandbox endpoint.")
			safe.HTTPStatus, safe.RequestID = e.HTTPStatus, e.RequestID
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
		if !envelope.Data.ExpiresAt.After(a.Now()) {
			continue
		}
		return envelope.Data, nil
	}
	return listenSignedDelivery{}, invalidResponseError("SIGNED_DELIVERY_EXPIRED", "The signed delivery expired before it could be forwarded. Retry listening.", nil)
}
