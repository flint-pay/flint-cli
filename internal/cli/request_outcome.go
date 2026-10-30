package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Three attempts bound both retry work and the diagnostics kept for recovery.
type requestAttempt struct {
	DurationMs int64  `json:"duration_ms"`
	HTTPStatus int    `json:"http_status,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
}

type requestHistory struct {
	sent       bool
	started    time.Time
	attempts   []*requestAttempt
	requestIDs []string
	uncertain  *CLIError
}

func (h *requestHistory) requestID(fact *requestAttempt, id string) {
	if id == "" {
		return
	}
	fact.RequestID = id
	for _, seen := range h.requestIDs {
		if seen == id {
			return
		}
	}
	h.requestIDs = append(h.requestIDs, id)
}

func (h *requestHistory) failure(fact *requestAttempt, e *CLIError, uncertain bool) {
	fact.ErrorCode = e.Code
	if uncertain && h.uncertain == nil {
		h.uncertain = e
	}
}

func transportTimedOut(err error) bool {
	var timeout net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout()
}

func connectionFailedBeforeSend(err error) bool {
	var operation *net.OpError
	var dns *net.DNSError
	return errors.As(err, &operation) && operation.Op == "dial" || errors.As(err, &dns)
}

func (h *requestHistory) finalError(ctx context.Context, e *CLIError, method, path, key string) *CLIError {
	canceled := errors.Is(e.Cause, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) && errorRetryable(e)
	timedOut := transportTimedOut(e.Cause) || errors.Is(ctx.Err(), context.DeadlineExceeded) && errorRetryable(e)
	// A later refused attempt (including an auth or throttle response before
	// idempotency replay) cannot settle an earlier write's transport ambiguity.
	write := method != http.MethodGet && method != http.MethodHead
	uncertain := h.uncertain != nil && (write || e.ExitCode == ExitNetwork)
	if !h.sent || len(h.attempts) == 0 || !(canceled || timedOut || uncertain) {
		return e
	}

	elapsed := time.Since(h.started)
	seconds := strings.TrimRight(strings.TrimRight(strconv.FormatFloat(elapsed.Seconds(), 'f', 3, 64), "0"), ".")
	code, message := "CLIENT_TIMEOUT", "No definite result was received after "+seconds+" seconds."
	if timedOut || h.uncertain != nil && transportTimedOut(h.uncertain.Cause) {
		message = "The request timed out after " + seconds + " seconds."
	}
	if canceled {
		code, message = "CLIENT_CANCELED", "The request was canceled after "+seconds+" seconds."
	}
	message += " Its outcome is unknown."
	reads := recoveryReads(method, path, key)
	if len(reads) > 0 {
		routes := make([]string, 0, len(reads))
		for _, read := range reads {
			routes = append(routes, read["method"]+" "+read["path"])
		}
		message += " Read " + strings.Join(routes, " and ") + " before retrying."
	} else {
		message += " Read the affected resources and request logs before retrying."
	}
	if write && key != "" {
		message += " If you call this write again, use the same idempotency key."
	}
	for _, read := range reads {
		if strings.Contains(read["path"], "idempotency_key=") {
			message += " An empty recovery list does not prove that the write had no effect."
			break
		}
	}
	details := map[string]any{
		"outcome": "unknown", "retry": "none", "attempt_count": len(h.attempts), "attempts": h.attempts,
		"request_ids": append([]string{}, h.requestIDs...), "recovery_reads": reads,
		"remediation": map[string]any{"retry": "none"},
	}
	if write && key != "" {
		details["idempotency_key"] = key
		details["remediation"].(map[string]any)["next_actions"] = []any{map[string]any{"reason_message": "Idempotency key: " + key}}
	}
	// Keep the last API error, including its own retry remedy, separate from the
	// CLI's unknown-outcome policy. Do not replace an in-progress API remedy.
	if e.ExitCode != ExitNetwork && e.Details != nil {
		details["last_error"] = e.Details
	}
	cause := e.Cause
	if cause == nil && h.uncertain != nil {
		cause = h.uncertain.Cause
	}
	result := networkError(code, message, cause)
	result.Details, result.HTTPStatus, result.RequestID, result.RetryAfterMs = details, e.HTTPStatus, e.RequestID, e.RetryAfterMs
	if result.RequestID == "" && len(h.requestIDs) > 0 {
		result.RequestID = h.requestIDs[len(h.requestIDs)-1]
	}
	retryable := false
	result.retryable = &retryable
	// Human output includes the same bounded attempt facts as JSON.
	facts := make([]string, 0, len(h.attempts))
	for i, fact := range h.attempts {
		summary := fmt.Sprintf("%d: %d ms", i+1, fact.DurationMs)
		if fact.ErrorCode != "" {
			summary += " " + fact.ErrorCode
		}
		if fact.HTTPStatus != 0 {
			summary += fmt.Sprintf(" HTTP %d", fact.HTTPStatus)
		}
		if fact.RequestID != "" {
			summary += " (" + fact.RequestID + ")"
		}
		facts = append(facts, summary)
	}
	result.Message += fmt.Sprintf(" Attempts (%d): %s.", len(facts), strings.Join(facts, "; "))
	return result
}

// Match the actual operation, including flint api calls. CLI command names and
// convenience aliases do not define recovery semantics.
func recoveryReads(method, path, key string) []map[string]string {
	operation, ok := matchPublicOperation(method, path)
	if !ok {
		return []map[string]string{}
	}
	parsed, err := url.Parse(path)
	if err != nil {
		return []map[string]string{}
	}
	parent := parsed.EscapedPath()[:strings.LastIndex(parsed.EscapedPath(), "/")]
	read := func(path string, filter bool) map[string]string {
		if filter && key != "" {
			path += "?" + url.Values{"idempotency_key": []string{key}}.Encode()
		}
		return map[string]string{"method": "GET", "path": path}
	}
	switch operation.OperationID {
	case "confirmPaymentIntent", "capturePaymentIntent":
		return []map[string]string{read(parent, false)}
	case "payOrder":
		return []map[string]string{read(parent, false), read(parent+"/payment-attempts", true)}
	case "createRefund":
		return []map[string]string{read("/v1/refunds", true)}
	default:
		return []map[string]string{}
	}
}
