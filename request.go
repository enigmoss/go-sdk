package enigmoss

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Fixed protocol settings, identical in every official SDK.
const (
	attemptTimeout    = 30 * time.Second
	payoutTimeout     = 180 * time.Second // payouts and refunds call the blockchain gateway synchronously
	maxRetries        = 2
	maxRetryDelay     = 10 * time.Second // the longest Retry-After a single call waits for
	iterateRetryDelay = 60 * time.Second // long scans hit the rate limit: All waits longer
	backoffBase       = 0.5              // seconds
	backoffCap        = 8.0              // seconds
	minClockDrift     = 30               // seconds; less is network latency, not a wrong clock
	maxClockDrift     = 86400            // seconds
	maxResponseBytes  = 32 << 20
	derivedKeyPrefix  = "sdk1-"
)

// keyScope is the API key an endpoint accepts.
type keyScope int

const (
	scopePayment keyScope = iota
	scopePayout
	scopeAny // either key; the payment key when both are set
)

// retryClass decides which failures of an endpoint are retried.
type retryClass int

const (
	// classRead: reads, calculations and idempotent updates, retried on any transient failure.
	classRead retryClass = iota
	// classMoney: creates and actions protected by an Idempotency-Key, retried only when
	// the request was not sent or the server says it was not executed.
	classMoney
	// classUnsafe: actions without an Idempotency-Key, retried only when nothing was sent.
	classUnsafe
)

type idempotency int

const (
	idemNone    idempotency = iota
	idemDerived             // "sdk1-" + sha256(request URI, order_id): a repeated create gets the stored response
	idemRandom              // a random UUIDv4 per call, reused by the SDK's own retries
)

// endpoint is the static description of one API operation.
type endpoint struct {
	method   string
	path     string // after /api/v4, with %s for path parameters
	scope    keyScope
	class    retryClass
	idem     idempotency
	timeout  time.Duration // per attempt
	resource string        // for "payouts need the payout key"
}

func get(path string, scope keyScope, resource string) *endpoint {
	return &endpoint{method: http.MethodGet, path: path, scope: scope, class: classRead, timeout: attemptTimeout, resource: resource}
}

func create(path string, scope keyScope, timeout time.Duration, resource string) *endpoint {
	return &endpoint{method: http.MethodPost, path: path, scope: scope, class: classMoney, idem: idemDerived, timeout: timeout, resource: resource}
}

func action(path string, scope keyScope, resource string) *endpoint {
	return &endpoint{method: http.MethodPost, path: path, scope: scope, class: classMoney, idem: idemRandom, timeout: attemptTimeout, resource: resource}
}

func unsafePost(path string, scope keyScope, resource string) *endpoint {
	return &endpoint{method: http.MethodPost, path: path, scope: scope, class: classUnsafe, timeout: attemptTimeout, resource: resource}
}

func calculation(path string, scope keyScope, resource string) *endpoint {
	return &endpoint{method: http.MethodPost, path: path, scope: scope, class: classRead, timeout: attemptTimeout, resource: resource}
}

func patch(path string, scope keyScope, resource string) *endpoint {
	return &endpoint{method: http.MethodPatch, path: path, scope: scope, class: classRead, timeout: attemptTimeout, resource: resource}
}

// call is one API call: what to send and how.
type call struct {
	ep            *endpoint
	params        []string // path parameters, in order
	query         []queryParam
	body          any    // nil: "{}" on POST/PATCH, nothing on GET
	orderID       string // the order_id the Idempotency-Key is derived from
	maxRetryDelay time.Duration
}

type queryParam struct{ name, value string }

// response is a successful (2xx) answer.
type response struct {
	status    int
	requestID string
	data      json.RawMessage
	meta      json.RawMessage
}

type core struct {
	origin     string
	projectID  string
	paymentKey secret
	payoutKey  secret
	http       *http.Client
	userAgent  string
	clockSkew  atomic.Int64 // seconds added to the local clock; the only mutable state

	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	random func() float64
}

func userAgent() string {
	return "enigmoss-go-sdk/" + Version + " go/" + strings.TrimPrefix(runtime.Version(), "go")
}

func (c *core) key(ep *endpoint) (secret, error) {
	switch ep.scope {
	case scopePayment:
		if c.paymentKey != nil {
			return c.paymentKey, nil
		}
		return nil, fmt.Errorf("enigmoss: %s need the payment key: pass enigmoss.WithPaymentKey", ep.resource)
	case scopePayout:
		if c.payoutKey != nil {
			return c.payoutKey, nil
		}
		return nil, fmt.Errorf("enigmoss: %s need the payout key: pass enigmoss.WithPayoutKey", ep.resource)
	default:
		if c.paymentKey != nil {
			return c.paymentKey, nil
		}
		return c.payoutKey, nil
	}
}

// do executes one API call: builds and signs the request, retries safely and maps errors.
// The body bytes and the Idempotency-Key are fixed before the first attempt; the timestamp
// and the signature are fresh on every attempt.
func (c *core) do(ctx context.Context, cl call) (*response, error) {
	ep := cl.ep
	key, err := c.key(ep)
	if err != nil {
		return nil, err
	}
	for _, p := range cl.params {
		// A dot segment could be resolved by a proxy into another endpoint.
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("enigmoss: %q is not a valid path parameter", p)
		}
	}
	requestURI := buildRequestURI(ep, cl.params, cl.query)
	u, err := url.Parse(c.origin + requestURI)
	if err != nil || u.RequestURI() != requestURI {
		return nil, fmt.Errorf("enigmoss: cannot send the request URI %q unchanged", requestURI)
	}

	var body []byte
	if ep.method != http.MethodGet {
		if body, err = encodeBody(cl.body); err != nil {
			return nil, err
		}
	}
	idemKey := ""
	switch ep.idem {
	case idemDerived:
		idemKey = derivedIdempotencyKey(requestURI, cl.orderID)
	case idemRandom:
		idemKey = uuid4()
	}

	retryDelayLimit := maxRetryDelay
	if cl.maxRetryDelay > 0 {
		retryDelayLimit = cl.maxRetryDelay
	}
	deadline := c.now().Add(ep.timeout*(maxRetries+1) + retryDelayLimit*maxRetries)

	var (
		attempt        int
		clockCorrected bool
		mayHaveRun     bool // an earlier attempt of this series may have been executed
	)
	for {
		attempt++
		resp, apiErr := c.attempt(ctx, ep, u, requestURI, key, body, idemKey)
		if apiErr == nil {
			return resp, nil
		}

		if apiErr.Code == CodeTimestampExpired && !clockCorrected {
			if serverTime, ok := apiErr.detailInt("server_time"); ok && c.adjustClock(serverTime) {
				// Authentication runs before any business logic: signing again is always
				// safe and is not a retry.
				clockCorrected = true
				attempt--
				continue
			}
		}

		delay, retry := c.retryDelay(ep, apiErr, idemKey != "", attempt, retryDelayLimit)
		if retry && (attempt > maxRetries || !c.now().Add(delay).Before(deadline)) {
			retry = false
		}
		if retry && ctx.Err() == nil {
			mayHaveRun = mayHaveRun || apiErr.unknown
			if err := c.sleep(ctx, delay); err == nil {
				continue
			}
		}
		// The rule of the series: once an attempt may have been executed, a later failure
		// does not prove that nothing happened, unless the server says the order_id is taken.
		if mayHaveRun && ep.class == classMoney && !apiErr.orderIDTaken() {
			apiErr.unknown = true
		}
		return nil, apiErr
	}
}

// attempt sends the request once. A returned error is always an *Error.
func (c *core) attempt(ctx context.Context, ep *endpoint, u *url.URL, requestURI string, key secret, body []byte, idemKey string) (*response, *Error) {
	ctx, cancel := context.WithTimeout(ctx, ep.timeout)
	defer cancel()

	// Written headers mean the server may execute the request: only an error before that
	// point proves the request was not sent.
	var sent atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { sent.Store(true) }})

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, ep.method, u.String(), reader)
	if err != nil {
		return nil, &Error{Message: "The request could not be built: " + err.Error() + ".", cause: err}
	}
	req.URL = u
	timestamp := strconv.FormatInt(c.now().Unix()+c.clockSkew.Load(), 10)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("X-Project-Uuid", c.projectID)
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", signRequest(key(), timestamp, c.projectID, ep.method, requestURI, body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(err, sent.Load())
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return nil, transportError(err, true)
	}
	if len(raw) > maxResponseBytes {
		return nil, &Error{Status: res.StatusCode, RequestID: res.Header.Get("X-Request-Id"), Message: "The response is larger than 32 MiB.", unknown: res.StatusCode < 300}
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return parseSuccess(res, raw)
	}
	return nil, mapError(res, raw)
}

func transportError(err error, sent bool) *Error {
	if !sent {
		return &Error{Message: "The request was not sent: " + err.Error() + ".", cause: err}
	}
	return &Error{
		Message: "No response after the request was sent: " + err.Error() + ". The outcome is unknown.",
		cause:   err,
		unknown: true,
		sent:    true,
	}
}

func parseSuccess(res *http.Response, raw []byte) (*response, *Error) {
	requestID := res.Header.Get("X-Request-Id")
	var envelope struct {
		Data json.RawMessage `json:"data"`
		Meta json.RawMessage `json:"meta"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, unreadable(res.StatusCode, requestID, "The response body is not valid JSON: "+err.Error()+".")
	}
	if envelope.Data == nil {
		return nil, unreadable(res.StatusCode, requestID, `The response has no "data" member.`)
	}
	return &response{status: res.StatusCode, requestID: requestID, data: envelope.Data, meta: envelope.Meta}, nil
}

// unreadable reports a 2xx the SDK cannot read: the operation most likely succeeded.
func unreadable(status int, requestID, message string) *Error {
	return &Error{Status: status, RequestID: requestID, Message: message, unknown: true}
}

// retryDelay decides whether a failed attempt is retried, and after which delay.
//
// On money-creating endpoints a business error is never retried, even when the server
// marks it retryable: a failed payout keeps its order_id, so a retry would hide the real
// cause behind a conflict.
func (c *core) retryDelay(ep *endpoint, e *Error, hasKey bool, attempt int, delayLimit time.Duration) (time.Duration, bool) {
	var retry bool
	switch {
	case e.Status == 0: // no response
		retry = !e.sent || ep.class == classRead || (ep.class == classMoney && hasKey)
	case !e.envelope:
		gateway := e.Status >= 502 && e.Status <= 504
		retry = ep.class != classUnsafe && (gateway || e.Status == http.StatusTooManyRequests)
	case ep.class == classRead:
		retry = e.retryable
	case ep.class == classMoney:
		switch e.Code {
		case CodeRateLimitExceeded, CodeIdempotencyInProgress, CodeLockNotAcquired, CodeDependencyUnavailable:
			retry = true
		}
	}
	if !retry {
		return 0, false
	}
	if e.hasRetryAfter {
		d := time.Duration(e.retryAfter) * time.Second
		return d, d <= delayLimit
	}
	return c.backoff(attempt), true
}

// backoff is exponential backoff with full jitter, in milliseconds.
func (c *core) backoff(attempt int) time.Duration {
	limit := math.Min(backoffCap, backoffBase*math.Pow(2, float64(max(0, attempt-1))))
	return time.Duration(math.Round(c.random()*limit*1000)) * time.Millisecond
}

// adjustClock sets the clock offset from a server time. It reports whether the offset
// changed and the request should be signed again.
func (c *core) adjustClock(serverTime int64) bool {
	drift := serverTime - c.now().Unix()
	if drift > -minClockDrift && drift < minClockDrift {
		return false
	}
	offset := min(maxClockDrift, max(-maxClockDrift, drift))
	return c.clockSkew.Swap(offset) != offset
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// signRequest is hex(HMAC-SHA256(key, "{ts}\n{project}\n{METHOD}\n{request URI}\n{body}")).
func signRequest(key, timestamp, projectID, method, requestURI string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp + "\n" + projectID + "\n" + strings.ToUpper(method) + "\n" + requestURI + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// derivedIdempotencyKey is the key of a create request: the same as every official SDK
// derives, so switching SDKs in the middle of retries never creates a duplicate.
func derivedIdempotencyKey(requestURI, orderID string) string {
	sum := sha256.Sum256([]byte(requestURI + "\n" + orderID))
	return derivedKeyPrefix + hex.EncodeToString(sum[:])[:59]
}

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// buildRequestURI builds the path with /api/v4 and the query once: the same string is
// signed and sent. Never a bare "?": some servers drop it and the signature would differ.
func buildRequestURI(ep *endpoint, params []string, query []queryParam) string {
	args := make([]any, len(params))
	for i, p := range params {
		args[i] = rawURLEncode(p)
	}
	uri := apiPrefix + fmt.Sprintf(ep.path, args...)
	var q strings.Builder
	for _, p := range query {
		if p.value == "" {
			continue
		}
		if q.Len() > 0 {
			q.WriteByte('&')
		}
		q.WriteString(rawURLEncode(p.name) + "=" + rawURLEncode(p.value))
	}
	if q.Len() > 0 {
		uri += "?" + q.String()
	}
	return uri
}

// rawURLEncode is RFC 3986 percent-encoding: everything but A-Z a-z 0-9 - _ . ~ is encoded.
func rawURLEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z' || '0' <= ch && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			b.WriteByte(ch)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[ch>>4])
		b.WriteByte(hexDigits[ch&0x0f])
	}
	return b.String()
}

// encodeBody encodes a request body without HTML escaping. An empty body is "{}": the
// API expects a JSON object on every POST.
func encodeBody(v any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("enigmoss: the request body cannot be encoded as JSON: %w", err)
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if string(out) == "null" {
		return []byte("{}"), nil
	}
	return out, nil
}

// errNilParams is returned when a method gets nil parameters.
var errNilParams = errors.New("enigmoss: params are required")
