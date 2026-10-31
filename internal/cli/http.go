package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

type AuthContext struct {
	SelectedContext    bool     `json:"-"`
	OAuthSessionID     string   `json:"oauth_session_id,omitempty"`
	ContextID          string   `json:"context_id,omitempty"`
	CredentialScope    string   `json:"-"`
	AuthType           string   `json:"auth_type"`
	WorkloadIdentityID string   `json:"workload_identity_id,omitempty"`
	OAuthGrantID       string   `json:"oauth_grant_id,omitempty"`
	APIKeyID           string   `json:"api_key_id"`
	Name               string   `json:"name,omitempty"`
	Environment        string   `json:"environment"`
	Mode               string   `json:"mode,omitempty"`
	Access             string   `json:"access,omitempty"`
	MerchantID         string   `json:"merchant_id"`
	OrganizationID     string   `json:"organization_id,omitempty"`
	SandboxID          string   `json:"sandbox_id,omitempty"`
	Scopes             []string `json:"scopes"`
	ExpiresAt          *string  `json:"expires_at"`
}

// New servers report mode. Keep the effective environment for existing guards
// and accept older servers and saved sessions that only carry environment.
func (auth *AuthContext) UnmarshalJSON(data []byte) error {
	type fields AuthContext
	var value fields
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*auth = AuthContext(value)
	if auth.Mode != "" {
		auth.Environment = normalizeEnvironment(auth.Mode)
	}
	return nil
}

func (auth AuthContext) isCLILogin() bool {
	return auth.AuthType == "oauth" || auth.AuthType == "cli_login"
}

type authContextEnvelope struct {
	Data      AuthContext    `json:"data"`
	RequestID string         `json:"request_id"`
	Meta      map[string]any `json:"meta,omitempty"`
	fromCache bool
}

type apiResponse struct {
	Status  int
	Headers http.Header
	Raw     []byte
	Value   any
}

const defaultAPIBaseURL = "https://api.withflintpay.com"

// App copies (including concurrent MCP tools) share the connection pool. Idle
// connections expire even in a long-running process with no further requests.
var defaultAPIHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
	},
	CheckRedirect: rejectAPIRedirect,
}

func (a *App) baseURLForCredential(key string) (string, error) {
	if _, err := credentialEnvironment(key); err != nil {
		return "", err
	}
	if a.BaseURL != "" {
		return validateBaseURL(a.BaseURL)
	}
	if value := strings.TrimSpace(os.Getenv("FLINT_BASE_URL")); value != "" {
		return validateBaseURL(value)
	}
	return defaultAPIBaseURL, nil
}

func validateBaseURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	u, err := url.Parse(value)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return "", errors.New("FLINT_BASE_URL must be an absolute HTTP or HTTPS URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("FLINT_BASE_URL must not contain credentials, a query, or a fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		hostname := strings.ToLower(u.Hostname())
		ip := net.ParseIP(hostname)
		if hostname != "localhost" && !strings.HasSuffix(hostname, ".localhost") && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("FLINT_BASE_URL may use HTTP only for a loopback host")
		}
	default:
		return "", errors.New("FLINT_BASE_URL must use HTTP or HTTPS")
	}
	return strings.TrimSuffix(strings.TrimRight(value, "/"), "/v1"), nil
}

func (a *App) fetchAuthContext(ctx context.Context, baseURL, key string, debug bool) (AuthContext, *CLIError) {
	envelope, e := a.fetchAuthContextResponse(ctx, baseURL, key, debug)
	if e != nil {
		return AuthContext{}, e
	}
	return envelope.Data, nil
}

func (a *App) fetchAuthContextResponse(ctx context.Context, baseURL, key string, debug bool) (authContextEnvelope, *CLIError) {
	if envelope, ok := a.cachedAuthContext(baseURL, key); ok {
		a.observeCachedAuthContext(envelope.Data)
		return envelope, nil
	}
	return a.fetchFreshAuthContextResponse(ctx, baseURL, key, debug)
}

func (a *App) fetchFreshAuthContextResponse(ctx context.Context, baseURL, key string, debug bool) (authContextEnvelope, *CLIError) {
	resp, e := a.doRequest(ctx, baseURL, key, http.MethodGet, "/v1/auth-context", nil, "", debug)
	if e != nil {
		return authContextEnvelope{}, e
	}
	b, err := json.Marshal(resp.Value)
	if err != nil {
		return authContextEnvelope{}, invalidResponseError("INVALID_RESPONSE", "Flint returned an invalid auth context response.", err)
	}
	var envelope authContextEnvelope
	if err := json.Unmarshal(b, &envelope); err != nil || envelope.Data.MerchantID == "" || envelope.Data.Environment == "" {
		return authContextEnvelope{}, invalidResponseError("INVALID_RESPONSE", "Flint returned an invalid auth context response.", err)
	}
	a.cacheAuthContext(baseURL, key, envelope)
	return envelope, nil
}

func (a *App) fetchCurrentAPIVersion(ctx context.Context, baseURL string, debug bool) (string, *CLIError) {
	baseURL, err := validateBaseURL(baseURL)
	if err != nil {
		return "", usageError("INVALID_BASE_URL", err.Error(), "FLINT_BASE_URL")
	}
	response, requestError := a.doAPIRequest(ctx, baseURL, "", http.MethodGet, "/v1/openapi.json", nil, "", debug, true)
	if requestError != nil {
		return "", requestError
	}
	version := response.Headers.Get("Flint-Version")
	if _, err := time.Parse(time.DateOnly, version); err != nil {
		e := invalidResponseError("INVALID_RESPONSE", "The OpenAPI response did not include a valid Flint-Version header.", err)
		e.RequestID = response.Headers.Get("X-Request-Id")
		return "", e
	}
	return version, nil
}

func (a *App) doRequest(ctx context.Context, baseURL, key, method, path string, body []byte, idempotencyKey string, debug bool) (*apiResponse, *CLIError) {
	return a.doAPIRequest(ctx, baseURL, key, method, path, body, idempotencyKey, debug, false)
}

// versionProbe omits the version pin and reads only response headers. Doctor can
// discover the server's default version without downloading the OpenAPI document.
func (a *App) doAPIRequest(ctx context.Context, baseURL, key, method, path string, body []byte, idempotencyKey string, debug, versionProbe bool) (result *apiResponse, requestErr *CLIError) {
	responseStatus := 0
	history := requestHistory{started: time.Now()}
	replaySafe := method == http.MethodGet || idempotencyKey != "" && !unknownRawOperation(responseContract(ctx))
	responseLimit := 16 << 20
	if method == http.MethodGet && strings.SplitN(path, "?", 2)[0] == "/v1/openapi.json" {
		responseLimit = 64 << 20
	}
	defer func() {
		if requestErr != nil {
			if requestErr.RequestID == "" && len(history.requestIDs) > 0 {
				requestErr.RequestID = history.requestIDs[len(history.requestIDs)-1]
			}
			if requestErr.HTTPStatus == 0 {
				requestErr.HTTPStatus = responseStatus
			}
			retryable := replaySafe && errorRetryable(requestErr) && !errors.Is(ctx.Err(), context.Canceled) && !errors.Is(requestErr.Cause, context.Canceled)
			requestErr.retryable = &retryable
			requestErr = history.finalError(ctx, requestErr, method, path, idempotencyKey)
			requestErr.requestSent = history.sent
			if !unknownRawOperation(responseContract(ctx)) {
				requestErr = withIdempotencyRetryContext(requestErr, idempotencyKey)
			}
		}
	}()
	if !strings.HasPrefix(path, "/v1/") && path != "/v1" {
		return nil, usageError("NON_PUBLIC_API_PATH", "Only documented /v1 public API paths are allowed.", "path")
	}
	client := a.HTTPClient
	if client == nil {
		client = defaultAPIHTTPClient
	} else if versionProbe || client.CheckRedirect == nil {
		clone := *client
		clone.CheckRedirect = rejectAPIRedirect
		client = &clone
	}
	attempt := 0
	credential := key
	federationRetried := false
	for {
		responseStatus = 0
		attempt++
		currentKey, tokenErr := a.oauthRequestToken(ctx, credential, baseURL)
		if tokenErr != nil {
			return nil, tokenErr
		}
		key = currentKey
		federated := credential == workloadCredential || strings.HasPrefix(key, "fwat_")
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		defer cancelAttempt()
		req, err := http.NewRequestWithContext(attemptCtx, method, baseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, usageError("INVALID_REQUEST_URL", err.Error(), "path")
		}
		if method != http.MethodGet && unknownRawOperation(responseContract(ctx)) {
			// A caller key is forwarded without promising replay safety. Prevent
			// net/http from transparently replaying on a reused connection, even
			// for an empty body, independently of our explicit retry loop.
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.GetBody = nil
		}
		req.Header.Set("Accept", "application/json")
		if command := responseContract(ctx); command != nil && command.ResponseMediaType != "" {
			req.Header.Set("Accept", command.ResponseMediaType)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("User-Agent", "flintpay-cli/"+a.Info.Version)
		req.Header.Set("X-Flint-CLI-Version", a.Info.Version)
		if !versionProbe {
			req.Header.Set("Flint-Version", apispec.APIVersion())
		}
		if command := responseContract(ctx); command != nil {
			if operation, ok := openAPIOperationByID(command.OperationID); ok {
				for _, parameter := range operation.Parameters {
					if parameter["name"] == "Flint-Challenge-Token" {
						if token := os.Getenv("FLINT_CHALLENGE_TOKEN"); token != "" {
							req.Header.Set("Flint-Challenge-Token", token)
						}
					}
				}
			}
		}
		if command := responseContract(ctx); command != nil {
			if operation, ok := openAPIOperationByID(command.OperationID); ok {
				if mediaType, _ := requestContentSchema(operation.RequestBody); mediaType == "application/x-www-form-urlencoded" {
					req.Header.Set("Content-Type", mediaType)
				}
			}
		}
		if len(body) > 0 && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		if debug && !isSensitivePath(path) {
			debugPath := path
			if federated {
				debugPath = federationSafe(path, key)
			}
			fmt.Fprintf(a.Stderr, "debug: %s %s (attempt %d)\n", method, debugPath, attempt)
			if idempotencyKey != "" {
				debugKey := idempotencyKey
				if federated {
					debugKey = federationSafe(debugKey, key)
				}
				fmt.Fprintf(a.Stderr, "debug: idempotency key %s\n", debugKey)
			}
		}
		fact := &requestAttempt{}
		history.attempts = append(history.attempts, fact)
		attemptStarted := time.Now()
		cancelContext := cancelAttempt
		cancelAttempt = func() {
			fact.DurationMs = time.Since(attemptStarted).Milliseconds()
			cancelContext()
		}
		dispatched := false
		attemptClient := *client
		transport := client.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		attemptClient.Transport = requestDispatchTransport{base: transport, dispatched: &dispatched}
		resp, err := attemptClient.Do(req)
		if dispatched && !connectionFailedBeforeSend(err) {
			history.sent = true
		}
		if err != nil {
			cancelAttempt()
			if errors.Is(err, context.Canceled) {
				e := networkError("CLIENT_CANCELED", "The request was canceled.", err)
				history.failure(fact, e, true)
				return nil, e
			}
			if ctx.Err() != nil {
				e := networkError("CLIENT_TIMEOUT", "The request timed out.", ctx.Err())
				history.failure(fact, e, true)
				return nil, e
			}
			e := networkError("NETWORK_ERROR", "The request failed before Flint returned a response: "+transportErrorReason(err)+". Check FLINT_BASE_URL and your network connection.", err)
			if transportTimedOut(err) {
				e = networkError("CLIENT_TIMEOUT", "The request attempt timed out before Flint returned a response.", err)
			}
			history.failure(fact, e, !connectionFailedBeforeSend(err))
			if !dispatched {
				retryable := false
				e.retryable = &retryable
			}
			if dispatched && replaySafe && retryRequest(ctx, attempt, "") {
				continue
			}
			return nil, e
		}
		responseStatus = resp.StatusCode
		fact.HTTPStatus = resp.StatusCode
		history.requestID(fact, resp.Header.Get("X-Request-Id"))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			a.invalidateAuthContext(baseURL, key)
		}
		if versionProbe {
			resp.Body.Close()
			cancelAttempt()
			result := &apiResponse{Status: resp.StatusCode, Headers: resp.Header.Clone()}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return result, nil
			}
			e := apiErrorFromResponse(result)
			history.failure(fact, e, false)
			if errorRetryable(e) && retryRequest(ctx, attempt, resp.Header.Get("Retry-After")) {
				continue
			}
			return nil, e
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(responseLimit)+1))
		resp.Body.Close()
		cancelAttempt()
		if readErr != nil {
			if errors.Is(readErr, context.Canceled) {
				e := networkError("CLIENT_CANCELED", "The request was canceled while reading the response.", readErr)
				history.failure(fact, e, true)
				return nil, e
			}
			e := networkError("RESPONSE_READ_FAILED", "The Flint response could not be read.", readErr)
			if transportTimedOut(readErr) {
				e = networkError("CLIENT_TIMEOUT", "The request timed out while reading the response.", readErr)
			}
			history.failure(fact, e, true)
			retryAfter := resp.Header.Get("Retry-After")
			if delay, ok := parseRetryAfter(retryAfter); ok {
				milliseconds := delay.Milliseconds()
				e.RetryAfterMs = &milliseconds
			}
			if replaySafe && retryRequest(ctx, attempt, retryAfter) {
				continue
			}
			return nil, e
		}
		if len(raw) > responseLimit {
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				e := apiErrorFromResponse(&apiResponse{Status: resp.StatusCode, Headers: resp.Header.Clone()})
				e.Code = "RESPONSE_TOO_LARGE"
				e.Message = fmt.Sprintf("Flint returned an oversized response with HTTP %d.", resp.StatusCode)
				e.RequestID = resp.Header.Get("X-Request-Id")
				if replaySafe && errorRetryable(e) && retryRequest(ctx, attempt, resp.Header.Get("Retry-After")) {
					continue
				}
				return nil, e
			}
			return nil, invalidResponseError("RESPONSE_TOO_LARGE", fmt.Sprintf("The Flint response exceeded %d MiB.", responseLimit>>20), nil)
		}

		command := responseContract(ctx)
		if command != nil {
			if resp.StatusCode == http.StatusTemporaryRedirect && command.OperationID == "getReportDownload" {
				return a.followReportDownload(ctx, resp.Header.Get("Location"))
			}
			if resp.StatusCode >= 300 && resp.StatusCode < 400 && command.OperationID == "authorizePartnerInstall" {
				location := resp.Header.Get("Location")
				if location == "" {
					return nil, invalidResponseError("INVALID_REDIRECT_RESPONSE", "The authorization response omitted its redirect location.", nil)
				}
				return &apiResponse{Status: resp.StatusCode, Headers: resp.Header.Clone(), Value: map[string]any{"data": map[string]any{"url": location, "status": resp.StatusCode}}}, nil
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && command.ResponseMediaType != "" {
				mediaType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
				if mediaType != command.ResponseMediaType {
					return nil, invalidResponseError("INVALID_CONTENT_TYPE", "The response content type does not match the requested file format.", nil)
				}
				return &apiResponse{Status: resp.StatusCode, Headers: resp.Header.Clone(), Raw: raw, Value: fileResponse(raw, mediaType)}, nil
			}
		}
		var value any
		var decodeErr error
		if len(bytes.TrimSpace(raw)) > 0 {
			decodeErr = decodeJSONNumbers(raw, &value)
			if decodeErr != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil, invalidResponseError("INVALID_JSON_RESPONSE", fmt.Sprintf("Flint returned non-JSON data with HTTP %d.", resp.StatusCode), decodeErr)
			}
			if decodeErr != nil {
				value = nil
			}
		}
		result := &apiResponse{Status: resp.StatusCode, Headers: resp.Header.Clone(), Raw: raw, Value: value}
		if requestID, ok := lookupPath(value, "request_id"); ok {
			if id, ok := requestID.(string); ok {
				history.requestID(fact, id)
			}
		}
		if debug {
			if federated {
				fmt.Fprintf(a.Stderr, "debug: HTTP %d\n", result.Status)
			} else {
				debugResponse(a.Stderr, result)
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			e := apiErrorFromResponse(result)
			e.Cause = decodeErr
			history.requestID(fact, e.RequestID)
			history.failure(fact, e, method != http.MethodGet && method != http.MethodHead && idempotencyKey != "" && apiOutcomeUnknown(e))
			if federated {
				e = redactFederationError(e, key)
			}
			if resp.StatusCode == http.StatusUnauthorized && credential == workloadCredential {
				// A credential refresh must obey the server's retry policy too.
				// Refresh once, only after reading the complete error response.
				if federationRetried || !replaySafe || !errorRetryable(e) || !retryRequest(ctx, attempt, resp.Header.Get("Retry-After")) {
					return nil, e
				}
				federationRetried = true
				if _, tokenErr := a.workloadToken(ctx, baseURL, key); tokenErr != nil {
					return nil, tokenErr
				}
				continue
			}
			if replaySafe && errorRetryable(e) && retryRequest(ctx, attempt, resp.Header.Get("Retry-After")) {
				continue
			}
			return nil, e
		}
		return result, nil
	}
}

func rejectAPIRedirect(*http.Request, []*http.Request) error {
	// Public API operations have stable canonical paths. Treat redirects as API
	// responses instead of risking credential forwarding to another origin.
	return http.ErrUseLastResponse
}

func (a *App) doRequestWithin(ctx context.Context, timeout time.Duration, baseURL, key, method, path string, body []byte, idempotencyKey string, debug bool) (*apiResponse, *CLIError) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return a.doRequest(requestCtx, baseURL, key, method, path, body, idempotencyKey, debug)
}

func isSensitivePath(path string) bool {
	return strings.Contains(path, "/api-keys") || strings.Contains(path, "/test-key")
}
func retryableErrorStatus(status int) bool { return status == 429 || status >= 500 && status <= 599 }

func retryableStatus(status int) bool {
	return status == 429 || status == 500 || status == 502 || status == 503 || status == 504
}

func responseRetryable(resp *apiResponse) bool {
	if envelope, ok := resp.Value.(map[string]any); ok {
		if apiError, ok := envelope["error"].(map[string]any); ok {
			remediation, _ := apiError["remediation"].(map[string]any)
			return remediation["retry"] == "same_request"
		}
	}
	return retryableStatus(resp.Status)
}

func errorRetryable(err *CLIError) bool {
	if err.retryable != nil {
		return *err.retryable
	}
	// Other HTTP clients (upgrade downloads and streams) set their own status.
	return retryableErrorStatus(err.HTTPStatus) || err.ExitCode == ExitNetwork
}

func retryRequest(ctx context.Context, attempt int, retryAfter string) bool {
	if attempt >= 3 || ctx.Err() != nil {
		return false
	}
	delay := retryDelay(attempt, retryAfter)
	if deadline, ok := ctx.Deadline(); ok && delay >= time.Until(deadline) {
		return false
	}
	return sleepContext(ctx, delay)
}

func retryDelay(attempt int, retryAfter string) time.Duration {
	base := min(time.Duration(math.Pow(2, float64(attempt-1)))*500*time.Millisecond, 5*time.Second)
	delay := base/2 + time.Duration(mathrand.Float64()*float64(base/2))
	if d, ok := parseRetryAfter(retryAfter); ok {
		delay = max(delay, d)
	}
	return delay
}
func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if instant, err := http.ParseTime(value); err == nil {
		return max(time.Until(instant), 0), true
	}
	return 0, false
}
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func apiErrorFromResponse(resp *apiResponse) *CLIError {
	exit := ExitAPI
	if resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden {
		exit = ExitAuth
	}
	message := http.StatusText(resp.Status)
	if message == "" {
		message = fmt.Sprintf("Flint returned HTTP %d.", resp.Status)
	}
	retryable := responseRetryable(resp)
	e := &CLIError{requestSent: true, retryable: &retryable, HTTPStatus: resp.Status, ExitCode: exit, Type: errorTypeAPI, Code: "HTTP_" + strconv.Itoa(resp.Status), Message: message}
	if m, ok := resp.Value.(map[string]any); ok {
		if raw, ok := m["error"].(map[string]any); ok {
			if v, ok := raw["type"].(string); ok {
				e.Type = v
			}
			if v, ok := raw["code"].(string); ok {
				e.Code = v
			}
			if v, ok := raw["message"].(string); ok {
				e.Message = v
			}
			if v, ok := raw["param"].(string); ok {
				e.Param = v
			}
			if v, ok := raw["request_id"].(string); ok {
				e.RequestID = v
			}
			e.Details = raw
		}
		// OAuth token endpoints use the protocol's string error field rather
		// than Flint's resource error envelope. Keep the original code and
		// description available to both scripts and terminal users.
		if code, ok := m["error"].(string); ok && code != "" {
			e.Code = code
			if description, ok := m["error_description"].(string); ok && description != "" {
				e.Message = description
			}
			e.Details = m
		}
	}
	if e.RequestID == "" {
		e.RequestID = resp.Headers.Get("X-Request-Id")
	}
	if delay, ok := parseRetryAfter(resp.Headers.Get("Retry-After")); ok {
		milliseconds := delay.Milliseconds()
		e.RetryAfterMs = &milliseconds
	}
	return e
}
func transportErrorReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return terminalSafe(err.Error())
}

func networkError(code, message string, cause error) *CLIError {
	return &CLIError{ExitCode: ExitNetwork, Type: errorTypeNetwork, Code: code, Message: message, Cause: cause}
}

func withIdempotencyRetryContext(err *CLIError, idempotencyKey string) *CLIError {
	idempotencyKey = strings.Trim(idempotencyKey, " \t")
	if err == nil || !err.requestSent || idempotencyKey == "" {
		return err
	}
	if details, ok := err.Details.(map[string]any); ok && details["outcome"] == "unknown" {
		return err
	}
	err.idempotencyKey = idempotencyKey
	// A response can be lost after Flint commits a mutation. Preserve the logical
	// request key in both JSON and human output so a retry cannot duplicate it.
	details, _ := err.Details.(map[string]any)
	if details == nil {
		details = map[string]any{}
	} else {
		details = copyMap(details)
	}
	// API errors retain their original envelope in Details.
	target := details
	if _, serverEnvelope := details["type"]; serverEnvelope {
		target, _ = details["details"].(map[string]any)
		if target == nil {
			target = map[string]any{}
			if original := details["details"]; original != nil {
				target["problems"] = original
			}
		} else {
			target = copyMap(target)
		}
		details["details"] = target
	}
	target["idempotency_key"] = idempotencyKey
	remediation, _ := target["remediation"].(map[string]any)
	if remediation == nil {
		remediation = map[string]any{}
	} else {
		remediation = copyMap(remediation)
	}
	actions, _ := remediation["next_actions"].([]any)
	retry := remediationRetry(details["remediation"])
	if details["remediation"] == nil {
		retry = remediationRetry(target["remediation"])
	}
	message := idempotencyRetryAdvice(retry, idempotencyKey)
	if message == "" {
		err.Details = details
		return err
	}
	for _, action := range actions {
		if next, ok := action.(map[string]any); ok && next["reason_message"] == message {
			err.Details = details
			return err
		}
	}
	remediation["next_actions"] = append(append([]any(nil), actions...), map[string]any{"reason_message": message})
	target["remediation"] = remediation
	err.Details = details
	return err
}

// Observe dispatch separately from key allocation. Client validation and an
// already canceled context can fail before RoundTrip ever runs.
type requestDispatchTransport struct {
	base       http.RoundTripper
	dispatched *bool
}

func (t requestDispatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	// net/http validates values inside its transport. Check them before marking
	// possible dispatch, including when a caller supplies another RoundTripper.
	for name, values := range req.Header {
		for _, value := range values {
			for i := 0; i < len(value); i++ {
				if c := value[i]; c == 127 || c < 32 && c != '\t' {
					return nil, fmt.Errorf("invalid HTTP header value for %q", name)
				}
			}
		}
	}
	*t.dispatched = true
	return t.base.RoundTrip(req)
}

func remediationRetry(value any) string {
	remediation, _ := value.(map[string]any)
	retry, _ := remediation["retry"].(string)
	return retry
}

func idempotencyRetryAdvice(retry, key string) string {
	switch retry {
	case "same_request":
		return "Wait for Retry-After or use backoff, then resend the identical request with --idempotency-key " + key + "."
	case "after_refresh":
		return "Read the current resource, rebuild the request, and use a new --idempotency-key for the new request."
	default:
		return ""
	}
}

func invalidResponseError(code, message string, cause error) *CLIError {
	return &CLIError{ExitCode: ExitAPI, Type: errorTypeAPI, Code: code, Message: message, Cause: cause}
}

func newIdempotencyKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "flint-cli-" + hex.EncodeToString(b), nil
}

func debugResponse(w io.Writer, resp *apiResponse) {
	apiVersion, traceID := "", ""
	if envelope, ok := resp.Value.(map[string]any); ok {
		if meta, ok := envelope["meta"].(map[string]any); ok {
			apiVersion, _ = meta["api_version"].(string)
			traceID, _ = meta["trace_id"].(string)
		}
	}
	fmt.Fprintf(w, "debug: HTTP %d", resp.Status)
	if apiVersion != "" {
		fmt.Fprintf(w, " api_version=%s", apiVersion)
	}
	if traceID != "" {
		fmt.Fprintf(w, " trace_id=%s", traceID)
	}
	fmt.Fprintln(w)
}
