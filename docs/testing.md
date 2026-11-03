# Testing

The API has no sandbox. Test your integration with the standard library: a fake API on
`httptest.NewTLSServer`, the client pointed at it with `WithHTTPClient`, and webhooks signed with
`SignWebhook`.

## A fake API

```go
func fakeAPI(t *testing.T) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("%s %s without Idempotency-Key", r.Method, r.URL.Path)
		}
		data := map[string]string{
			"POST /api/v4/payments": `{"id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","order_id":"order-42","status":"pending","is_final":false,"amount":"10.50","currency_code":"USDT","payment_url":"https://pay.example.com/invoice/1"}`,
			"GET /api/v4/payouts":   `[]`,
		}[r.Method+" "+r.URL.Path]
		if data == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"type":"not_found","code":"resource.not_found","message":"Not found.","retryable":false},"meta":{"request_id":"test"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":`+data+`,"meta":{"request_id":"test"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckout(t *testing.T) {
	srv := fakeAPI(t)
	c, err := enigmoss.NewClient(srv.URL, "3f2b8c1e-5d4a-4e7b-9c6f-1a2b3c4d5e6f",
		enigmoss.WithPaymentKey("test-key"), enigmoss.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Payments.Create(context.Background(), &enigmoss.PaymentParams{Amount: "10.50", CurrencyCode: "USDT", OrderID: "order-42"})
	if err != nil || p.PaymentURL == "" {
		t.Fatalf("%+v, %v", p, err)
	}
}
```

Answer an error with the API envelope to test your branches:
`{"error":{"type":"...","code":"payout.insufficient_funds","message":"...","retryable":false},"meta":{"request_id":"..."}}`
with the HTTP status of the error (422 here). A 500 or `"code":"idempotency.in_progress"` makes
`OutcomeUnknown(err)` true; a 409 `resource.conflict` with `"details":{"id":"<uuid>"}` makes
`IsOrderIDTaken(err)` true.

## A webhook

```go
func TestWebhook(t *testing.T) {
	body := []byte(`{"event":"payment.paid","event_id":"019b7710-0000-7000-8000-000000000001","created_at":"2026-01-01T00:05:00Z",` +
		`"data":{"id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","order_id":"order-42","status":"paid","is_final":true,"amount":"10.50"}}`)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/enigmoss", bytes.NewReader(body))
	req.Header = enigmoss.SignWebhook(body, projectID, "test-payment-key")
	rec := httptest.NewRecorder()

	webhookHandler(rec, req) // your handler, configured with the same key

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
}
```

`SignWebhook` signs with the current time, exactly as the API does. The same helper checks your
endpoint locally without a tunnel: see [webhooks](webhooks.md#check-your-endpoint).

## The first live call

`c.Project.Get(ctx)` changes nothing. It returns the project, the type of the key, its permissions
and the server time: run it once against the live API before anything that moves money.
