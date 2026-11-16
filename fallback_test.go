package enigmoss

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	testPrimary  = "https://api.example.com"
	testFallback = "https://api.example.org"
	testMoved    = "https://api2.example.com"
)

// routed is one request seen by the hostRouter.
type routed struct {
	Host   string
	Method string
	URI    string
	Header http.Header
	Body   []byte
}

// routeHandler answers a request; n is the 1-based number of the request to its host.
type routeHandler func(r *http.Request, n int) (*http.Response, error)

// hostRouter is a transport that answers by the host of the request. A host without a
// handler refuses the connection.
type hostRouter struct {
	mu       sync.Mutex
	handlers map[string]routeHandler
	requests []routed
}

func (h *hostRouter) handle(origin string, handler routeHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[strings.TrimPrefix(origin, "https://")] = handler
}

func (h *hostRouter) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	h.mu.Lock()
	h.requests = append(h.requests, routed{Host: r.URL.Host, Method: r.Method, URI: r.URL.RequestURI(), Header: r.Header.Clone(), Body: body})
	n := 0
	for _, seen := range h.requests {
		if seen.Host == r.URL.Host {
			n++
		}
	}
	handler := h.handlers[r.URL.Host]
	h.mu.Unlock()
	if handler == nil {
		return nil, refused()
	}
	return handler(r, n)
}

// to returns the requests sent to an origin.
func (h *hostRouter) to(origin string) []routed {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []routed
	for _, r := range h.requests {
		if r.Host == strings.TrimPrefix(origin, "https://") {
			out = append(out, r)
		}
	}
	return out
}

// fallbackURIPattern is the request URI of a request to the fallback host: the nonce is
// 16 random bytes in lower-case hex.
var fallbackURIPattern = regexp.MustCompile(`^/api/v4/project/platform-brand\?nonce=[0-9a-f]{32}$`)

// refused is the error of the real transport for a refused connection.
func refused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func unreachable(*http.Request, int) (*http.Response, error) { return nil, refused() }

func httpReply(r *http.Request, status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func errorBody(code, details string) string {
	if details == "" {
		details = "{}"
	}
	return `{"error":{"type":"x","code":"` + code + `","message":"Server says ` + code + `.","retryable":false,"details":` + details + `},"meta":{"request_id":"req-fb"}}`
}

// apiOK is a healthy API host.
func apiOK(r *http.Request, _ int) (*http.Response, error) {
	data, status := fxProject, 200
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v4/payouts":
		data, status = fxPayout, 201
	case strings.HasPrefix(r.URL.Path, "/api/v4/payments/"):
		data = fxPayment
	}
	return httpReply(r, status, `{"data":`+data+`,"meta":{"request_id":"req-1"}}`, nil), nil
}

func brandBody(api string) string {
	return `{"data":{"brand":null,"payform":{"origin":"https://pay.example.com"},` + api + `"refresh_after":86400},"meta":{"request_id":"req-fb"}}`
}

func namesOrigin(origin string) string {
	return brandBody(`"api":{"origin":` + strconv.Quote(origin) + `},`)
}

// responseSignature is an independent implementation of the response signature.
func responseSignature(key, timestamp, requestSignature, body string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp + "\n" + requestSignature + "\n" + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// signedReply is a 200 response signed like the API signs the platform brand.
func signedReply(r *http.Request, key, body string, at time.Time) *http.Response {
	ts := strconv.FormatInt(at.Unix(), 10)
	h := http.Header{}
	h.Set("X-Request-Id", "req-fb")
	h.Set("X-Response-Timestamp", ts)
	h.Set("X-Response-Signature", responseSignature(key, ts, r.Header.Get("X-Signature"), body))
	return httpReply(r, 200, body, h)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// fallbackEnv is a client of an API host that refuses connections, with a fallback host
// that names api2.example.com and a healthy API there.
type fallbackEnv struct {
	c      *Client
	router *hostRouter
	clock  *fakeClock

	mu      sync.Mutex
	changes []OriginChange
}

func newFallbackEnv(t *testing.T) *fallbackEnv {
	t.Helper()
	env := &fallbackEnv{
		router: &hostRouter{handlers: map[string]routeHandler{}},
		clock:  &fakeClock{t: time.Unix(1_800_000_000, 0)},
	}
	env.router.handle(testPrimary, unreachable)
	env.router.handle(testFallback, env.names(testMoved))
	env.router.handle(testMoved, apiOK)
	c, err := NewClient(testPrimary, testProject,
		WithPaymentKey(testPaymentKey), WithPayoutKey(testPayoutKey),
		WithHTTPClient(&http.Client{Transport: env.router}),
		WithFallbackBaseURL(testFallback),
		WithOnOriginChange(func(change OriginChange) {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.changes = append(env.changes, change)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	c.c.now = env.clock.now
	c.c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	c.c.random = func() float64 { return 0.5 }
	env.c = c
	return env
}

// names is a fallback host that answers with a correctly signed origin. Like the API, it
// first checks the signature of the request over the request URI with its query.
func (env *fallbackEnv) names(origin string) routeHandler {
	return func(r *http.Request, _ int) (*http.Response, error) {
		if r.Header.Get("X-Signature") != signRequest(testPaymentKey, r.Header.Get("X-Timestamp"), testProject, r.Method, r.URL.RequestURI(), nil) {
			return httpReply(r, 401, errorBody(CodeSignatureInvalid, ""), nil), nil
		}
		return signedReply(r, testPaymentKey, namesOrigin(origin), env.clock.now()), nil
	}
}

func (env *fallbackEnv) moves() []OriginChange {
	env.mu.Lock()
	defer env.mu.Unlock()
	return append([]OriginChange(nil), env.changes...)
}

// counts returns the number of requests to the API host, the fallback host and the new origin.
func (env *fallbackEnv) counts() [3]int {
	return [3]int{len(env.router.to(testPrimary)), len(env.router.to(testFallback)), len(env.router.to(testMoved))}
}

// wantNetworkError checks that err is the original network error of the call, not an
// answer of the fallback host.
func wantNetworkError(t *testing.T, err error) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Status != 0 || OutcomeUnknown(err) || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("want the network error of the call, got %v", err)
	}
	if strings.Contains(err.Error(), "fallback") {
		t.Errorf("the network error mentions the fallback: %v", err)
	}
}

func TestRetryOnTheAPIHostHelpsAndTheFallbackIsNotAsked(t *testing.T) {
	env := newFallbackEnv(t)
	env.router.handle(testPrimary, func(r *http.Request, n int) (*http.Response, error) {
		if n == 1 {
			return nil, refused()
		}
		return apiOK(r, n)
	})
	if _, err := env.c.Payouts.Create(context.Background(), payoutParams); err != nil {
		t.Fatal(err)
	}
	if got := env.counts(); got != [3]int{2, 0, 0} {
		t.Errorf("requests %v", got)
	}
	if env.c.c.activeOrigin() != testPrimary || len(env.moves()) != 0 {
		t.Error("the origin changed")
	}
}

func TestFallbackMovesTheClientToTheNewOrigin(t *testing.T) {
	env := newFallbackEnv(t)
	ctx := context.Background()
	p, err := env.c.Payments.Get(ctx, paymentID)
	if err != nil || p.ID != paymentID {
		t.Fatalf("%+v, %v", p, err)
	}
	if got := env.counts(); got != [3]int{3, 1, 1} {
		t.Fatalf("requests %v: want the retries on the API host, one request to the fallback host, one to the new origin", got)
	}

	probe := env.router.to(testFallback)[0]
	if probe.Method != http.MethodGet || !fallbackURIPattern.MatchString(probe.URI) || len(probe.Body) != 0 {
		t.Errorf("fallback request %s %s %q", probe.Method, probe.URI, probe.Body)
	}
	ts := probe.Header.Get("X-Timestamp")
	if probe.Header.Get("X-Signature") != signRequest(testPaymentKey, ts, testProject, "GET", probe.URI, nil) {
		t.Error("the fallback request is not signed with the payment key")
	}
	if probe.Header.Get("Idempotency-Key") != "" || probe.Header.Get("Content-Type") != "" || probe.Header.Get("X-Project-Uuid") != testProject || !strings.HasPrefix(probe.Header.Get("User-Agent"), "enigmoss-go-sdk/") {
		t.Errorf("fallback request headers %v", probe.Header)
	}

	want := OriginChange{Previous: testPrimary, Current: testMoved, ExpiresAt: env.clock.now().Add(24 * time.Hour)}
	if moves := env.moves(); len(moves) != 1 || moves[0] != want {
		t.Errorf("origin changes %+v, want one %+v", moves, want)
	}
	if !strings.Contains(env.c.String(), testMoved) {
		t.Errorf("String() %s does not show the origin in use", env.c)
	}

	// The next call goes straight to the remembered origin.
	if _, err := env.c.Project.Get(ctx); err != nil {
		t.Fatal(err)
	}
	if got := env.counts(); got != [3]int{3, 1, 2} {
		t.Errorf("requests %v after the second call", got)
	}
	if len(env.moves()) != 1 {
		t.Error("the hook was called again")
	}
}

func TestFallbackRepeatsAMoneyRequestWithTheSameKeyAndBody(t *testing.T) {
	env := newFallbackEnv(t)
	env.router.handle(testFallback, func(r *http.Request, n int) (*http.Response, error) {
		env.clock.advance(7 * time.Second)
		return env.names(testMoved)(r, n)
	})
	// The attempt counter starts again on the new origin: two failures are retried.
	env.router.handle(testMoved, func(r *http.Request, n int) (*http.Response, error) {
		if n < 3 {
			return nil, refused()
		}
		return apiOK(r, n)
	})
	p, err := env.c.Payouts.Create(context.Background(), payoutParams)
	if err != nil || p.OrderID != "withdrawal-42" {
		t.Fatalf("%+v, %v", p, err)
	}
	if got := env.counts(); got != [3]int{3, 1, 3} {
		t.Fatalf("requests %v", got)
	}

	first := env.router.to(testPrimary)[0]
	key := derivedIdempotencyKey("/api/v4/payouts", "withdrawal-42")
	for _, r := range append(env.router.to(testPrimary), env.router.to(testMoved)...) {
		if r.Header.Get("Idempotency-Key") != key || string(r.Body) != string(first.Body) || r.URI != "/api/v4/payouts" {
			t.Errorf("%s: key %q, body %q", r.Host, r.Header.Get("Idempotency-Key"), r.Body)
		}
		if r.Header.Get("X-Signature") != signRequest(testPayoutKey, r.Header.Get("X-Timestamp"), testProject, "POST", r.URI, r.Body) {
			t.Errorf("%s: the signature does not match the timestamp and the body", r.Host)
		}
	}
	replay := env.router.to(testMoved)[0]
	if replay.Header.Get("X-Timestamp") == first.Header.Get("X-Timestamp") || replay.Header.Get("X-Signature") == first.Header.Get("X-Signature") {
		t.Error("the request on the new origin reuses the old timestamp and signature")
	}
}

func TestFallbackHostErrorIsReturnedAndNothingIsRemembered(t *testing.T) {
	signed := func(body string) func(*fallbackEnv) routeHandler {
		return func(env *fallbackEnv) routeHandler {
			return func(r *http.Request, _ int) (*http.Response, error) {
				return signedReply(r, testPaymentKey, body, env.clock.now()), nil
			}
		}
	}
	plain := func(status int, body string) func(*fallbackEnv) routeHandler {
		return func(*fallbackEnv) routeHandler {
			return func(r *http.Request, _ int) (*http.Response, error) {
				return httpReply(r, status, body, nil), nil
			}
		}
	}
	cases := []struct {
		name     string
		fallback func(*fallbackEnv) routeHandler
		status   int
		code     string
		message  string
	}{
		{"404", plain(404, errorBody(CodeResourceNotFound, "")), 404, CodeResourceNotFound, "Server says resource.not_found."},
		{"404 page", plain(404, "<html>"), 404, "http.404", "Check the URL passed to enigmoss.WithFallbackBaseURL"},
		{"401", plain(401, errorBody(CodeSignatureInvalid, "")), 401, CodeSignatureInvalid, ""},
		{"403", plain(403, errorBody(CodeIPForbidden, "")), 403, CodeIPForbidden, ""},
		{"429", plain(429, errorBody(CodeRateLimitExceeded, "")), 429, CodeRateLimitExceeded, ""},
		{"500", plain(500, errorBody(CodeInternalServerError, "")), 500, CodeInternalServerError, ""},
		{"503 page", plain(503, "unavailable"), 503, "http.503", ""},
		{"redirect", plain(302, ""), 302, "http.302", ""},
		{"200 without api", signed(brandBody("")), 200, "", `has no "data.api.origin"`},
		{"200 with api of another shape", signed(brandBody(`"api":"https://api2.example.com",`)), 200, "", `has no "data.api.origin"`},
		{"200 with an origin that is not a string", signed(brandBody(`"api":{"origin":42},`)), 200, "", `has no "data.api.origin"`},
		{"200 that is not JSON", signed("<html>"), 200, "", `has no "data.api.origin"`},
		{"http origin", signed(namesOrigin("http://api2.example.com")), 200, "", "does not accept"},
		{"IP origin", signed(namesOrigin("https://203.0.113.7")), 200, "", "does not accept"},
		{"origin with a port", signed(namesOrigin("https://api2.example.com:8443")), 200, "", "does not accept"},
		{"origin with a path", signed(namesOrigin("https://api2.example.com/api/v4")), 200, "", "does not accept"},
		{"origin with credentials", signed(namesOrigin("https://user:pass@api2.example.com")), 200, "", "does not accept"},
		{"local origin", signed(namesOrigin("https://printer.local")), 200, "", "does not accept"},
		{"origin with a trailing line break", signed(namesOrigin("https://api2.example.com\n")), 200, "", "does not accept"},
		{"origin that is ASCII only after case folding", signed(namesOrigin("https://ban\u212a.example.com")), 200, "", "does not accept"},
		{"signed with another key", func(env *fallbackEnv) routeHandler {
			return func(r *http.Request, _ int) (*http.Response, error) {
				return signedReply(r, "another-key", namesOrigin(testMoved), env.clock.now()), nil
			}
		}, 200, "", "no valid response signature"},
		{"signature of another request", func(env *fallbackEnv) routeHandler {
			return func(r *http.Request, _ int) (*http.Response, error) {
				other := r.Clone(r.Context())
				other.Header.Set("X-Signature", strings.Repeat("ab", 32))
				res := signedReply(other, testPaymentKey, namesOrigin(testMoved), env.clock.now())
				res.Request = r
				return res, nil
			}
		}, 200, "", "no valid response signature"},
		{"no signature", plain(200, namesOrigin(testMoved)), 200, "", "no valid response signature"},
		{"stale signature", func(env *fallbackEnv) routeHandler {
			return func(r *http.Request, _ int) (*http.Response, error) {
				return signedReply(r, testPaymentKey, namesOrigin(testMoved), env.clock.now().Add(-301*time.Second)), nil
			}
		}, 200, "", "no valid response signature"},
		{"signature from the future", func(env *fallbackEnv) routeHandler {
			return func(r *http.Request, _ int) (*http.Response, error) {
				return signedReply(r, testPaymentKey, namesOrigin(testMoved), env.clock.now().Add(301*time.Second)), nil
			}
		}, 200, "", "no valid response signature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newFallbackEnv(t)
			env.router.handle(testFallback, tc.fallback(env))
			ctx := context.Background()

			_, err := env.c.Payouts.Create(ctx, payoutParams)
			var e *Error
			if !errors.As(err, &e) || e.Status != tc.status || e.Code != tc.code {
				t.Fatalf("got %v, want status %d and code %q of the fallback host", err, tc.status, tc.code)
			}
			if !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), "fallback API host "+testFallback) || !strings.Contains(err.Error(), testPrimary+" could not be reached") {
				t.Errorf("message %q", err)
			}
			// The payout request was never sent: whatever the fallback host answered, the
			// outcome is known.
			if OutcomeUnknown(err) || IsOrderIDTaken(err) {
				t.Errorf("OutcomeUnknown %v, IsOrderIDTaken %v", OutcomeUnknown(err), IsOrderIDTaken(err))
			}
			var original *Error
			if !errors.As(errors.Unwrap(err), &original) || original.Status != 0 || !errors.Is(err, syscall.ECONNREFUSED) {
				t.Errorf("the cause %v is not the network error of the call", errors.Unwrap(err))
			}
			if got := env.counts(); got != [3]int{3, 1, 0} {
				t.Errorf("requests %v", got)
			}
			if env.c.c.activeOrigin() != testPrimary || env.c.c.fb.moved.Load() != nil || len(env.moves()) != 0 {
				t.Error("an origin was remembered")
			}

			// During the pause the fallback host is not asked: the call gets its own error.
			_, err = env.c.Payouts.Create(ctx, payoutParams)
			wantNetworkError(t, err)
			if got := env.counts(); got != [3]int{6, 1, 0} {
				t.Errorf("requests %v during the pause", got)
			}

			// After the pause it is asked again.
			env.clock.advance(fallbackPause)
			_, err = env.c.Payouts.Create(ctx, payoutParams)
			if !errors.As(err, &e) || e.Status != tc.status {
				t.Errorf("after the pause: %v", err)
			}
			if got := env.counts(); got != [3]int{9, 2, 0} {
				t.Fatalf("requests %v after the pause", got)
			}
			// Every request to the fallback host has its own nonce, so its own signature.
			first, second := env.router.to(testFallback)[0], env.router.to(testFallback)[1]
			if !fallbackURIPattern.MatchString(first.URI) || !fallbackURIPattern.MatchString(second.URI) || first.URI == second.URI {
				t.Errorf("fallback requests %s and %s", first.URI, second.URI)
			}
		})
	}
}

func TestFallbackNamesTheSameOrigin(t *testing.T) {
	for _, named := range []string{testPrimary, "HTTPS://API.Example.com:443"} {
		t.Run(named, func(t *testing.T) {
			env := newFallbackEnv(t)
			env.router.handle(testFallback, env.names(named))
			ctx := context.Background()
			_, err := env.c.Payouts.Create(ctx, payoutParams)
			wantNetworkError(t, err)
			if got := env.counts(); got != [3]int{3, 1, 0} {
				t.Errorf("requests %v", got)
			}
			if env.c.c.fb.moved.Load() != nil || len(env.moves()) != 0 {
				t.Error("an origin was remembered")
			}
			_, err = env.c.Payouts.Create(ctx, payoutParams)
			wantNetworkError(t, err)
			if got := env.counts(); got != [3]int{6, 1, 0} {
				t.Errorf("requests %v: the fallback host was asked again during the pause", got)
			}
		})
	}
}

func TestFallbackHostIsUnreachableToo(t *testing.T) {
	failures := map[string]error{
		"refused":  refused(),
		"timeout":  context.DeadlineExceeded,
		"reset":    &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		"panicked": nil,
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			env := newFallbackEnv(t)
			env.router.handle(testFallback, func(*http.Request, int) (*http.Response, error) {
				if failure == nil {
					panic("the transport panicked")
				}
				return nil, failure
			})
			ctx := context.Background()
			_, err := env.c.Payments.Get(ctx, paymentID)
			wantNetworkError(t, err)
			_, err = env.c.Payments.Get(ctx, paymentID)
			wantNetworkError(t, err)
			if got := env.counts(); got != [3]int{6, 1, 0} {
				t.Errorf("requests %v", got)
			}
		})
	}
}

func TestOneFallbackPerCall(t *testing.T) {
	env := newFallbackEnv(t)
	env.router.handle(testMoved, unreachable)
	ctx := context.Background()

	// The new origin cannot be reached either: its error is returned, the fallback host is
	// not asked a second time in the same call.
	_, err := env.c.Payouts.Create(ctx, payoutParams)
	wantNetworkError(t, err)
	if got := env.counts(); got != [3]int{3, 1, 3} {
		t.Fatalf("requests %v", got)
	}
	if env.c.c.activeOrigin() != testMoved {
		t.Error("the new origin was forgotten")
	}

	// The next call asks again, hears the same origin and gets its own error.
	_, err = env.c.Payouts.Create(ctx, payoutParams)
	wantNetworkError(t, err)
	if got := env.counts(); got != [3]int{3, 2, 6} {
		t.Errorf("requests %v", got)
	}
	_, err = env.c.Payouts.Create(ctx, payoutParams)
	wantNetworkError(t, err)
	if got := env.counts(); got != [3]int{3, 2, 9} {
		t.Errorf("requests %v: the fallback host was asked during the pause", got)
	}
}

func TestFallbackOutcomeFollowsTheOriginalCall(t *testing.T) {
	env := newFallbackEnv(t)
	// The first attempt was sent and lost, the retries could not connect.
	env.router.handle(testPrimary, func(r *http.Request, n int) (*http.Response, error) {
		if n == 1 {
			httptrace.ContextClientTrace(r.Context()).WroteHeaders()
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
		}
		return nil, refused()
	})
	env.router.handle(testFallback, func(r *http.Request, _ int) (*http.Response, error) {
		return httpReply(r, 404, errorBody(CodeResourceNotFound, ""), nil), nil
	})
	_, err := env.c.Payouts.Create(context.Background(), payoutParams)
	var e *Error
	if !errors.As(err, &e) || e.Status != 404 {
		t.Fatalf("err %v", err)
	}
	if !OutcomeUnknown(err) {
		t.Error("the first attempt may have run: the outcome is unknown whatever the fallback host answered")
	}
}

func TestOriginExpiresAndThePauseEnds(t *testing.T) {
	env := newFallbackEnv(t)
	ctx := context.Background()
	if _, err := env.c.Project.Get(ctx); err != nil {
		t.Fatal(err)
	}
	env.clock.advance(24*time.Hour - time.Second)
	if env.c.c.activeOrigin() != testMoved {
		t.Fatal("the origin expired too early")
	}
	env.clock.advance(time.Second)
	if env.c.c.activeOrigin() != testPrimary || !strings.Contains(env.c.String(), testPrimary) {
		t.Fatal("the origin did not expire")
	}

	// The API host is still down: the fallback host is asked again.
	if _, err := env.c.Project.Get(ctx); err != nil {
		t.Fatal(err)
	}
	if got := env.counts(); got != [3]int{6, 2, 2} {
		t.Errorf("requests %v", got)
	}
	if len(env.moves()) != 2 {
		t.Errorf("%d origin changes", len(env.moves()))
	}

	// The API host is back by the time the origin expires again.
	env.clock.advance(24 * time.Hour)
	env.router.handle(testPrimary, apiOK)
	if _, err := env.c.Project.Get(ctx); err != nil {
		t.Fatal(err)
	}
	if got := env.counts(); got != [3]int{7, 2, 2} {
		t.Errorf("requests %v", got)
	}
}

func TestFallbackCorrectsTheClockOnce(t *testing.T) {
	env := newFallbackEnv(t)
	var timestamps []string
	env.router.handle(testFallback, func(r *http.Request, n int) (*http.Response, error) {
		timestamps = append(timestamps, r.Header.Get("X-Timestamp"))
		serverTime := env.clock.now().Add(time.Hour)
		if n == 1 {
			return httpReply(r, 401, errorBody(CodeTimestampExpired, fmt.Sprintf(`{"server_time":%d}`, serverTime.Unix())), nil), nil
		}
		return signedReply(r, testPaymentKey, namesOrigin(testMoved), serverTime), nil
	})
	if _, err := env.c.Project.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := env.clock.now().Unix()
	want := []string{strconv.FormatInt(now, 10), strconv.FormatInt(now+3600, 10)}
	if fmt.Sprint(timestamps) != fmt.Sprint(want) {
		t.Errorf("timestamps %v, want %v", timestamps, want)
	}
	// The repeat is a new request: a new nonce and a new signature over it.
	first, repeat := env.router.to(testFallback)[0], env.router.to(testFallback)[1]
	if !fallbackURIPattern.MatchString(repeat.URI) || repeat.URI == first.URI || repeat.Header.Get("X-Signature") == first.Header.Get("X-Signature") {
		t.Errorf("the repeat %s reuses the nonce or the signature of %s", repeat.URI, first.URI)
	}
	for _, r := range []routed{first, repeat} {
		if r.Header.Get("X-Signature") != signRequest(testPaymentKey, r.Header.Get("X-Timestamp"), testProject, "GET", r.URI, nil) {
			t.Errorf("%s: the signature does not cover the request URI with the nonce", r.URI)
		}
	}
	if env.c.c.clockSkew.Load() != 3600 || env.c.c.activeOrigin() != testMoved {
		t.Errorf("clock offset %d, origin %s", env.c.c.clockSkew.Load(), env.c.c.activeOrigin())
	}

	// A second expired timestamp is the error of the fallback host.
	env2 := newFallbackEnv(t)
	env2.router.handle(testFallback, func(r *http.Request, n int) (*http.Response, error) {
		return httpReply(r, 401, errorBody(CodeTimestampExpired, fmt.Sprintf(`{"server_time":%d}`, env2.clock.now().Unix()+int64(n)*3600)), nil), nil
	})
	_, err := env2.c.Project.Get(context.Background())
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeTimestampExpired || len(env2.router.to(testFallback)) != 2 {
		t.Errorf("err %v after %d requests to the fallback host", err, len(env2.router.to(testFallback)))
	}
}

// TestRecordedFallbackResponseCannotBeReplayed: whoever answers on the fallback host and
// once recorded a genuine signed response cannot replay it. The server_time of
// auth.timestamp_expired is not signed, so the client can be sent back to the recorded
// second; the nonce still makes the signature of the new request, which the response
// signature covers, differ from the recorded one.
func TestRecordedFallbackResponseCannotBeReplayed(t *testing.T) {
	env := newFallbackEnv(t)
	ctx := context.Background()
	recordedAt := env.clock.now()
	var recording *http.Response
	env.router.handle(testFallback, func(r *http.Request, n int) (*http.Response, error) {
		switch n {
		case 1: // the genuine response, recorded on its way
			recording = signedReply(r, testPaymentKey, namesOrigin(testMoved), env.clock.now())
			return signedReply(r, testPaymentKey, namesOrigin(testMoved), env.clock.now()), nil
		case 2: // a day later: send the clock of the client back to the recorded second
			return httpReply(r, 401, errorBody(CodeTimestampExpired, fmt.Sprintf(`{"server_time":%d}`, recordedAt.Unix())), nil), nil
		default: // and replay the recording
			return httpReply(r, 200, namesOrigin(testMoved), recording.Header.Clone()), nil
		}
	})
	if _, err := env.c.Project.Get(ctx); err != nil {
		t.Fatal(err)
	}
	env.clock.advance(24 * time.Hour) // the remembered origin has expired, the API host is still down

	_, err := env.c.Project.Get(ctx)
	var e *Error
	if !errors.As(err, &e) || e.Status != 200 || !strings.Contains(err.Error(), "no valid response signature") {
		t.Fatalf("the replayed response was not rejected: %v", err)
	}
	probes := env.router.to(testFallback)
	if len(probes) != 3 {
		t.Fatalf("%d requests to the fallback host", len(probes))
	}
	recorded, replayed := probes[0], probes[2]
	if replayed.Header.Get("X-Timestamp") != recorded.Header.Get("X-Timestamp") {
		t.Fatalf("timestamps %s and %s: the test did not bring the client back to the recorded second", recorded.Header.Get("X-Timestamp"), replayed.Header.Get("X-Timestamp"))
	}
	if replayed.URI == recorded.URI || replayed.Header.Get("X-Signature") == recorded.Header.Get("X-Signature") {
		t.Error("two requests of the same second have the same nonce and signature")
	}
	if env.c.c.activeOrigin() != testPrimary || len(env.moves()) != 1 {
		t.Errorf("origin %s after %d changes: the replay moved the client", env.c.c.activeOrigin(), len(env.moves()))
	}
}

func TestFallbackNeedsAProofThatTheHostIsUnreachable(t *testing.T) {
	sent := func(err error) routeHandler {
		return func(r *http.Request, _ int) (*http.Response, error) {
			httptrace.ContextClientTrace(r.Context()).WroteHeaders()
			return nil, err
		}
	}
	notSentBy := func(err error) routeHandler {
		return func(*http.Request, int) (*http.Response, error) { return nil, err }
	}
	cases := map[string]routeHandler{
		"response timeout":               sent(context.DeadlineExceeded),
		"connection lost after sending":  sent(&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}),
		"refused after the headers went": sent(refused()),
		"error without a type":           notSentBy(errors.New("dial tcp: connection refused")),
		"timeout without a trace":        notSentBy(context.DeadlineExceeded),
		"proxy cannot be reached":        notSentBy(&net.OpError{Op: "proxyconnect", Net: "tcp", Err: refused()}),
		"proxy name cannot be resolved":  notSentBy(&net.OpError{Op: "proxyconnect", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "proxy.example.com", IsNotFound: true}}),
		"gateway page": func(r *http.Request, _ int) (*http.Response, error) {
			return httpReply(r, 503, "unavailable", nil), nil
		},
		"API error": func(r *http.Request, _ int) (*http.Response, error) {
			return httpReply(r, 500, errorBody(CodeInternalServerError, ""), nil), nil
		},
		"connection reset before sending": notSentBy(&net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}),
	}
	for name, primary := range cases {
		t.Run(name, func(t *testing.T) {
			env := newFallbackEnv(t)
			env.router.handle(testPrimary, primary)
			_, err := env.c.Payments.Get(context.Background(), paymentID)
			if err == nil {
				t.Fatal("no error")
			}
			if got := env.counts(); got[1] != 0 || got[2] != 0 {
				t.Errorf("requests %v: the fallback host was asked", got)
			}
			if strings.Contains(err.Error(), "fallback") || env.c.c.fb.pausedUntil.Load() != 0 {
				t.Errorf("err %v, pause %d", err, env.c.c.fb.pausedUntil.Load())
			}
		})
	}

	for name, failure := range map[string]error{
		"name not resolved":   &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "api.example.com", IsNotFound: true}},
		"connect timeout":     &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded},
		"certificate":         &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
		"not a TLS handshake": tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
		"handshake alert":     &net.OpError{Op: "remote error", Err: tls.AlertError(40)},
	} {
		t.Run(name, func(t *testing.T) {
			env := newFallbackEnv(t)
			env.router.handle(testPrimary, notSentBy(failure))
			if _, err := env.c.Payments.Get(context.Background(), paymentID); err != nil {
				t.Fatal(err)
			}
			if got := env.counts(); got != [3]int{3, 1, 1} {
				t.Errorf("requests %v", got)
			}
		})
	}
}

func TestCancelledContextDoesNotStartTheFallback(t *testing.T) {
	env := newFallbackEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	env.c.c.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	_, err := env.c.Payments.Get(ctx, paymentID)
	wantNetworkError(t, err)
	if got := env.counts(); got != [3]int{1, 0, 0} {
		t.Errorf("requests %v", got)
	}

	// Cancelled before the first attempt.
	env2 := newFallbackEnv(t)
	_, err = env2.c.Payments.Get(ctx, paymentID)
	if err == nil || len(env2.router.to(testFallback)) != 0 {
		t.Errorf("err %v, %d requests to the fallback host", err, len(env2.router.to(testFallback)))
	}
}

func TestCallerCancelledWhileTheFallbackHostIsAsked(t *testing.T) {
	env := newFallbackEnv(t)
	asked, release := make(chan struct{}), make(chan struct{})
	env.router.handle(testFallback, func(r *http.Request, n int) (*http.Response, error) {
		close(asked)
		<-release
		return env.names(testMoved)(r, n)
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := env.c.Payments.Get(ctx, paymentID)
		result <- err
	}()
	<-asked
	cancel()
	select {
	case err := <-result:
		// The caller gets its own error at once; the request to the fallback host goes on.
		wantNetworkError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled call still waits for the fallback host")
	}
	if got := env.counts(); got != [3]int{3, 1, 0} {
		t.Errorf("requests %v", got)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for len(env.moves()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the answer of the fallback host was dropped with its caller")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := env.c.Payments.Get(context.Background(), paymentID); err != nil {
		t.Fatal(err)
	}
	if got := env.counts(); got != [3]int{3, 1, 1} {
		t.Errorf("requests %v", got)
	}
}

func TestConcurrentCallsShareOneFallbackRequest(t *testing.T) {
	const calls = 24
	for _, healthy := range []bool{true, false} {
		t.Run(fmt.Sprintf("fallback answers with an origin: %v", healthy), func(t *testing.T) {
			env := newFallbackEnv(t)
			var attempts atomic.Int32
			env.router.handle(testPrimary, func(*http.Request, int) (*http.Response, error) {
				attempts.Add(1)
				return nil, refused()
			})
			env.router.handle(testFallback, func(r *http.Request, n int) (*http.Response, error) {
				// Answer only when every call has used up its retries and waits.
				for deadline := time.Now().Add(2 * time.Second); attempts.Load() < 3*calls && time.Now().Before(deadline); {
					time.Sleep(time.Millisecond)
				}
				time.Sleep(20 * time.Millisecond)
				if healthy {
					return env.names(testMoved)(r, n)
				}
				return httpReply(r, 404, errorBody(CodeResourceNotFound, ""), nil), nil
			})

			errs := make([]error, calls)
			var wg sync.WaitGroup
			for i := range calls {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, errs[i] = env.c.Payments.Get(context.Background(), paymentID)
				}()
			}
			wg.Wait()

			if n := len(env.router.to(testFallback)); n != 1 {
				t.Fatalf("%d requests to the fallback host, want 1", n)
			}
			shared := 0
			for _, err := range errs {
				var e *Error
				switch {
				case healthy && err != nil:
					t.Errorf("err %v", err)
				case healthy:
				case errors.As(err, &e) && e.Status == 404:
					// Every waiting call gets the answer with its own network error as the cause.
					shared++
					var original *Error
					if !errors.As(errors.Unwrap(err), &original) || original.Status != 0 {
						t.Errorf("cause %v", errors.Unwrap(err))
					}
				default:
					// A call that came after the answer is in the pause.
					wantNetworkError(t, err)
				}
			}
			if healthy {
				if got := env.counts(); got != [3]int{3 * calls, 1, calls} || len(env.moves()) != 1 {
					t.Errorf("requests %v, %d origin changes", got, len(env.moves()))
				}
			} else if shared == 0 {
				t.Error("no call got the answer of the fallback host")
			}
		})
	}
}

func TestFallbackIsSkippedWhenTheDeadlineIsNear(t *testing.T) {
	env := newFallbackEnv(t)
	// The call may take 30 s * 3 + 10 s * 2 = 110 s; after 108 s there is no room for the
	// 3 s of the fallback request.
	env.router.handle(testPrimary, func(_ *http.Request, n int) (*http.Response, error) {
		if n == 1 {
			env.clock.advance(108 * time.Second)
		}
		return nil, refused()
	})
	_, err := env.c.Payments.Get(context.Background(), paymentID)
	wantNetworkError(t, err)
	if got := env.counts(); got != [3]int{3, 0, 0} {
		t.Errorf("requests %v", got)
	}
	if env.c.c.fb.pausedUntil.Load() != 0 {
		t.Error("a skipped fallback started the pause")
	}
}

func TestOriginChangeHookMayPanic(t *testing.T) {
	router := &hostRouter{handlers: map[string]routeHandler{}}
	router.handle(testFallback, func(r *http.Request, _ int) (*http.Response, error) {
		return signedReply(r, testPayoutKey, namesOrigin(testMoved), time.Now()), nil
	})
	router.handle(testMoved, apiOK)
	c, err := NewClient(testPrimary, testProject, WithPayoutKey(testPayoutKey), WithHTTPClient(&http.Client{Transport: router}),
		WithFallbackBaseURL(testFallback+"/api/v4/"), WithOnOriginChange(func(OriginChange) { panic("hook") }))
	if err != nil {
		t.Fatal(err)
	}
	c.c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	// Only the payout key is set: it signs the request to the fallback host.
	if _, err := c.Payouts.Create(context.Background(), payoutParams); err != nil {
		t.Fatal(err)
	}
	probe := router.to(testFallback)[0]
	if probe.Header.Get("X-Signature") != signRequest(testPayoutKey, probe.Header.Get("X-Timestamp"), testProject, "GET", probe.URI, nil) {
		t.Error("the fallback request is not signed with the payout key")
	}
}

func TestWithoutTheFallbackNothingChanges(t *testing.T) {
	router := &hostRouter{handlers: map[string]routeHandler{}}
	router.handle(testFallback, apiOK)
	c, err := NewClient(testPrimary, testProject, WithPaymentKey(testPaymentKey), WithHTTPClient(&http.Client{Transport: router}))
	if err != nil {
		t.Fatal(err)
	}
	c.c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	_, err = c.Payments.Get(context.Background(), paymentID)
	wantNetworkError(t, err)
	var e *Error
	if !errors.As(err, &e) || e.unreachable {
		t.Errorf("err %#v", e)
	}
	if len(router.to(testPrimary)) != 3 || len(router.to(testFallback)) != 0 {
		t.Errorf("%d requests to the API host, %d to another host", len(router.to(testPrimary)), len(router.to(testFallback)))
	}
}

func TestFallbackOptions(t *testing.T) {
	key := WithPaymentKey("k")
	bad := map[string]Option{
		"http":        WithFallbackBaseURL("http://api.example.org"),
		"path":        WithFallbackBaseURL("https://api.example.org/v4"),
		"query":       WithFallbackBaseURL("https://api.example.org/?a=1"),
		"credentials": WithFallbackBaseURL("https://u:p@api.example.org"),
		"relative":    WithFallbackBaseURL("api.example.org"),
		"nil hook":    WithOnOriginChange(nil),
	}
	for name, opt := range bad {
		_, err := NewClient(testPrimary, testProject, key, opt)
		if err == nil || !strings.HasPrefix(err.Error(), "enigmoss: With") {
			t.Errorf("%s: err %v", name, err)
		}
	}
	c, err := NewClient(testPrimary, testProject, key, WithFallbackBaseURL(" https://API.example.org/api/v4/ "))
	if err != nil || c.c.fb.origin != testFallback {
		t.Errorf("origin %q, err %v", c.c.fb.origin, err)
	}
	c, err = NewClient(testPrimary, testProject, key)
	if err != nil || c.c.fb.origin != "" {
		t.Errorf("the fallback is on by default: %q, %v", c.c.fb.origin, err)
	}
	// An empty URL, as from an unset environment variable, leaves the fallback off.
	for _, empty := range []string{"", "  ", "\t\n"} {
		c, err = NewClient(testPrimary, testProject, key, WithFallbackBaseURL(empty))
		if err != nil || c.c.fb.origin != "" {
			t.Errorf("%q: origin %q, err %v", empty, c.c.fb.origin, err)
		}
	}
	// The last option wins, an empty one included.
	c, err = NewClient(testPrimary, testProject, key, WithFallbackBaseURL(testFallback), WithFallbackBaseURL(""))
	if err != nil || c.c.fb.origin != "" {
		t.Errorf("origin %q, err %v", c.c.fb.origin, err)
	}
}

func TestEmptyFallbackURLLeavesTheFallbackOff(t *testing.T) {
	router := &hostRouter{handlers: map[string]routeHandler{}}
	c, err := NewClient(testPrimary, testProject, WithPaymentKey(testPaymentKey), WithHTTPClient(&http.Client{Transport: router}), WithFallbackBaseURL(""))
	if err != nil {
		t.Fatal(err)
	}
	c.c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	_, err = c.Payments.Get(context.Background(), paymentID)
	wantNetworkError(t, err)
	var e *Error
	if !errors.As(err, &e) || e.unreachable || len(router.requests) != 3 {
		t.Errorf("err %#v after %d requests", e, len(router.requests))
	}
}

func TestValidOrigin(t *testing.T) {
	valid := map[string]string{
		"https://api2.example.com":                            "https://api2.example.com",
		"HTTPS://API2.Example.COM":                            "https://api2.example.com",
		"https://api2.example.com:443":                        "https://api2.example.com",
		"https://example.com":                                 "https://example.com",
		"https://a-b.c1.example.org":                          "https://a-b.c1.example.org",
		"https://api2.xn--kgbechtv":                           "https://api2.xn--kgbechtv",
		"https://" + strings.Repeat("a", 63) + ".example.com": "https://" + strings.Repeat("a", 63) + ".example.com",
	}
	for named, want := range valid {
		if got, ok := validOrigin(named); !ok || got != want {
			t.Errorf("%q: %q, %v", named, got, ok)
		}
	}
	invalid := []string{
		"", "api2.example.com", "//api2.example.com", "http://api2.example.com", "ftp://api2.example.com",
		"https://", "https://localhost", "https://example", "https://printer.local", "https://dev.localhost", "https://API.LOCAL",
		"https://203.0.113.7", "https://203.0.113.7:443", "https://[::1]", "https://[2001:db8::1]:443", "https://0x7f.0.0.1", "https://2130706433",
		"https://api2.example.com:8443", "https://api2.example.com:", "https://api2.example.com:80", "https://api2.example.com:0443",
		"https://api2.example.com/", "https://api2.example.com/api/v4", "https://api2.example.com?x=1", "https://api2.example.com#x",
		"https://user@api2.example.com", "https://user:pass@api2.example.com", "https://api2.example.com@evil.example.org/",
		"https://api2.example.com.", "https://.example.com", "https://api2..example.com", "https://-api.example.com", "https://api-.example.com",
		"https://api_2.example.com", "https://api2.example.c", "https://api2.example.1com", "https://ап.example.com",
		// (?i) would fold these to ASCII: U+212A (Kelvin sign) to "k", U+017F (long s) to "s".
		"https://ban\u212a.example.com", "https://api2.example.\u212aitchen", "https://\u017fhop.example.com", "http\u017f://api2.example.com", "HTTP\u017F://API2.EXAMPLE.COM",
		" https://api2.example.com", "https://api2.example.com ", "https://api2.example.com\n", "https://api2.example.com\nhttps://x.example.com",
		"https://api2.example.com\\@evil.example.org", "https://" + strings.Repeat("a", 64) + ".example.com",
		"https://" + strings.Repeat("a.", 127) + "example.com", "https://" + strings.Repeat("a", 2100) + ".example.com",
		"http://localhost", "http://127.0.0.1:8080",
	}
	for _, named := range invalid {
		if got, ok := validOrigin(named); ok {
			t.Errorf("%q is accepted as %q", named, got)
		}
	}
	if !sameOrigin("https://api.example.com:443", "https://api.example.com") || sameOrigin("https://api.example.com:8443", "https://api.example.com") {
		t.Error("sameOrigin")
	}
}

func TestOriginLifetime(t *testing.T) {
	for refreshAfter, want := range map[string]time.Duration{
		`"refresh_after":86400`:                24 * time.Hour,
		`"refresh_after":60`:                   time.Hour,
		`"refresh_after":0`:                    time.Hour,
		`"refresh_after":-9223372036854775808`: time.Hour,
		`"refresh_after":604800`:               7 * 24 * time.Hour,
		`"refresh_after":604801`:               7 * 24 * time.Hour,
		`"refresh_after":9223372036854775807`:  7 * 24 * time.Hour,
		`"refresh_after":99999999999999999999`: time.Hour,
		`"refresh_after":7200.5`:               time.Hour,
		`"refresh_after":"86400"`:              time.Hour,
		`"refresh_after":null`:                 time.Hour,
		`"x":1`:                                time.Hour,
	} {
		origin, lifetime, ok := parseFallbackResponse([]byte(`{"data":{"api":{"origin":"https://api2.example.com"},` + refreshAfter + `}}`))
		if !ok || origin != testMoved || lifetime != want {
			t.Errorf("%s: %q, %v, %v; want %v", refreshAfter, origin, lifetime, ok, want)
		}
	}
}

func TestResponseSignature(t *testing.T) {
	const now = 1_800_000_000
	body := []byte(namesOrigin(testMoved))
	requestSignature := strings.Repeat("0f", 32)
	header := func(ts, signature string) http.Header {
		h := http.Header{}
		h.Set("X-Response-Timestamp", ts)
		h.Set("X-Response-Signature", signature)
		return h
	}
	ts := strconv.Itoa(now)
	good := responseSignature("key", ts, requestSignature, string(body))

	// A fixed vector of the signed string "{timestamp}\n{request signature}\n{body}",
	// computed outside Go.
	vectorBody := []byte(`{"data":{"api":{"origin":"https://api2.example.com"},"refresh_after":86400}}`)
	if !validResponseSignature(testPaymentKey, header(ts, "757cab8492a20829e3d7e1fbe65889dc0be9059f5d3ef15639a80e148d98e9d8"), requestSignature, vectorBody, now) {
		t.Error("the fixed vector is rejected")
	}
	if !validResponseSignature("key", header(ts, good), requestSignature, body, now) {
		t.Error("a valid signature is rejected")
	}
	if !validResponseSignature("key", header(ts, strings.ToUpper(good)), requestSignature, body, now) {
		t.Error("an upper-case signature is rejected")
	}
	for _, edge := range []int{now - 300, now + 300} {
		ts := strconv.Itoa(edge)
		if !validResponseSignature("key", header(ts, responseSignature("key", ts, requestSignature, string(body))), requestSignature, body, now) {
			t.Errorf("timestamp %d is rejected", edge)
		}
	}
	bad := map[string]bool{
		"another key":         validResponseSignature("other", header(ts, good), requestSignature, body, now),
		"another request":     validResponseSignature("key", header(ts, good), strings.Repeat("0e", 32), body, now),
		"another body":        validResponseSignature("key", header(ts, good), requestSignature, append([]byte(" "), body...), now),
		"another timestamp":   validResponseSignature("key", header(strconv.Itoa(now+1), good), requestSignature, body, now),
		"no timestamp":        validResponseSignature("key", header("", good), requestSignature, body, now),
		"timestamp with sign": validResponseSignature("key", header("+"+ts, responseSignature("key", "+"+ts, requestSignature, string(body))), requestSignature, body, now),
		"no signature":        validResponseSignature("key", header(ts, ""), requestSignature, body, now),
		"not hex":             validResponseSignature("key", header(ts, strings.Repeat("zz", 32)), requestSignature, body, now),
		"truncated":           validResponseSignature("key", header(ts, good[:32]), requestSignature, body, now),
	}
	for _, edge := range []int{now - 301, now + 301} {
		ts := strconv.Itoa(edge)
		bad[ts] = validResponseSignature("key", header(ts, responseSignature("key", ts, requestSignature, string(body))), requestSignature, body, now)
	}
	for name, accepted := range bad {
		if accepted {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHostUnreachable(t *testing.T) {
	alive := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer stop()

	dial := &url.Error{Op: "Get", URL: testPrimary, Err: refused()}
	type flags struct{ sent, gotConn, dialed, tlsFailed bool }
	cases := []struct {
		name       string
		err        error
		flags      flags
		attemptCtx context.Context
		callerCtx  context.Context
		want       bool
	}{
		{"refused", dial, flags{dialed: true}, alive, alive, true},
		{"refused, transport without a trace", dial, flags{}, alive, alive, true},
		{"name not resolved", &url.Error{Op: "Get", URL: testPrimary, Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}}, flags{dialed: true}, alive, alive, true},
		{"bare DNS error", &net.DNSError{Err: "server misbehaving"}, flags{}, alive, alive, true},
		{"certificate of another host", &url.Error{Op: "Get", URL: testPrimary, Err: &tls.CertificateVerificationError{Err: x509.HostnameError{Host: "api.example.com"}}}, flags{dialed: true, tlsFailed: true}, alive, alive, true},
		{"unknown authority", x509.UnknownAuthorityError{}, flags{}, alive, alive, true},
		{"invalid certificate", x509.CertificateInvalidError{Reason: x509.Expired}, flags{}, alive, alive, true},
		{"handshake reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, flags{dialed: true, tlsFailed: true}, alive, alive, true},
		{"handshake timeout of the transport", errors.New("net/http: TLS handshake timeout"), flags{dialed: true, tlsFailed: true}, alive, alive, true},
		{"attempt timed out while connecting", context.DeadlineExceeded, flags{dialed: true}, expired, alive, true},
		{"attempt timed out, no trace", context.DeadlineExceeded, flags{}, expired, alive, false},
		{"attempt timed out waiting for the response", context.DeadlineExceeded, flags{sent: true, gotConn: true, dialed: true}, expired, alive, false},
		{"attempt timed out with a connection", context.DeadlineExceeded, flags{gotConn: true, dialed: true}, expired, alive, false},
		{"refused after the headers were written", dial, flags{sent: true}, alive, alive, false},
		{"caller cancelled", dial, flags{dialed: true}, alive, cancelled, false},
		{"caller deadline", context.DeadlineExceeded, flags{dialed: true}, expired, expired, false},
		{"proxy refused", &net.OpError{Op: "proxyconnect", Net: "tcp", Err: refused()}, flags{}, alive, alive, false},
		{"proxy handshake failed", &net.OpError{Op: "proxyconnect", Net: "tcp", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, flags{tlsFailed: true}, alive, alive, false},
		{"connection lost", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, flags{gotConn: true}, alive, alive, false},
		{"broken pipe before the headers", &net.OpError{Op: "write", Err: syscall.EPIPE}, flags{gotConn: true}, alive, alive, false},
		{"error without a type", errors.New("dial tcp: connection refused"), flags{}, alive, alive, false},
	}
	for _, tc := range cases {
		var trace attemptTrace
		trace.sent.Store(tc.flags.sent)
		trace.gotConn.Store(tc.flags.gotConn)
		trace.dialed.Store(tc.flags.dialed)
		trace.tlsFailed.Store(tc.flags.tlsFailed)
		if got := trace.hostUnreachable(tc.err, tc.attemptCtx, tc.callerCtx); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// closedAddr returns the address of a port nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// realFallback is a fallback host and a new API origin on real HTTPS servers. The
// certificate of httptest is valid for example.com: the client reaches the new origin
// https://example.com through a dialer that maps host names to the test servers.
type realFallback struct {
	fallback, moved *fakeAPI
	routes          map[string]string // "host:port" of a request -> address to dial
	changes         chan OriginChange
}

func newRealFallback(t *testing.T) *realFallback {
	t.Helper()
	rf := &realFallback{routes: map[string]string{}, changes: make(chan OriginChange, 4)}
	rf.moved = newFakeAPI(t, func(w http.ResponseWriter, r *recorded) { reply(w, 200, fxProject, "") })
	rf.fallback = newFakeAPI(t, func(w http.ResponseWriter, r *recorded) {
		body := namesOrigin("https://example.com")
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		w.Header().Set("X-Response-Timestamp", ts)
		w.Header().Set("X-Response-Signature", responseSignature(testPaymentKey, ts, r.Header.Get("X-Signature"), body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	rf.routes["example.com:443"] = rf.moved.Listener.Addr().String()
	return rf
}

func (rf *realFallback) client(t *testing.T, baseURL string) *Client {
	t.Helper()
	transport := rf.fallback.Client().Transport.(*http.Transport).Clone()
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if to, ok := rf.routes[addr]; ok {
			addr = to
		}
		return dialer.DialContext(ctx, network, addr)
	}
	t.Cleanup(transport.CloseIdleConnections)
	c, err := NewClient(baseURL, testProject, WithPaymentKey(testPaymentKey), WithHTTPClient(&http.Client{Transport: transport}),
		WithFallbackBaseURL(rf.fallback.URL), WithOnOriginChange(func(change OriginChange) { rf.changes <- change }))
	if err != nil {
		t.Fatal(err)
	}
	c.c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return c
}

func (rf *realFallback) wantMoved(t *testing.T, c *Client, previous string) {
	t.Helper()
	if n := len(rf.fallback.recorded()); n != 1 {
		t.Fatalf("%d requests to the fallback host", n)
	}
	// The fake API checks the signature over the request URI with its query.
	if probe := rf.fallback.recorded()[0]; probe.Key != "payment" || !fallbackURIPattern.MatchString(probe.RequestURI) {
		t.Errorf("fallback request %s signed with %q", probe.RequestURI, probe.Key)
	}
	if got := rf.moved.recorded(); len(got) != 1 || got[0].Key != "payment" || got[0].RequestURI != "/api/v4/project" {
		t.Errorf("requests to the new origin: %+v", got)
	}
	select {
	case change := <-rf.changes:
		if change.Previous != previous || change.Current != "https://example.com" || time.Until(change.ExpiresAt) < 23*time.Hour {
			t.Errorf("change %+v", change)
		}
	default:
		t.Error("the hook was not called")
	}
	if c.c.activeOrigin() != "https://example.com" {
		t.Errorf("origin %s", c.c.activeOrigin())
	}
}

// TestRealTransportFallback checks the proofs of an unreachable host with the real
// transport: a refused connection, a failed TLS handshake and a handshake that never ends.
func TestRealTransportFallback(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		rf := newRealFallback(t)
		dead := "https://" + closedAddr(t)
		c := rf.client(t, dead)
		if _, err := c.Project.Get(context.Background()); err != nil {
			t.Fatal(err)
		}
		rf.wantMoved(t, c, dead)
	})

	t.Run("certificate of another host", func(t *testing.T) {
		rf := newRealFallback(t)
		// The certificate of the server behind api.example.org is not valid for that name.
		const base = "https://api.example.org"
		impostor := newFakeAPI(t, func(w http.ResponseWriter, r *recorded) { reply(w, 200, fxProject, "") })
		rf.routes["api.example.org:443"] = impostor.Listener.Addr().String()
		c := rf.client(t, base)
		if _, err := c.Project.Get(context.Background()); err != nil {
			t.Fatal(err)
		}
		rf.wantMoved(t, c, base)
		if n := len(impostor.recorded()); n != 0 {
			t.Errorf("%d requests reached the host with the wrong certificate", n)
		}
	})

	t.Run("handshake never ends", func(t *testing.T) {
		rf := newRealFallback(t)
		// A host that accepts the connection and stays silent, as a filter that drops the
		// TLS handshake does.
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var held []net.Conn
		var mu sync.Mutex
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				held = append(held, conn)
				mu.Unlock()
			}
		}()
		t.Cleanup(func() {
			_ = ln.Close()
			mu.Lock()
			defer mu.Unlock()
			for _, conn := range held {
				_ = conn.Close()
			}
		})
		silent := "https://" + ln.Addr().String()
		c := rf.client(t, silent)
		quick := &endpoint{method: http.MethodGet, path: "/project", scope: scopeAny, class: classRead, timeout: 200 * time.Millisecond, resource: "the project"}
		if _, err := c.c.do(context.Background(), call{ep: quick}); err != nil {
			t.Fatal(err)
		}
		rf.wantMoved(t, c, silent)
	})

	t.Run("response timeout is not a proof", func(t *testing.T) {
		rf := newRealFallback(t)
		release := make(chan struct{})
		slow := newFakeAPI(t, func(w http.ResponseWriter, r *recorded) { <-release })
		t.Cleanup(func() { close(release) })
		c := rf.client(t, slow.URL)
		quick := &endpoint{method: http.MethodGet, path: "/project", scope: scopeAny, class: classRead, timeout: 200 * time.Millisecond, resource: "the project"}
		_, err := c.c.do(context.Background(), call{ep: quick})
		var e *Error
		if !errors.As(err, &e) || e.Status != 0 || !OutcomeUnknown(err) {
			t.Fatalf("err %v", err)
		}
		if n := len(rf.fallback.recorded()); n != 0 {
			t.Errorf("%d requests to the fallback host after a response timeout", n)
		}
	})
}
