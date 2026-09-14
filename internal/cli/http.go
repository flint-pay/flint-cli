package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	apispec "github.com/flint-pay/flint-cli/internal/spec"
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
)

type AuthContext struct {
	CredentialScope string   `json:"-"`
	AuthType        string   `json:"auth_type"`
	APIKeyID        string   `json:"api_key_id"`
	Name            string   `json:"name,omitempty"`
	Environment     string   `json:"environment"`
	MerchantID      string   `json:"merchant_id"`
	OrganizationID  string   `json:"organization_id,omitempty"`
	SandboxID       string   `json:"sandbox_id,omitempty"`
	Scopes          []string `json:"scopes"`
	ExpiresAt       *string  `json:"expires_at"`
}

type authContextEnvelope struct {
	Data      AuthContext    `json:"data"`
	RequestID string         `json:"request_id"`
	Meta      map[string]any `json:"meta,omitempty"`
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
	if a.BaseURL != "" {
		return validateBaseURL(a.BaseURL)
	}
	if value := strings.TrimSpace(os.Getenv("FLINT_BASE_URL")); value != "" {
		return validateBaseURL(value)
	}
	_, err := credentialEnvironment(key)
	if err != nil {
		return "", err
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
	return strings.TrimRight(value, "/"), nil
}

func (a *App) fetchAuthContext(ctx context.Context, baseURL, key string, debug bool) (AuthContext, *CLIError) {
	envelope, e := a.fetchAuthContextResponse(ctx, baseURL, key, debug)
	if e != nil {
		return AuthContext{}, e
	}
	return envelope.Data, nil
}

func (a *App) fetchAuthContextResponse(ctx context.Context, baseURL, key string, debug bool) (authContextEnvelope, *CLIError) {
	resp, e := a.doRequest(ctx, baseURL, key, http.MethodGet, "/v1/developer/auth-context", nil, "", debug)
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
	return envelope, nil
}

func (a *App) fetchCurrentAPIVersion(ctx context.Context, baseURL string, debug bool) (string, *CLIError) {
	response, requestError := a.doRequest(ctx, baseURL, "", http.MethodGet, "/v1/openapi.json", nil, "", debug)
	if requestError != nil {
		return "", requestError
	}
	document, _ := response.Value.(map[string]any)
	catalog, _ := document["x-flint-api-releases"].(map[string]any)
	version, _ := catalog["current_version"].(string)
	if _, err := time.Parse(time.DateOnly, version); err != nil {
		return "", invalidResponseError("INVALID_RESPONSE", "The OpenAPI release catalog did not include a valid current API version.", err)
	}
	return version, nil
}

func (a *App) doRequest(ctx context.Context, baseURL, key, method, path string, body []byte, idempotencyKey string, debug bool) (*apiResponse, *CLIError) {
	if !strings.HasPrefix(path, "/v1/") && path != "/v1" {
		return nil, usageError("NON_PUBLIC_API_PATH", "Only documented /v1 public API paths are allowed.", "path")
	}
	client := a.HTTPClient
	if client == nil {
		client = defaultAPIHTTPClient
	} else if client.CheckRedirect == nil {
		clone := *client
		clone.CheckRedirect = rejectAPIRedirect
		client = &clone
	}
	attempt := 0
	for {
		attempt++
		req, err := http.NewRequestWithContext(ctx, method, baseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, usageError("INVALID_REQUEST_URL", err.Error(), "path")
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
		req.Header.Set("Flint-Version", apispec.APIVersion())
		if command := responseContract(ctx); command != nil {
			for _, requirement := range command.Security {
				if _, ok := requirement["CheckoutSessionSecretHeader"]; ok {
					if secret := os.Getenv("FLINT_CHECKOUT_SESSION_SECRET"); secret != "" {
						req.Header.Set("X-Checkout-Session-Secret", secret)
						req.Header.Set("X-Checkout-Session-ID", os.Getenv("FLINT_CHECKOUT_SESSION_ID"))
					}
				}
			}
			if operation, ok := openAPIOperationByID(command.OperationID); ok {
				for _, parameter := range operation.Parameters {
					if parameter["name"] == "X-Turnstile-Token" {
						if token := os.Getenv("FLINT_TURNSTILE_TOKEN"); token != "" {
							req.Header.Set("X-Turnstile-Token", token)
						}
					}
				}
			}
		}
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		if debug && !isSensitivePath(path) {
			fmt.Fprintf(a.Stderr, "debug: %s %s (attempt %d)\n", method, path, attempt)
			if idempotencyKey != "" {
				fmt.Fprintf(a.Stderr, "debug: idempotency key %s\n", idempotencyKey)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, withIdempotencyRetryContext(
					networkError("REQUEST_TIMEOUT", "The request timed out.", ctx.Err()),
					idempotencyKey,
				)
			}
			if !retryBudgetAllows(ctx, attempt) {
				return nil, withIdempotencyRetryContext(
					networkError("NETWORK_ERROR", "The request failed before Flint returned a response.", err),
					idempotencyKey,
				)
			}
			if !sleepContext(ctx, retryDelay(attempt, "")) {
				return nil, withIdempotencyRetryContext(
					networkError("REQUEST_TIMEOUT", "The request timed out.", ctx.Err()),
					idempotencyKey,
				)
			}
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
		resp.Body.Close()
		if readErr != nil {
			if retryBudgetAllows(ctx, attempt) {
				if !sleepContext(ctx, retryDelay(attempt, "")) {
					return nil, withIdempotencyRetryContext(
						networkError("REQUEST_TIMEOUT", "The request timed out.", ctx.Err()),
						idempotencyKey,
					)
				}
				continue
			}
			return nil, withIdempotencyRetryContext(
				networkError("RESPONSE_READ_FAILED", "The Flint response could not be read.", readErr),
				idempotencyKey,
			)
		}
		if len(raw) > 16<<20 {
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				e := apiErrorFromResponse(&apiResponse{Status: resp.StatusCode, Headers: resp.Header.Clone()})
				e.Code = "RESPONSE_TOO_LARGE"
				e.Message = fmt.Sprintf("Flint returned an oversized response with HTTP %d.", resp.StatusCode)
				e.RequestID = resp.Header.Get("X-Request-Id")
				if retryableStatus(resp.StatusCode) {
					e = withIdempotencyRetryContext(e, idempotencyKey)
				}
				return nil, e
			}
			return nil, withIdempotencyRetryContext(
				invalidResponseError("RESPONSE_TOO_LARGE", "The Flint response exceeded 16 MiB.", nil),
				idempotencyKey,
			)
		}
		if retryableStatus(resp.StatusCode) && retryBudgetAllows(ctx, attempt) {
			if !sleepContext(ctx, retryDelay(attempt, resp.Header.Get("Retry-After"))) {
				return nil, withIdempotencyRetryContext(
					networkError("REQUEST_TIMEOUT", "The request timed out.", ctx.Err()),
					idempotencyKey,
				)
			}
			continue
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
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := decodeJSONNumbers(raw, &value); err != nil {
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					e := apiErrorFromResponse(&apiResponse{Status: resp.StatusCode, Headers: resp.Header.Clone()})
					e.Cause = err
					if retryableStatus(resp.StatusCode) {
						e = withIdempotencyRetryContext(e, idempotencyKey)
					}
					return nil, e
				}
				return nil, withIdempotencyRetryContext(
					invalidResponseError("INVALID_JSON_RESPONSE", fmt.Sprintf("Flint returned non-JSON data with HTTP %d.", resp.StatusCode), err),
					idempotencyKey,
				)
			}
		}
		result := &apiResponse{Status: resp.StatusCode, Headers: resp.Header.Clone(), Raw: raw, Value: value}
		if debug {
			debugResponse(a.Stderr, result)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			e := apiErrorFromResponse(result)
			if retryableStatus(resp.StatusCode) {
				e = withIdempotencyRetryContext(e, idempotencyKey)
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
func retryableStatus(status int) bool {
	return status == 429 || status == 502 || status == 503 || status == 504
}
func retryBudgetAllows(ctx context.Context, attempt int) bool {
	if attempt >= 6 {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > 250*time.Millisecond
}
func retryDelay(attempt int, retryAfter string) time.Duration {
	if d, ok := parseRetryAfter(retryAfter); ok {
		return d
	}
	base := time.Duration(math.Pow(2, float64(attempt-1))) * 250 * time.Millisecond
	if base > 5*time.Second {
		base = 5 * time.Second
	}
	return time.Duration(mathrand.Float64() * float64(base))
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
	if resp.Status == http.StatusUnauthorized {
		exit = ExitAuth
	}
	message := http.StatusText(resp.Status)
	if message == "" {
		message = fmt.Sprintf("Flint returned HTTP %d.", resp.Status)
	}
	e := &CLIError{ExitCode: exit, Type: "api_error", Code: "HTTP_" + strconv.Itoa(resp.Status), Message: message}
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
	return e
}
func networkError(code, message string, cause error) *CLIError {
	return &CLIError{ExitCode: ExitNetwork, Type: "network_error", Code: code, Message: message, Cause: cause}
}

func withIdempotencyRetryContext(err *CLIError, idempotencyKey string) *CLIError {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if err == nil || idempotencyKey == "" {
		return err
	}
	// A response can be lost after Flint commits a mutation. Preserve the logical
	// request key in both JSON and human output so a retry cannot duplicate it.
	err.Details = map[string]any{
		"idempotency_key": idempotencyKey,
		"remediation": map[string]any{"next_actions": []any{
			map[string]any{"reason_message": "Retry the same command with --idempotency-key " + idempotencyKey + "."},
		}},
	}
	return err
}

func invalidResponseError(code, message string, cause error) *CLIError {
	return &CLIError{ExitCode: ExitAPI, Type: "api_error", Code: code, Message: message, Cause: cause}
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
