package enigmoss

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const webhookBody = `{"event":"payment.paid","event_id":"019b7710-0000-7000-8000-000000000001","created_at":"2026-01-01T03:05:00+03:00","data":` + fxPaidPayment + `}`

func webhookRequest(body string, header http.Header) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhooks", strings.NewReader(body))
	r.Header = header
	return r
}

func TestParseWebhook(t *testing.T) {
	ev, err := ParseWebhook(webhookRequest(webhookBody, SignWebhook([]byte(webhookBody), testProject, testPaymentKey)), testProject, testPaymentKey)
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "019b7710-0000-7000-8000-000000000001" || ev.Type != "payment.paid" || !ev.CreatedAt.Equal(time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)) || ev.CreatedAt.Location() != time.UTC {
		t.Errorf("event %+v", ev)
	}
	p, err := ev.Payment()
	if err != nil || !p.IsSuccessful() || p.Amount != "10.50" {
		t.Errorf("payment %+v, err %v", p, err)
	}
	if _, err := ev.Payout(); err == nil {
		t.Error("a payment event has no payout")
	}
}

func TestParseWebhookKeyRotation(t *testing.T) {
	h := SignWebhook([]byte(webhookBody), testProject, "old-payment-key")
	if _, err := ParseWebhook(webhookRequest(webhookBody, h), testProject, "new-payment-key", "old-payment-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseWebhook(webhookRequest(webhookBody, h), testProject, "new-payment-key"); err == nil {
		t.Fatal("an unknown key was accepted")
	}
}

func TestParseWebhookRejects(t *testing.T) {
	signed := func() http.Header { return SignWebhook([]byte(webhookBody), testProject, testPaymentKey) }
	cases := []struct {
		name   string
		body   string
		header func() http.Header
		keys   []string
		want   string
	}{
		{"no headers", webhookBody, func() http.Header { return http.Header{} }, []string{testPaymentKey}, "no X-Project-Uuid, X-Timestamp, X-Signature header"},
		{"no signature", webhookBody, func() http.Header { h := signed(); h.Del("X-Signature"); return h }, []string{testPaymentKey}, "no X-Signature header"},
		{"another project", webhookBody, func() http.Header { h := signed(); h.Set("X-Project-Uuid", "d6a7f0c2-6b1e-4b9a-9f3e-2c4d5e6f7a8b"); return h }, []string{testPaymentKey}, "another project"},
		{"expired", webhookBody, func() http.Header {
			h := http.Header{}
			ts := strconv.FormatInt(time.Now().Unix()-301, 10)
			h.Set("X-Project-Uuid", testProject)
			h.Set("X-Timestamp", ts)
			h.Set("X-Signature", signWebhook(testPaymentKey, ts, testProject, []byte(webhookBody)))
			return h
		}, []string{testPaymentKey}, "outside the accepted window"},
		{"malformed timestamp", webhookBody, func() http.Header { h := signed(); h.Set("X-Timestamp", "1e9"); return h }, []string{testPaymentKey}, "outside the accepted window"},
		{"tampered body", strings.Replace(webhookBody, "10.50", "99.50", 1), signed, []string{testPaymentKey}, "signature does not match"},
		{"payout key", webhookBody, signed, []string{testPayoutKey}, "signed with the payment key"},
		{"no key", webhookBody, signed, nil, "needs the payment key"},
		{"not an object", `[1,2]`, func() http.Header { return SignWebhook([]byte(`[1,2]`), testProject, testPaymentKey) }, []string{testPaymentKey}, "not a JSON object"},
		{"too large", strings.Repeat(" ", maxWebhookBytes+1), signed, []string{testPaymentKey}, "larger than 1 MiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseWebhook(webhookRequest(tc.body, tc.header()), testProject, tc.keys...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseWebhookTolerantHeaders(t *testing.T) {
	h := SignWebhook([]byte(webhookBody), strings.ToUpper(testProject), testPaymentKey)
	sig := h.Get("X-Signature")
	h.Set("X-Signature", "deadbeef")
	h.Add("X-Signature", " "+strings.ToUpper(sig)+" ")
	if _, err := ParseWebhook(webhookRequest(webhookBody, h), testProject, testPaymentKey); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownAndTestEvents(t *testing.T) {
	for _, body := range []string{
		`{"event":"wallet.archived","event_id":"e1","created_at":"not a time","data":{"anything":[1,2]}}`,
		`{"data": null, "event": "webhook.test", "event_id": "019b7710-0000-7000-8000-000000000001", "created_at": "2026-01-01T00:00:00Z"}`,
		`{"event":42}`,
	} {
		ev, err := ParseWebhook(webhookRequest(body, SignWebhook([]byte(body), testProject, testPaymentKey)), testProject, testPaymentKey)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if _, err := ev.Payment(); err == nil {
			t.Errorf("%s: Payment() of a non-payment event", body)
		}
	}
	ev := &Event{Type: "refund.completed", Data: []byte(fxRefund)}
	if r, err := ev.Refund(); err != nil || r.Status != RefundPending {
		t.Errorf("refund %+v, err %v", r, err)
	}
	ev = &Event{Type: "payout.completed", Data: []byte(`{"amount":"lots"}`)}
	if _, err := ev.Payout(); err == nil {
		t.Error("a non-decimal amount was accepted")
	}
}

func TestEventRecordPayloadIsAnEvent(t *testing.T) {
	c, _, _ := scriptedClient(t, step{status: 200, response: fxEventRecord})
	rec, err := c.Events.Get(t.Context(), "019b7710-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	p, err := rec.Payload.Payment()
	if err != nil || p.ID != paymentID || rec.Attempts[0].ResponseStatusCode != 200 {
		t.Errorf("record %+v, payment %+v, err %v", rec, p, err)
	}
}

func FuzzWebhook(f *testing.F) {
	f.Add([]byte(webhookBody), "1767225600", testProject, "ab")
	f.Add([]byte(`{"data":null}`), "x", "", "")
	f.Add([]byte(`{"event":"payment.paid","data":{"amount":7}}`), "0", testProject, strings.Repeat("0", 64))
	f.Fuzz(func(t *testing.T, body []byte, timestamp, project, signature string) {
		_ = signRequest(signature, timestamp, project, "POST", "/api/v4/"+project, body)
		h := http.Header{}
		h.Set("X-Project-Uuid", project)
		h.Set("X-Timestamp", timestamp)
		h.Set("X-Signature", signature)
		_ = verifyWebhook(body, h, testProject, []string{testPaymentKey}, time.Unix(1767225600, 0))

		r := &http.Request{Header: SignWebhook(body, testProject, testPaymentKey), Body: io.NopCloser(bytes.NewReader(body))}
		ev, err := ParseWebhook(r, testProject, testPaymentKey)
		if err != nil {
			return
		}
		_, _ = ev.Payment()
		_, _ = ev.Payout()
		_, _ = ev.Refund()
	})
}
