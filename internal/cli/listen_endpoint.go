package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const listenMarkerURL = "https://listen.flint-cli.invalid/webhooks"
const listenEndpointDescription = "Created by flint listen to forward sandbox events to localhost. Safe to delete."

type listenEndpoint struct {
	ID             string    `json:"webhook_endpoint_id"`
	URL            string    `json:"url"`
	CreatedAt      time.Time `json:"created_at"`
	Version        int64     `json:"version"`
	Status         string    `json:"status"`
	SigningMethods []string  `json:"signing_methods"`
	EnabledEvents  []string  `json:"enabled_events"`
}

func listenTestModeRequired() *CLIError {
	return usageError("TEST_MODE_REQUIRED", "flint listen --forward-to runs only with a sandbox credential. Flint signs forwarded deliveries for sandbox endpoints only. Omit --forward-to to print live events.", "live")
}
func listenForwarding(cmd *Command, opts Options) bool {
	return cmd != nil && cmd.CanonicalName == "listen" && len(opts.Raw["forward-to"]) > 0
}
func listenEndpointUsability(endpoint listenEndpoint, cursor string) *CLIError {
	reason, message := "", ""
	switch {
	case endpoint.Status != "active":
		reason, message = "inactive", fmt.Sprintf("Endpoint %s is inactive. Activate it with flint webhook-endpoints update %s --status active.", endpoint.ID, endpoint.ID)
	case !slices.Contains(endpoint.SigningMethods, "jwt"):
		reason, message = "jwt_signing_not_enabled", fmt.Sprintf("Endpoint %s doesn't sign with JWT. Add it with flint webhook-endpoints update %s --add-signing-methods jwt.", endpoint.ID, endpoint.ID)
	default:
		return nil
	}
	return listenEndpointError(endpoint.ID, reason, message, cursor)
}
func listenEndpointError(id, reason, message, cursor string) *CLIError {
	if cursor != "" {
		message += " Resume with --cursor " + cursor + "."
	}
	e := cliError(ExitAPI, errorTypeAPI, "WEBHOOK_ENDPOINT_NOT_USABLE", message)
	e.Param = "webhook_endpoint_id"
	e.Details = map[string]any{"reason": reason, "webhook_endpoint_id": id}
	return e
}
func (a *App) getListenEndpoint(ctx context.Context, opts Options, source listenDeliverySource, cursor string) (listenEndpoint, *CLIError) {
	resp, e := a.doRequestWithin(ctx, opts.Timeout, source.baseURL, source.key, http.MethodGet, "/v1/webhook-endpoints/"+url.PathEscape(source.endpointID), nil, "", false)
	if e != nil {
		if e.HTTPStatus == 404 {
			return listenEndpoint{}, listenEndpointError(source.endpointID, "not_found", fmt.Sprintf("Endpoint %s wasn't found in this merchant and sandbox.", source.endpointID), cursor)
		}
		return listenEndpoint{}, e
	}
	var envelope struct {
		Data listenEndpoint `json:"data"`
	}
	if json.Unmarshal(resp.Raw, &envelope) != nil || envelope.Data.ID != source.endpointID {
		return listenEndpoint{}, invalidResponseError("INVALID_RESPONSE", "Flint returned an invalid webhook endpoint.", nil)
	}
	return envelope.Data, nil
}
func listenPreparationError(e *CLIError) *CLIError {
	e.Message = "Could not prepare the CLI listener endpoint: " + e.Message + " Pass --webhook-endpoint-id to use an existing sandbox endpoint."
	return e
}
func (a *App) resolveListenEndpoint(ctx context.Context, opts Options, source listenDeliverySource) (listenEndpoint, string, *CLIError) {
	if source.endpointID != "" {
		endpoint, e := a.getListenEndpoint(ctx, opts, source, "")
		if e != nil {
			return endpoint, "flag", e
		}
		return endpoint, "flag", listenEndpointUsability(endpoint, "")
	}
	endpoint, origin, e := a.prepareListenEndpoint(ctx, opts, source)
	if e != nil {
		return endpoint, origin, listenPreparationError(e)
	}
	return endpoint, origin, nil
}
func (a *App) prepareListenEndpoint(ctx context.Context, opts Options, source listenDeliverySource) (listenEndpoint, string, *CLIError) {
	var oldest *listenEndpoint
	token := ""
	for {
		query := url.Values{"page_size": {"100"}}
		if token != "" {
			query.Set("page_token", token)
		}
		resp, e := a.doRequestWithin(ctx, opts.Timeout, source.baseURL, source.key, http.MethodGet, "/v1/webhook-endpoints?"+query.Encode(), nil, "", false)
		if e != nil {
			return listenEndpoint{}, "", e
		}
		var page struct {
			Data []listenEndpoint `json:"data"`
			Meta struct {
				NextPageToken string `json:"next_page_token"`
			} `json:"meta"`
		}
		if json.Unmarshal(resp.Raw, &page) != nil {
			return listenEndpoint{}, "", invalidResponseError("INVALID_RESPONSE", "Flint returned an invalid webhook endpoint list.", nil)
		}
		for _, candidate := range page.Data {
			if candidate.URL == listenMarkerURL && (oldest == nil || candidate.CreatedAt.Before(oldest.CreatedAt) || candidate.CreatedAt.Equal(oldest.CreatedAt) && candidate.ID < oldest.ID) {
				copy := candidate
				oldest = &copy
			}
		}
		token = page.Meta.NextPageToken
		if token == "" {
			break
		}
	}
	if oldest == nil {
		body, _ := json.Marshal(map[string]any{"url": listenMarkerURL, "enabled_events": []string{"*"}, "signing_methods": []string{"jwt"}, "status": "active", "description": listenEndpointDescription})
		key, err := newIdempotencyKey()
		if err != nil {
			return listenEndpoint{}, "", networkError("IDEMPOTENCY_KEY_FAILED", "Could not generate a listener endpoint idempotency key.", nil)
		}
		resp, e := a.doRequestWithin(ctx, opts.Timeout, source.baseURL, source.key, http.MethodPost, "/v1/webhook-endpoints", body, key, false)
		if e != nil {
			return listenEndpoint{}, "", e
		}
		var envelope struct {
			Data listenEndpoint `json:"data"`
		}
		if json.Unmarshal(resp.Raw, &envelope) != nil || envelope.Data.ID == "" {
			return listenEndpoint{}, "", invalidResponseError("INVALID_RESPONSE", "Flint returned an invalid webhook endpoint.", nil)
		}
		return envelope.Data, "created", listenEndpointUsability(envelope.Data, "")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if oldest.Status == "active" && slices.Contains(oldest.SigningMethods, "jwt") && slices.Equal(oldest.EnabledEvents, []string{"*"}) {
			return *oldest, "reused", nil
		}
		methods := slices.Clone(oldest.SigningMethods)
		if !slices.Contains(methods, "jwt") {
			methods = append(methods, "jwt")
		}
		body, _ := json.Marshal(map[string]any{"expected_version": oldest.Version, "status": "active", "enabled_events": []string{"*"}, "signing_methods": methods})
		resp, e := a.doRequestWithin(ctx, opts.Timeout, source.baseURL, source.key, http.MethodPatch, "/v1/webhook-endpoints/"+url.PathEscape(oldest.ID), body, "", false)
		if e != nil {
			if attempt == 0 && (e.Code == "VERSION_CONFLICT" || e.Code == "CONCURRENT_MODIFICATION") {
				source.endpointID = oldest.ID
				refreshed, readErr := a.getListenEndpoint(ctx, opts, source, "")
				if readErr != nil {
					return *oldest, "", readErr
				}
				oldest = &refreshed
				continue
			}
			return *oldest, "", e
		}
		var envelope struct {
			Data listenEndpoint `json:"data"`
		}
		if json.Unmarshal(resp.Raw, &envelope) != nil || envelope.Data.ID != oldest.ID {
			return *oldest, "", invalidResponseError("INVALID_RESPONSE", "Flint returned an invalid webhook endpoint.", nil)
		}
		return envelope.Data, "reused", listenEndpointUsability(envelope.Data, "")
	}
	panic("unreachable listener repair")
}
func listenEffectiveFilter(endpoint listenEndpoint, selected []string) ([]string, *CLIError) {
	if slices.Contains(endpoint.EnabledEvents, "*") {
		return selected, nil
	}
	if len(selected) == 0 {
		return slices.Clone(endpoint.EnabledEvents), nil
	}
	missing := []string{}
	for _, typ := range selected {
		if !slices.Contains(endpoint.EnabledEvents, typ) {
			missing = append(missing, typ)
		}
	}
	if len(missing) > 0 {
		e := usageError("EVENT_TYPE_NOT_ENABLED", fmt.Sprintf("Endpoint %s doesn't subscribe to %s, so it can't be forwarded. Add it with flint webhook-endpoints update %s --add-enabled-events %s, or omit --webhook-endpoint-id to use the CLI's listener endpoint, which receives every event type.", endpoint.ID, strings.Join(missing, ","), endpoint.ID, strings.Join(missing, ",")), "event_type")
		e.Details = map[string]any{"not_enabled": missing, "enabled_events": endpoint.EnabledEvents}
		return nil, e
	}
	return selected, nil
}
func (source listenDeliverySource) allows(eventType string) bool {
	return source.endpoint == nil || (len(source.selected) == 0 || slices.Contains(source.selected, eventType)) && (slices.Contains(source.endpoint.EnabledEvents, "*") || slices.Contains(source.endpoint.EnabledEvents, eventType))
}
