package enigmoss

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Settings of the API origin fallback, identical in every official SDK.
const (
	fallbackPath      = apiPrefix + "/project/platform-brand"
	fallbackTimeout   = 3 * time.Second    // of the request to the fallback host
	fallbackPause     = 5 * time.Minute    // after a fallback that gave no new origin
	minOriginLifetime = time.Hour          // a new origin is remembered for the refresh_after
	maxOriginLifetime = 7 * 24 * time.Hour // of the response, within these bounds
	responseTolerance = 300                // seconds between X-Response-Timestamp and the clock
	maxOriginLength   = 2048
)

// OriginChange describes a move of the client to another API origin. It is passed to the
// function set with WithOnOriginChange.
type OriginChange struct {
	// Previous is the origin that could not be reached, such as "https://api.example.com".
	Previous string
	// Current is the origin the client sends requests to from now on.
	Current string
	// ExpiresAt is when the client goes back to the base URL it was created with.
	ExpiresAt time.Time
}

// originFallback is the state of the API origin fallback. The zero value is "off".
type originFallback struct {
	origin   string             // of the fallback host; "" when the fallback is off
	onChange func(OriginChange) // nil when not set

	moved       atomic.Pointer[movedOrigin] // the origin learned from the fallback host
	pausedUntil atomic.Int64                // Unix nanoseconds; the fallback host is not asked before

	mu     sync.Mutex
	flight *fallbackFlight // the request to the fallback host in progress, shared by concurrent calls
}

type movedOrigin struct {
	origin  string
	expires time.Time
}

// fallbackFlight is one request to the fallback host. result is set before done is closed.
type fallbackFlight struct {
	done   chan struct{}
	result fallbackResult
}

// fallbackResult is what the fallback host said. The zero value means it could not be
// reached either.
type fallbackResult struct {
	origin   string        // the valid origin it named; "" when it named none
	lifetime time.Duration // how long origin is remembered
	err      *Error        // it answered, but not with a valid origin
}

// activeOrigin is the origin requests go to now: the one learned from the fallback host
// until it expires, the configured one otherwise.
func (c *core) activeOrigin() string {
	if m := c.fb.moved.Load(); m != nil {
		if c.now().Before(m.expires) {
			return m.origin
		}
		c.fb.moved.CompareAndSwap(m, nil)
	}
	return c.origin
}

// fallbackAllowed reports whether the fallback host may be asked after the call failed
// with e: the API host could not be reached, the caller has not given up, the last
// fallback did not fail within the pause and the deadline of the call leaves room for it.
func (c *core) fallbackAllowed(ctx context.Context, e *Error, deadline time.Time) bool {
	if c.fb.origin == "" || !e.unreachable || ctx.Err() != nil {
		return false
	}
	now := c.now()
	return now.UnixNano() >= c.fb.pausedUntil.Load() && deadline.Sub(now) >= fallbackTimeout
}

// fallback asks the fallback host for the API origin after the origin failed could not
// be reached with original. It returns the origin to repeat the request on, or the error
// of the fallback host, or neither when the caller should get its original error.
//
// Concurrent calls share one request to the fallback host and its result.
func (c *core) fallback(ctx context.Context, failed string, original *Error, deadline time.Time) (string, *Error) {
	fb := &c.fb
	fb.mu.Lock()
	if active := c.activeOrigin(); !sameOrigin(active, failed) {
		// Another call has moved the client while this one was retrying.
		fb.mu.Unlock()
		return active, nil
	}
	flight := fb.flight
	if flight == nil {
		if c.now().UnixNano() < fb.pausedUntil.Load() {
			fb.mu.Unlock()
			return "", nil
		}
		flight = &fallbackFlight{done: make(chan struct{})}
		fb.flight = flight
		// The request outlives the call that started it: other calls wait for its result.
		go c.runFallback(context.WithoutCancel(ctx), flight, failed, deadline)
	}
	fb.mu.Unlock()

	select {
	case <-flight.done:
	case <-ctx.Done():
		return "", nil
	}
	res := flight.result
	if res.err != nil {
		// Each caller gets its own copy with its own original error as the cause. Whether
		// the outcome is unknown is a property of the original call: nothing of it was
		// sent in the last attempt, whatever the fallback host answered.
		e := *res.err
		e.cause = original
		e.unknown = original.unknown
		e.hint = strings.TrimSpace(fmt.Sprintf("This is the answer of the fallback API host %s, asked because %s could not be reached. %s", fb.origin, failed, e.hint))
		return "", &e
	}
	if res.origin == "" || sameOrigin(res.origin, failed) {
		return "", nil
	}
	return res.origin, nil
}

// runFallback makes the request of a flight, records the result in the client and
// releases the calls waiting for it.
func (c *core) runFallback(ctx context.Context, flight *fallbackFlight, failed string, deadline time.Time) {
	res := c.askFallback(ctx, deadline)
	now := c.now()
	var change *OriginChange
	if res.err == nil && res.origin != "" && !sameOrigin(res.origin, failed) {
		expires := now.Add(res.lifetime)
		c.fb.moved.Store(&movedOrigin{origin: res.origin, expires: expires})
		c.fb.pausedUntil.Store(0)
		change = &OriginChange{Previous: failed, Current: res.origin, ExpiresAt: expires}
	} else {
		// Only the time is kept, never the answer: a call made during the pause gets its
		// own network error.
		c.fb.pausedUntil.Store(now.Add(fallbackPause).UnixNano())
	}

	c.fb.mu.Lock()
	c.fb.flight = nil
	c.fb.mu.Unlock()
	if change != nil {
		c.originChanged(*change)
	}
	flight.result = res
	close(flight.done)
}

// originChanged calls the hook of WithOnOriginChange. A panic of the hook is dropped: it
// must not break the calls waiting for the new origin.
func (c *core) originChanged(change OriginChange) {
	if c.fb.onChange == nil {
		return
	}
	defer func() { _ = recover() }()
	c.fb.onChange(change)
}

// fallbackRequestURI is the request URI of one request to the fallback host. The nonce,
// 16 random bytes, is new for every request and is part of the signed URI: it makes the
// request signature unique, and the response signature covers the request signature, so
// a response fits one request only. Without it the request signature would depend only
// on the second of the timestamp, and the timestamp may come from an unsigned server_time
// (auth.timestamp_expired): a recorded response could be replayed at its recorded second.
func fallbackRequestURI() string {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	return fallbackPath + "?nonce=" + hex.EncodeToString(nonce[:])
}

// askFallback sends the signed GET /api/v4/project/platform-brand?nonce=... to the
// fallback host, past the retry loop, and reads the origin from a correctly signed response.
func (c *core) askFallback(ctx context.Context, deadline time.Time) (res fallbackResult) {
	// This runs in a goroutine of the SDK: a panic of a custom RoundTripper would end the
	// process there. It is reported as a fallback host that cannot be reached.
	defer func() {
		if recover() != nil {
			res = fallbackResult{}
		}
	}()

	key := c.paymentKey
	if key == nil {
		key = c.payoutKey
	}
	clockCorrected := false
	for {
		requestURI := fallbackRequestURI()
		u, err := requestURL(c.fb.origin, requestURI)
		if err != nil {
			return fallbackResult{}
		}
		x, apiErr := c.exchange(ctx, http.MethodGet, fallbackTimeout, u, requestURI, key, nil, "")
		if apiErr != nil {
			if apiErr.Status == 0 {
				return fallbackResult{}
			}
			return fallbackResult{err: apiErr}
		}
		if x.res.StatusCode >= 200 && x.res.StatusCode < 300 {
			return c.readFallback(x, key)
		}
		apiErr = mapError(x.res, x.raw)
		if apiErr.Code == CodeTimestampExpired && !clockCorrected {
			if serverTime, ok := apiErr.detailInt("server_time"); ok && c.adjustClock(serverTime) && deadline.Sub(c.now()) >= fallbackTimeout {
				clockCorrected = true
				continue
			}
		}
		if apiErr.Code == "http.404" {
			apiErr.hint = "Check the URL passed to enigmoss.WithFallbackBaseURL: the host does not serve this API."
		}
		return fallbackResult{err: apiErr}
	}
}

// readFallback takes the origin from a 2xx response of the fallback host. The origin is
// trusted only when the response is signed with the key of the request.
func (c *core) readFallback(x *exchanged, key secret) fallbackResult {
	rejected := func(message string) fallbackResult {
		return fallbackResult{err: &Error{Status: x.res.StatusCode, RequestID: x.res.Header.Get("X-Request-Id"), Message: message}}
	}
	if !validResponseSignature(key(), x.res.Header, x.signature, x.raw, c.now().Unix()+c.clockSkew.Load()) {
		return rejected("The response has no valid response signature.")
	}
	named, lifetime, ok := parseFallbackResponse(x.raw)
	if !ok {
		return rejected(`The response has no "data.api.origin".`)
	}
	origin, ok := validOrigin(named)
	if !ok {
		if len(named) > 100 {
			named = named[:100] + "..."
		}
		return rejected(fmt.Sprintf("The response names an API origin the SDK does not accept: %q.", named))
	}
	return fallbackResult{origin: origin, lifetime: lifetime}
}

var responseTimestampPattern = regexp.MustCompile(`^[0-9]{1,12}$`)

// validResponseSignature checks the signature of a response:
// X-Response-Signature = hex(HMAC-SHA256(key, "{X-Response-Timestamp}\n{X-Signature of the request}\n{body}")),
// with a timestamp within 300 seconds of now. Covering the request signature makes the
// response single-use: an old response cannot be replayed.
func validResponseSignature(key string, h http.Header, requestSignature string, body []byte, now int64) bool {
	timestamp := h.Get("X-Response-Timestamp")
	if !responseTimestampPattern.MatchString(timestamp) {
		return false
	}
	signedAt, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || signedAt-now > responseTolerance || now-signedAt > responseTolerance {
		return false
	}
	got, err := hex.DecodeString(h.Get("X-Response-Signature"))
	if err != nil || len(got) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp + "\n" + requestSignature + "\n"))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// parseFallbackResponse reads data.api.origin and data.refresh_after. The lifetime is
// refresh_after seconds within [1 hour, 7 days]; one hour when it is absent or not an integer.
func parseFallbackResponse(raw []byte) (origin string, lifetime time.Duration, ok bool) {
	var envelope struct {
		Data struct {
			API struct {
				Origin any `json:"origin"`
			} `json:"api"`
			RefreshAfter any `json:"refresh_after"`
		} `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&envelope) != nil {
		return "", 0, false
	}
	if origin, ok = envelope.Data.API.Origin.(string); !ok {
		return "", 0, false
	}
	lifetime = minOriginLifetime
	if n, isNumber := envelope.Data.RefreshAfter.(json.Number); isNumber {
		if seconds, err := n.Int64(); err == nil {
			seconds = min(max(seconds, int64(minOriginLifetime/time.Second)), int64(maxOriginLifetime/time.Second))
			lifetime = time.Duration(seconds) * time.Second
		}
	}
	return origin, lifetime, true
}

// originPattern: https, a host of at least two labels whose last label starts with a
// letter (so neither an IP literal nor "localhost"), the port empty or 443, and nothing
// else: no credentials, path, query or fragment. It is matched against text in ASCII lower
// case and is deliberately not case-insensitive: (?i) folds by Unicode rules, under which
// U+212A (Kelvin sign) is a "k" and U+017F (long s) is an "s".
var originPattern = regexp.MustCompile(`^https://((?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9])(?::443)?$`)

// validOrigin checks the shape of an origin named by the fallback host and returns it
// normalized: lower case, without the default port. Only ASCII is accepted.
func validOrigin(named string) (string, bool) {
	if len(named) > maxOriginLength {
		return "", false
	}
	lower := []byte(named)
	for i, ch := range lower {
		switch {
		case ch >= 0x80:
			return "", false
		case 'A' <= ch && ch <= 'Z':
			lower[i] = ch + ('a' - 'A')
		}
	}
	m := originPattern.FindSubmatch(lower)
	if m == nil {
		return "", false
	}
	host := string(m[1])
	if len(host) > 253 || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".localhost") {
		return "", false
	}
	return "https://" + host, true
}

// sameOrigin compares two https origins in lower case, ignoring the default port.
func sameOrigin(a, b string) bool {
	return strings.TrimSuffix(a, ":443") == strings.TrimSuffix(b, ":443")
}

// attemptTrace is what httptrace reported about one attempt.
type attemptTrace struct {
	sent      atomic.Bool // the request headers were written
	gotConn   atomic.Bool // a connection was obtained for the request
	dialed    atomic.Bool // a DNS lookup of, or a connect to, the host of the request was started
	tlsFailed atomic.Bool // a TLS handshake failed
}

// hooks returns the trace of an attempt. Without the fallback only the written headers
// are watched, as before.
func (t *attemptTrace) hooks(u *url.URL, fallback bool) *httptrace.ClientTrace {
	trace := &httptrace.ClientTrace{WroteHeaders: func() { t.sent.Store(true) }}
	if !fallback {
		return trace
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(host, port)
	trace.GotConn = func(httptrace.GotConnInfo) { t.gotConn.Store(true) }
	trace.DNSStart = func(info httptrace.DNSStartInfo) {
		if strings.EqualFold(info.Host, host) {
			t.dialed.Store(true)
		}
	}
	trace.ConnectStart = func(_, a string) {
		if strings.EqualFold(a, addr) {
			t.dialed.Store(true)
		}
	}
	trace.TLSHandshakeDone = func(_ tls.ConnectionState, err error) {
		if err != nil {
			t.tlsFailed.Store(true)
		}
	}
	return trace
}

// hostUnreachable reports whether a failed attempt proves that no connection to the API
// host was established: the DNS lookup, the connect or the TLS handshake failed or timed
// out. Only a positive proof counts. A response timeout, a connection lost after the
// request was sent, a cancelled call and a failure to reach a proxy are not such proofs.
func (t *attemptTrace) hostUnreachable(err error, attemptCtx, callerCtx context.Context) bool {
	if t.sent.Load() || callerCtx.Err() != nil || hasNetOp(err, "proxyconnect") {
		return false
	}
	if connectFailure(err) || t.tlsFailed.Load() {
		return true
	}
	// The attempt timed out while connecting: no connection was obtained although the
	// transport did start to dial the host of the request (not a proxy). A transport that
	// reports no trace events never qualifies.
	return errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && !t.gotConn.Load() && t.dialed.Load()
}

// connectFailure reports whether err is, by its type, a failed DNS lookup, connect or
// TLS handshake.
func connectFailure(err error) bool {
	var (
		dnsErr          *net.DNSError
		verificationErr *tls.CertificateVerificationError
		authorityErr    x509.UnknownAuthorityError
		hostnameErr     x509.HostnameError
		invalidErr      x509.CertificateInvalidError
		recordErr       tls.RecordHeaderError
		alertErr        tls.AlertError
	)
	return errors.As(err, &dnsErr) || hasNetOp(err, "dial") ||
		errors.As(err, &verificationErr) || errors.As(err, &authorityErr) || errors.As(err, &hostnameErr) ||
		errors.As(err, &invalidErr) || errors.As(err, &recordErr) || errors.As(err, &alertErr)
}

// hasNetOp reports whether the chain of err has a *net.OpError of the operation op.
func hasNetOp(err error, op string) bool {
	for err != nil {
		var opErr *net.OpError
		if !errors.As(err, &opErr) {
			return false
		}
		if opErr.Op == op {
			return true
		}
		err = opErr.Err
	}
	return false
}
