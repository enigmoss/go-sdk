package enigmoss

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Error is every error of a request that reached the network: an error response of the
// API, a response the SDK cannot read, or a transport failure (Status 0). Errors found
// before sending (a malformed amount, a missing key) are plain errors.
//
// Money logic needs two checks, OutcomeUnknown and IsOrderIDTaken; branch on Code for the
// rest, never on the message text.
type Error struct {
	// Status is the HTTP status, 0 when no response was received.
	Status int
	// Code is the stable error code, such as "payout.insufficient_funds" (see the Code*
	// constants). A response without the API error body has the code "http.<status>".
	// Empty for transport failures.
	Code string
	// Message is the server message exactly as sent, or the SDK's description.
	Message string
	// RequestID identifies the request; quote it when you contact support.
	RequestID string
	// Fields maps a request field to its validation error, for Code "validation.failed".
	// The key "" is an error of the whole request.
	Fields map[string]string

	cause         error
	unknown       bool // the request may have been executed
	sent          bool // transport failure after the headers were written
	envelope      bool // the body was the API error envelope
	retryable     bool
	retryAfter    int
	hasRetryAfter bool
	details       map[string]any
	hint          string
}

// Error returns "<message> [<code>, request <id>]" and a hint when the SDK has one.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(e.Message, " "))
	var tags []string
	if e.Code != "" {
		tags = append(tags, e.Code)
	}
	if e.RequestID != "" {
		tags = append(tags, "request "+e.RequestID)
	}
	if len(tags) > 0 {
		b.WriteString(" [" + strings.Join(tags, ", ") + "]")
	}
	if e.hint != "" {
		b.WriteString(" " + e.hint)
	}
	return b.String()
}

// Unwrap returns the net/http error of a transport failure.
func (e *Error) Unwrap() error { return e.cause }

// OutcomeUnknown reports whether the request may have been executed although the call
// failed: the connection broke after sending, the server answered 5xx or
// "idempotency.in_progress", the 2xx response was unreadable, or an earlier attempt of the
// same call may have run.
//
// For an operation that moves money: keep your record "pending" and reconcile later with
// FindByOrderID. Never create a new order_id for it.
func OutcomeUnknown(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.unknown
}

// IsOrderIDTaken reports whether a create failed because the order_id already belongs to
// an object: get that object with FindByOrderID. It is false for a request with this
// order_id that is still in progress.
func IsOrderIDTaken(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.orderIDTaken()
}

func (e *Error) orderIDTaken() bool {
	if e.Code != CodeResourceConflict {
		return false
	}
	if _, ok := e.detailString("id"); !ok {
		return false
	}
	_, status := e.details["status"]
	_, reason := e.details["reason"]
	return !status && !reason
}

func (e *Error) detailString(name string) (string, bool) {
	switch v := e.details[name].(type) {
	case string:
		return v, v != ""
	case json.Number:
		return v.String(), true
	}
	return "", false
}

func (e *Error) detailInt(name string) (int64, bool) {
	n, ok := e.details[name].(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// Error codes. Codes are stable; the server may add new ones at any time, so keep a
// default branch when you switch on Error.Code.
const (
	CodeRequestInvalid        = "request.invalid"
	CodeResourceNotFound      = "resource.not_found"
	CodeResourceConflict      = "resource.conflict"
	CodeLockNotAcquired       = "lock.not_acquired"
	CodeDomainRuleViolation   = "domain.rule_violation"
	CodeValidationFailed      = "validation.failed"
	CodeRateLimitExceeded     = "rate_limit.exceeded"
	CodeInternalServerError   = "internal.server_error"
	CodeInternalUnexpected    = "internal.unexpected"
	CodeDependencyGateway     = "dependency.gateway"
	CodeDependencyExternal    = "dependency.external_service"
	CodeDependencyUnavailable = "dependency.unavailable"

	CodeSignatureInvalid = "auth.signature_invalid"
	CodeTimestampExpired = "auth.timestamp_expired"
	CodeProjectUnknown   = "auth.project_unknown"
	CodeKeyInactive      = "auth.key_inactive"
	CodeIPForbidden      = "auth.ip_forbidden"
	CodeDomainForbidden  = "auth.domain_forbidden"
	CodePermissionDenied = "api_permission.denied"

	CodeIdempotencyKeyRequired     = "idempotency.key_required"
	CodeIdempotencyKeyInvalid      = "idempotency.key_invalid"
	CodeIdempotencyPayloadMismatch = "idempotency.payload_mismatch"
	CodeIdempotencyInProgress      = "idempotency.in_progress"

	CodePayoutPayoutsForbidden  = "payout.payouts_forbidden"
	CodePayoutInsufficientFunds = "payout.insufficient_funds"
	CodePayoutNotCancellable    = "payout.not_cancellable"

	CodeRefundPayoutsForbidden         = "refund.payouts_forbidden"
	CodeRefundPaymentNotRefundable     = "refund.payment_not_refundable"
	CodeRefundWalletNotRefundable      = "refund.wallet_not_refundable"
	CodeRefundAmountExceedsPaid        = "refund.amount_exceeds_paid"
	CodeRefundAmountTooSmall           = "refund.amount_too_small"
	CodeRefundInsufficientFunds        = "refund.insufficient_funds"
	CodeRefundAMLNotSupported          = "refund.aml_not_supported"
	CodeRefundNothingToRefund          = "refund.nothing_to_refund"
	CodeTransferWalletNotFound         = "transfer.wallet_not_found"
	CodeTransferPayoutsForbidden       = "transfer.payouts_forbidden"
	CodeTransferPersonalWalletNotFound = "transfer.personal_wallet_not_found"
	CodeTransferInsufficientFunds      = "transfer.insufficient_funds"
	CodeTransferAmountBelowMin         = "transfer.amount_below_min"
	CodeTransferAmountAboveMax         = "transfer.amount_above_max"

	CodeWebhookEventNotLatest          = "webhook.event_not_latest"
	CodeWebhookRedeliveryTooManyEvents = "webhook.redelivery_too_many_events"
	CodeWebhookRedeliveryLimitReached  = "webhook.redelivery_limit_reached"
	CodeWebhookEventNotRedeliverable   = "webhook.event_not_redeliverable"
)

var retryAfterPattern = regexp.MustCompile(`^\d{1,10}$`)

// mapError turns a non-2xx response into an *Error. The API error envelope is
// {"error": {"type", "code", "message", "retryable", "details"?, "violations"?},
// "meta": {"request_id", "retry_after"?}}; anything else is a proxy page, mapped by status.
func mapError(res *http.Response, raw []byte) *Error {
	e := &Error{Status: res.StatusCode, RequestID: res.Header.Get("X-Request-Id")}
	if v := strings.TrimSpace(res.Header.Get("Retry-After")); retryAfterPattern.MatchString(v) {
		e.retryAfter, _ = strconv.Atoi(v)
		e.hasRetryAfter = true
	}

	var envelope struct {
		Error *struct {
			Code       *string        `json:"code"`
			Message    any            `json:"message"`
			Retryable  any            `json:"retryable"`
			Details    map[string]any `json:"details"`
			Violations []any          `json:"violations"`
		} `json:"error"`
		Meta map[string]any `json:"meta"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&envelope) != nil || envelope.Error == nil || envelope.Error.Code == nil {
		withoutEnvelope(e)
		return e
	}

	env := envelope.Error
	e.envelope = true
	e.Code = *env.Code
	e.Message = e.Code
	if m, ok := env.Message.(string); ok {
		e.Message = m
	}
	e.retryable, _ = env.Retryable.(bool)
	e.details = env.Details
	if id, ok := envelope.Meta["request_id"].(string); ok {
		e.RequestID = id
	}
	if n, ok := envelope.Meta["retry_after"].(json.Number); ok {
		if v, err := strconv.Atoi(n.String()); err == nil {
			e.retryAfter, e.hasRetryAfter = v, true
		}
	}
	e.Fields = violations(env.Violations)
	e.unknown = e.Status >= 500 || e.Code == CodeIdempotencyInProgress
	e.hint = hint(e)
	return e
}

func withoutEnvelope(e *Error) {
	e.Code = "http." + strconv.Itoa(e.Status)
	if e.Status < 500 && e.Status != http.StatusTooManyRequests {
		e.Message = fmt.Sprintf("Unexpected HTTP %d response without an API error body.", e.Status)
		if e.Status == http.StatusNotFound {
			e.hint = "Check the baseURL passed to enigmoss.NewClient: the host does not serve this API."
		}
		return
	}
	e.Message = fmt.Sprintf("HTTP %d from the API host or a proxy in front of it.", e.Status)
	e.retryable = e.Status == http.StatusTooManyRequests || (e.Status >= 502 && e.Status <= 504)
	e.unknown = e.Status >= 500
}

func violations(raw []any) map[string]string {
	fields := map[string]string{}
	for _, item := range raw {
		v, ok := item.(map[string]any)
		if !ok {
			continue
		}
		field, _ := v["field"].(string)
		message, _ := v["message"].(string)
		if message == "" {
			message, _ = v["rule"].(string)
		}
		if prev, ok := fields[field]; ok {
			message = prev + "; " + message
		}
		fields[field] = message
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// hint tells what to fix: which key, which permission, the clock.
func hint(e *Error) string {
	switch e.Code {
	case CodeSignatureInvalid:
		return "Check that the key type fits the endpoint (payouts, refunds and transfers need the payout key), that the key belongs to this project, and that the body is sent as signed."
	case CodeTimestampExpired:
		return "Sync the server clock (NTP)."
	case CodeProjectUnknown:
		return "Check the projectID passed to enigmoss.NewClient."
	case CodeKeyInactive:
		return "Enable the API for the project in the dashboard."
	case CodeIPForbidden:
		if ip, ok := e.detailString("ip"); ok {
			return fmt.Sprintf("Add the IP address %s to the IP whitelist of the project in the dashboard.", ip)
		}
		return "Add this server's IP address to the IP whitelist of the project in the dashboard."
	case CodeDomainForbidden:
		return "Use the API host shown in your dashboard as the baseURL."
	case CodePermissionDenied:
		if p, ok := e.detailString("permission"); ok {
			return fmt.Sprintf("Enable the %q API permission in the dashboard.", p)
		}
		return "Enable the API permission of this endpoint in the dashboard."
	case CodeIdempotencyPayloadMismatch:
		return "This order_id was already sent with other parameters: one order_id is one operation, repeat it with the same parameters."
	case CodeIdempotencyInProgress:
		return "The first request with this order_id is still running: reconcile later with FindByOrderID, do not create a new order_id."
	case CodeRateLimitExceeded:
		if e.hasRetryAfter {
			return fmt.Sprintf("Retry in %d s.", e.retryAfter)
		}
	}
	return ""
}
