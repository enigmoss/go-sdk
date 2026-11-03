package enigmoss

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var payoutParams = &PayoutParams{OrderID: "withdrawal-42", Amount: "100.00", CurrencyCode: "USDT", NetworkCode: "tron", Address: "TRecipientabcdefghijklmnopqrstuvwx"}

// TestWire sends real requests over HTTP/2 and checks what the server sees: the signature
// of r.RequestURI, the "{}" body, no bare "?", reserved characters and raw UTF-8 bytes.
func TestWire(t *testing.T) {
	f := newFakeAPI(t, func(w http.ResponseWriter, r *recorded) {
		switch {
		case strings.HasPrefix(r.Path, "/api/v4/payment-methods/"):
			reply(w, 200, fxPaymentMethod, "")
		case strings.HasPrefix(r.Path, "/api/v4/payments/"):
			reply(w, 200, fxPayment, "")
		case r.Path == "/api/v4/payouts":
			reply(w, 201, fxPayout, "")
		default:
			reply(w, 200, "[]", cursorMeta)
		}
	})
	c, _ := newTestClient(t, f)
	ctx := context.Background()

	if _, err := c.Payments.Cancel(ctx, paymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Payments.List(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Events.List(ctx, &EventQuery{CreatedFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Cursor: "a b&c=d/é"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PaymentMethods.UpdateDiscount(ctx, "US DT", "a/b?c#d", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Payouts.Create(ctx, &PayoutParams{OrderID: "withdrawal-42", Amount: "100.00", CurrencyCode: "USDT", Address: "TRecipientabcdefghijklmnopqrstuvwx", Memo: "Café №42\r\n— ok"}); err != nil {
		t.Fatal(err)
	}

	want := []struct{ method, uri, body string }{
		{"POST", "/api/v4/payments/" + paymentID + "/cancel", "{}"},
		{"GET", "/api/v4/payments", ""},
		{"GET", "/api/v4/events?created_from=2026-01-01T00%3A00%3A00Z&cursor=a%20b%26c%3Dd%2F%C3%A9", ""},
		{"PATCH", "/api/v4/payment-methods/US%20DT/a%2Fb%3Fc%23d", `{"discount_percent":10}`},
		{"POST", "/api/v4/payouts", `{"order_id":"withdrawal-42","amount":"100.00","currency_code":"USDT","address":"TRecipientabcdefghijklmnopqrstuvwx","memo":"Café №42\r\n— ok"}`},
	}
	got := f.recorded()
	if len(got) != len(want) {
		t.Fatalf("%d requests", len(got))
	}
	for i, w := range want {
		r := got[i]
		if r.Proto != 2 {
			t.Errorf("%s: HTTP/%d, want HTTP/2", w.uri, r.Proto)
		}
		if r.Method != w.method || r.RequestURI != w.uri || string(r.Body) != w.body {
			t.Errorf("got %s %s %q\nwant %s %s %q", r.Method, r.RequestURI, r.Body, w.method, w.uri, w.body)
		}
		if r.Key == "" {
			t.Errorf("%s: the signature does not match r.RequestURI and the body", w.uri)
		}
		if r.Header.Get("X-Project-Uuid") != testProject {
			t.Errorf("X-Project-Uuid %q", r.Header.Get("X-Project-Uuid"))
		}
	}
	if key := got[4].Header.Get("Idempotency-Key"); key != "sdk1-84622c7ee70dcba22e9050c64e85df723cd3f0ca736bfa61809b207c8ec" {
		t.Errorf("Idempotency-Key %s differs from the PHP vector payouts-create", key)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	f := newFakeAPI(t, func(w http.ResponseWriter, r *recorded) {
		w.Header().Set("Location", "https://elsewhere.example/api/v4/payouts")
		w.WriteHeader(http.StatusFound)
	})
	c, _ := newTestClient(t, f)
	_, err := c.Payouts.Create(context.Background(), payoutParams)
	var e *Error
	if !errors.As(err, &e) || e.Status != 302 || e.Code != "http.302" {
		t.Fatalf("err %v", err)
	}
	if n := len(f.recorded()); n != 1 {
		t.Errorf("%d requests, the redirect must not be followed", n)
	}
	if OutcomeUnknown(err) {
		t.Error("a redirect means the API did not execute the request")
	}
}

// step is one scripted answer of the fake transport.
type step struct {
	status   int
	code     string // envelope error code; "" with status 0 is a transport failure
	sent     bool   // for a transport failure: the headers were written
	header   string // Retry-After
	body     string // a raw body instead of the envelope
	details  string
	retry    bool // envelope retryable flag
	response string
}

// scripted is a transport that answers each attempt with the next step.
type scripted struct {
	steps    []step
	attempts atomic.Int32
	keys     []string
}

func (s *scripted) RoundTrip(r *http.Request) (*http.Response, error) {
	n := int(s.attempts.Add(1)) - 1
	s.keys = append(s.keys, r.Header.Get("Idempotency-Key"))
	st := s.steps[min(n, len(s.steps)-1)]
	if st.status == 0 {
		if st.sent {
			if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteHeaders != nil {
				trace.WroteHeaders()
			}
			return nil, fmt.Errorf("read: connection reset by peer")
		}
		return nil, fmt.Errorf("dial tcp: connection refused")
	}
	body := st.body
	if body == "" && st.code != "" {
		retryable := "false"
		if st.retry {
			retryable = "true"
		}
		details := st.details
		if details == "" {
			details = "{}"
		}
		body = `{"error":{"type":"x","code":"` + st.code + `","message":"Server message.","retryable":` + retryable + `,"details":` + details + `},"meta":{"request_id":"req-9"}}`
	}
	if st.response != "" {
		body = `{"data":` + st.response + `,"meta":{}}`
	}
	h := http.Header{}
	if st.header != "" {
		h.Set("Retry-After", st.header)
	}
	return &http.Response{StatusCode: st.status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func scriptedClient(t *testing.T, steps ...step) (*Client, *scripted, *[]time.Duration) {
	t.Helper()
	tr := &scripted{steps: steps}
	c, err := NewClient("https://api.example.com", testProject, WithPaymentKey(testPaymentKey), WithPayoutKey(testPayoutKey), WithHTTPClient(&http.Client{Transport: tr}))
	if err != nil {
		t.Fatal(err)
	}
	delays := &[]time.Duration{}
	c.c.sleep = func(ctx context.Context, d time.Duration) error { *delays = append(*delays, d); return ctx.Err() }
	c.c.random = func() float64 { return 0.5 }
	return c, tr, delays
}

var (
	ok201       = step{status: 201, response: fxPayout}
	sentFailure = step{sent: true}
	notSent     = step{}
)

func TestRetriesAndOutcome(t *testing.T) {
	type result struct {
		attempts int
		ok       bool
		unknown  bool
		taken    bool
	}
	cases := []struct {
		name  string
		op    string // "payout" (create), "get", "redeliver", "cancel"
		steps []step
		want  result
	}{
		{"created", "payout", []step{ok201}, result{1, true, false, false}},
		{"not sent then created", "payout", []step{notSent, ok201}, result{2, true, false, false}},
		{"sent and broken, keyed retry succeeds", "payout", []step{sentFailure, ok201}, result{2, true, false, false}},
		{"rate limited then created", "payout", []step{{status: 429, code: CodeRateLimitExceeded}, ok201}, result{2, true, false, false}},
		{"lock busy then created", "payout", []step{{status: 409, code: CodeLockNotAcquired}, ok201}, result{2, true, false, false}},
		{"dependency unavailable then created", "payout", []step{{status: 503, code: CodeDependencyUnavailable, retry: true}, ok201}, result{2, true, false, false}},
		{"gateway page then created", "payout", []step{{status: 502, body: "<html>bad gateway</html>"}, ok201}, result{2, true, false, false}},
		{"internal error is not retried", "payout", []step{{status: 500, code: CodeInternalServerError, retry: true}}, result{1, false, true, false}},
		{"business error is not retried", "payout", []step{{status: 422, code: CodePayoutInsufficientFunds}}, result{1, false, false, false}},
		{"retryable dependency error is not retried on a create", "payout", []step{{status: 502, code: CodeDependencyGateway, retry: true}}, result{1, false, true, false}},
		{"sent then rate limited is unknown", "payout", []step{sentFailure, {status: 429, code: CodeRateLimitExceeded}}, result{3, false, true, false}},
		{"sent then validation error is unknown", "payout", []step{sentFailure, {status: 422, code: CodeValidationFailed}}, result{2, false, true, false}},
		{"not sent then validation error is known", "payout", []step{notSent, {status: 422, code: CodeValidationFailed}}, result{2, false, false, false}},
		{"in progress to the end", "payout", []step{{status: 409, code: CodeIdempotencyInProgress}}, result{3, false, true, false}},
		{"gateway pages to the end", "payout", []step{{status: 504, body: "timeout"}}, result{3, false, true, false}},
		{"sent then order id taken", "payout", []step{sentFailure, {status: 409, code: CodeResourceConflict, details: `{"id":"9b2d4f6a-1c3e-4a5b-8d7f-0e1a2b3c4d5e"}`}}, result{2, false, false, true}},
		{"conflict of a state is not order id taken", "payout", []step{{status: 409, code: CodeResourceConflict, details: `{"id":"x","status":"completed"}`}}, result{1, false, false, false}},
		{"unreadable 2xx", "payout", []step{{status: 201, body: "not json"}}, result{1, false, true, false}},
		{"2xx without data", "payout", []step{{status: 201, body: `{"meta":{}}`}}, result{1, false, true, false}},
		{"2xx of another shape", "payout", []step{{status: 201, response: `{"id":42}`}}, result{1, false, true, false}},
		{"2xx with a non-decimal amount", "payout", []step{{status: 201, response: `{"id":"x","amount":"ten"}`}}, result{1, false, true, false}},
		{"long Retry-After is not waited", "payout", []step{{status: 429, code: CodeRateLimitExceeded, header: "30"}, ok201}, result{1, false, false, false}},
		{"read retries a retryable error", "get", []step{{status: 500, code: CodeInternalServerError, retry: true}, {status: 200, response: fxPayout}}, result{2, true, false, false}},
		{"read does not retry a non-retryable error", "get", []step{{status: 404, code: CodeResourceNotFound}}, result{1, false, false, false}},
		{"read retries a sent failure", "get", []step{sentFailure, sentFailure, sentFailure}, result{3, false, true, false}},
		{"redeliver is not retried after sending", "redeliver", []step{sentFailure, {status: 200, response: fxEventRecord}}, result{1, false, true, false}},
		{"redeliver is retried when not sent", "redeliver", []step{notSent, {status: 200, response: fxEventRecord}}, result{2, true, false, false}},
		{"redeliver is not retried on 503", "redeliver", []step{{status: 503, body: "x"}}, result{1, false, true, false}},
		{"action keeps its key across retries", "cancel", []step{sentFailure, {status: 429, code: CodeRateLimitExceeded}, {status: 200, response: fxPayment}}, result{3, true, false, false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, tr, _ := scriptedClient(t, tc.steps...)
			ctx := context.Background()
			var err error
			switch tc.op {
			case "payout":
				_, err = c.Payouts.Create(ctx, payoutParams)
			case "get":
				_, err = c.Payouts.Get(ctx, "9b2d4f6a-1c3e-4a5b-8d7f-0e1a2b3c4d5e")
			case "redeliver":
				_, err = c.Events.Redeliver(ctx, "019b7710-0000-7000-8000-000000000001")
			case "cancel":
				_, err = c.Payments.Cancel(ctx, paymentID)
			}
			got := result{int(tr.attempts.Load()), err == nil, OutcomeUnknown(err), IsOrderIDTaken(err)}
			if got != tc.want {
				t.Errorf("got %+v, want %+v (err %v)", got, tc.want, err)
			}
			if err != nil {
				var e *Error
				if !errors.As(err, &e) {
					t.Errorf("%T is not *Error", err)
				}
			}
			for _, k := range tr.keys {
				if k != tr.keys[0] {
					t.Errorf("Idempotency-Key changed between attempts: %q", tr.keys)
				}
			}
		})
	}
}

func TestRetryDelays(t *testing.T) {
	c, _, delays := scriptedClient(t, step{status: 429, code: CodeRateLimitExceeded, header: "2"}, notSent, ok201)
	if _, err := c.Payouts.Create(context.Background(), payoutParams); err != nil {
		t.Fatal(err)
	}
	// Retry-After wins; then backoff of the second attempt: 0.5 * min(8, 0.5*2) s.
	if want := []time.Duration{2 * time.Second, 500 * time.Millisecond}; fmt.Sprint(*delays) != fmt.Sprint(want) {
		t.Errorf("delays %v, want %v", *delays, want)
	}
}

func TestAllWaitsLongerRetryAfter(t *testing.T) {
	c, tr, delays := scriptedClient(t, step{status: 429, code: CodeRateLimitExceeded, retry: true, header: "45"}, step{status: 200, response: "[" + fxPayout + "]"})
	for _, err := range c.Payouts.All(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if tr.attempts.Load() != 2 || fmt.Sprint(*delays) != "[45s]" {
		t.Errorf("attempts %d, delays %v", tr.attempts.Load(), *delays)
	}
}

func TestContextCancelledWhileWaiting(t *testing.T) {
	c, tr, _ := scriptedClient(t, sentFailure, ok201)
	ctx, cancel := context.WithCancel(context.Background())
	c.c.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	_, err := c.Payouts.Create(ctx, payoutParams)
	if tr.attempts.Load() != 1 || !OutcomeUnknown(err) {
		t.Errorf("attempts %d, err %v", tr.attempts.Load(), err)
	}
}

func TestContextCancelledBeforeSending(t *testing.T) {
	f := newFakeAPI(t, func(w http.ResponseWriter, r *recorded) { reply(w, 201, fxPayout, "") })
	c, _ := newTestClient(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Payouts.Create(ctx, payoutParams)
	var e *Error
	if !errors.As(err, &e) || e.Status != 0 || OutcomeUnknown(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if len(f.recorded()) != 0 {
		t.Error("the request was sent")
	}
}

// TestRealTransportSendPhase checks httptrace with the real transport: a refused
// connection is not sent, a connection dropped after the request is.
func TestRealTransportSendPhase(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c, err := NewClient("https://"+addr, testProject, WithPayoutKey(testPayoutKey))
	if err != nil {
		t.Fatal(err)
	}
	c.c.sleep = func(context.Context, time.Duration) error { return nil }
	_, err = c.Payouts.Create(context.Background(), payoutParams)
	var e *Error
	if !errors.As(err, &e) || e.Status != 0 || OutcomeUnknown(err) || errors.Unwrap(err) == nil {
		t.Fatalf("refused connection: %v", err)
	}

	var hits atomic.Int32
	f := newFakeAPI(t, func(w http.ResponseWriter, r *recorded) {
		hits.Add(1)
		panic(http.ErrAbortHandler) // drop the connection after reading the request
	})
	c2, _ := newTestClient(t, f)
	_, err = c2.Payouts.Create(context.Background(), payoutParams)
	if !errors.As(err, &e) || e.Status != 0 || !OutcomeUnknown(err) {
		t.Fatalf("dropped connection: %v", err)
	}
	if hits.Load() != 3 {
		t.Errorf("%d attempts, a keyed create is retried after a dropped connection", hits.Load())
	}
}

func TestClockSkewIsCorrectedOnce(t *testing.T) {
	var timestamps []string
	serverTime := time.Now().Unix() + 3600
	f := newFakeAPI(t, func(w http.ResponseWriter, r *recorded) {
		timestamps = append(timestamps, r.Header.Get("X-Timestamp"))
		if len(timestamps) == 1 {
			replyError(w, 401, CodeTimestampExpired, false, fmt.Sprintf(`{"server_time":%d}`, serverTime))
			return
		}
		reply(w, 200, fxProject, "")
	})
	c, delays := newTestClient(t, f)
	if _, err := c.Project.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(timestamps) != 2 || len(*delays) != 0 {
		t.Fatalf("timestamps %v, delays %v", timestamps, *delays)
	}
	if got := c.c.clockSkew.Load(); got < 3590 || got > 3610 {
		t.Errorf("offset %d", got)
	}

	// A second expired timestamp with the same server time is not corrected again.
	f2 := newFakeAPI(t, func(w http.ResponseWriter, r *recorded) {
		replyError(w, 401, CodeTimestampExpired, false, `{"server_time":1}`)
	})
	c2, _ := newTestClient(t, f2)
	_, err := c2.Project.Get(context.Background())
	if len(f2.recorded()) != 2 || !strings.Contains(err.Error(), "Sync the server clock") {
		t.Errorf("%d requests, err %v", len(f2.recorded()), err)
	}
	if c2.c.clockSkew.Load() != -maxClockDrift {
		t.Errorf("offset %d is not clamped", c2.c.clockSkew.Load())
	}
}

func TestErrorMessages(t *testing.T) {
	cases := []struct {
		step step
		want string
	}{
		{step{status: 403, code: CodeIPForbidden, details: `{"ip":"203.0.113.7"}`}, "Server message. [auth.ip_forbidden, request req-9] Add the IP address 203.0.113.7 to the IP whitelist of the project in the dashboard."},
		{step{status: 403, code: CodeIPForbidden}, "Server message. [auth.ip_forbidden, request req-9] Add this server's IP address to the IP whitelist of the project in the dashboard."},
		{step{status: 403, code: CodePermissionDenied, details: `{"permission":"payout_create"}`}, `Enable the "payout_create" API permission in the dashboard.`},
		{step{status: 401, code: CodeSignatureInvalid}, "payouts, refunds and transfers need the payout key"},
		{step{status: 404, body: "<html>"}, "Unexpected HTTP 404 response without an API error body. [http.404] Check the baseURL"},
		{step{status: 429, code: CodeRateLimitExceeded, header: "60"}, "Retry in 60 s."},
		{step{status: 409, code: CodeIdempotencyPayloadMismatch}, "one order_id is one operation"},
	}
	for _, tc := range cases {
		c, _, _ := scriptedClient(t, tc.step)
		_, err := c.Payouts.Create(context.Background(), payoutParams)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error %q does not contain %q", err, tc.want)
		}
	}
}

func TestValidationFields(t *testing.T) {
	c, _, _ := scriptedClient(t, step{status: 422, body: `{"error":{"type":"validation_error","code":"validation.failed","message":"Invalid.","retryable":false,` +
		`"violations":[{"field":"amount","rule":"min","message":"Too small."},{"field":"amount","rule":"x","message":"Bad."},{"field":null,"rule":"custom","message":"Whole request."}]},"meta":{"request_id":"r"}}`})
	_, err := c.Payouts.Create(context.Background(), payoutParams)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeValidationFailed || e.Fields["amount"] != "Too small.; Bad." || e.Fields[""] != "Whole request." || e.RequestID != "r" {
		t.Fatalf("err %#v", err)
	}
}

func TestLocalErrorsAreNotSent(t *testing.T) {
	c, tr, _ := scriptedClient(t, ok201)
	paymentOnly, _ := NewClient("https://api.example.com", testProject, WithPaymentKey("k"))
	ctx := context.Background()
	cases := map[string]error{}
	_, cases["amount"] = c.Payouts.Create(ctx, &PayoutParams{OrderID: "a", Amount: "1,5", CurrencyCode: "USDT", Address: "TRecipientabcdefghij"})
	_, cases["float amount"] = c.Payments.Create(ctx, &PaymentParams{OrderID: "a", Amount: "1e5", CurrencyCode: "USDT"})
	_, cases["order id"] = c.Payouts.Create(ctx, &PayoutParams{OrderID: "a b", Amount: "1", CurrencyCode: "USDT", Address: "TRecipientabcdefghij"})
	_, cases["uuid"] = c.Payments.Get(ctx, "42")
	_, cases["nil params"] = c.Payouts.Create(ctx, nil)
	_, cases["limit"] = c.Payments.List(ctx, &PaymentQuery{Limit: 51})
	_, cases["transfer decimals"] = c.Transfers.Create(ctx, &TransferParams{OrderID: "t", Amount: "1.123456789", CurrencyCode: "USDT"})
	_, cases["other filter"] = c.Transactions.List(ctx, &TransactionQuery{Type: TransactionOther})
	_, cases["event"] = c.Events.List(ctx, &EventQuery{Event: "paid"})
	_, cases["discount"] = c.PaymentMethods.UpdateDiscount(ctx, "USDT", "tron", 91)
	_, cases["missing key"] = paymentOnly.Payouts.Create(ctx, payoutParams)
	for name, err := range cases {
		var e *Error
		if err == nil || errors.As(err, &e) || OutcomeUnknown(err) || !strings.HasPrefix(err.Error(), "enigmoss: ") {
			t.Errorf("%s: err %v", name, err)
		}
	}
	if !strings.Contains(cases["missing key"].Error(), "payouts need the payout key: pass enigmoss.WithPayoutKey") {
		t.Error(cases["missing key"])
	}
	if tr.attempts.Load() != 0 {
		t.Errorf("%d requests sent", tr.attempts.Load())
	}
}

func TestNewClientValidation(t *testing.T) {
	key := WithPaymentKey("k")
	bad := map[string]struct {
		url, project string
		opts         []Option
	}{
		"http":          {"http://api.example.com", testProject, []Option{key}},
		"relative":      {"api.example.com", testProject, []Option{key}},
		"query":         {"https://api.example.com/?a=1", testProject, []Option{key}},
		"fragment":      {"https://api.example.com/#x", testProject, []Option{key}},
		"credentials":   {"https://u:p@api.example.com", testProject, []Option{key}},
		"path":          {"https://api.example.com/v4", testProject, []Option{key}},
		"project":       {"https://api.example.com", "project-1", []Option{key}},
		"no key":        {"https://api.example.com", testProject, nil},
		"empty key":     {"https://api.example.com", testProject, []Option{WithPayoutKey("")}},
		"nil client":    {"https://api.example.com", testProject, []Option{key, WithHTTPClient(nil)}},
		"empty baseURL": {"", testProject, []Option{key}},
	}
	for name, tc := range bad {
		if _, err := NewClient(tc.url, tc.project, tc.opts...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	for u, origin := range map[string]string{
		"https://API.example.com":         "https://api.example.com",
		"https://api.example.com/":        "https://api.example.com",
		"https://api.example.com/api/v4/": "https://api.example.com",
		"https://api.example.com:8443":    "https://api.example.com:8443",
	} {
		c, err := NewClient(u, " 3F2B8C1E-5D4A-4E7B-9C6F-1A2B3C4D5E6F ", key)
		if err != nil {
			t.Errorf("%s: %v", u, err)
			continue
		}
		if c.c.projectID != testProject || c.c.origin != origin {
			t.Errorf("%s: origin %s, project %s", u, c.c.origin, c.c.projectID)
		}
	}
}

func TestKeysAreRedacted(t *testing.T) {
	c, err := NewClient("https://api.example.com", testProject, WithPaymentKey("very-secret-payment"), WithPayoutKey("very-secret-payout"))
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		for _, v := range []any{c, *c, c.c, c.c.paymentKey, c.Payouts, *c.Payouts} {
			if out := fmt.Sprintf(format, v); strings.Contains(out, "very-secret") {
				t.Errorf("%s of %T prints the key: %s", format, v, out)
			}
		}
	}
}

func TestCallerHTTPClientIsNotChanged(t *testing.T) {
	hc := &http.Client{}
	if _, err := NewClient("https://api.example.com", testProject, WithPaymentKey("k"), WithHTTPClient(hc)); err != nil {
		t.Fatal(err)
	}
	if hc.CheckRedirect != nil {
		t.Error("NewClient changed the caller's http.Client")
	}
}

func TestResponsesAreNormalized(t *testing.T) {
	c, _, _ := scriptedClient(t, step{status: 200, response: fxPaidPayment})
	p, err := c.Payments.Get(context.Background(), paymentID)
	if err != nil {
		t.Fatal(err)
	}
	if p.CreatedAt.Location() != time.UTC || !p.CreatedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt %v", p.CreatedAt)
	}
	if !p.IsSuccessful() || p.IsDeposit() || p.Conversion == nil || p.Conversion.To.CurrencyCode != "USDC" || *p.Discount.Percent != 5 || p.Metadata["cart"] != "42" {
		t.Errorf("payment %+v", p)
	}
	if !p.PaidAt.Equal(time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)) || !(&Payment{}).ExpiresAt.IsZero() {
		t.Error("times")
	}
}

func TestPagination(t *testing.T) {
	pages := map[string]string{
		"":   `{"cursor":{"next":"c2","has_more":true}}`,
		"c2": `{"cursor":{"next":"c3","has_more":true}}`,
		"c3": `{"cursor":{"next":null,"has_more":false}}`,
	}
	f := newFakeAPI(t, func(w http.ResponseWriter, r *recorded) {
		cursor := ""
		if _, after, ok := strings.Cut(r.RequestURI, "cursor="); ok {
			cursor = after
		}
		reply(w, 200, "["+fxPayment+","+fxPayment+"]", pages[cursor])
	})
	c, _ := newTestClient(t, f)
	ctx := context.Background()

	n := 0
	for p, err := range c.Payments.All(ctx, &PaymentQuery{Status: PaymentPending}) {
		if err != nil {
			t.Fatal(err)
		}
		if p.ID == "" {
			t.Fatal("empty payment")
		}
		n++
	}
	if n != 6 || len(f.recorded()) != 3 {
		t.Errorf("%d payments in %d requests", n, len(f.recorded()))
	}
	if uri := f.recorded()[1].RequestURI; uri != "/api/v4/payments?status=pending&cursor=c2" {
		t.Errorf("second page %s", uri)
	}

	// break stops the requests.
	before := len(f.recorded())
	for range c.Payments.All(ctx, nil) {
		break
	}
	if len(f.recorded()) != before+1 {
		t.Error("break did not stop the requests")
	}

	page, err := c.Payments.List(ctx, nil)
	if err != nil || !page.HasMore || page.NextCursor != "c2" || len(page.Items) != 2 {
		t.Errorf("page %+v, err %v", page, err)
	}

	// An error ends the iteration.
	c2, _, _ := scriptedClient(t, step{status: 404, code: CodeResourceNotFound})
	var errs int
	for p, err := range c2.Payouts.All(ctx, nil) {
		if err == nil || p != nil {
			t.Fatal("want an error")
		}
		errs++
	}
	if errs != 1 {
		t.Errorf("%d errors", errs)
	}
}

func TestFindByOrderIDReturnsNilWhenAbsent(t *testing.T) {
	c, _, _ := scriptedClient(t, step{status: 200, response: "[]"})
	p, err := c.Payouts.FindByOrderID(context.Background(), "withdrawal-42")
	if p != nil || err != nil {
		t.Errorf("%v, %v", p, err)
	}
}
