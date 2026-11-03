# Webhooks

The API sends a signed `POST` to your callback URL when a payment, payout or refund changes status.

## Verify

```go
func webhook(w http.ResponseWriter, r *http.Request) {
	ev, err := enigmoss.ParseWebhook(r, projectID, os.Getenv("ENIGMOSS_PAYMENT_KEY"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	switch {
	case strings.HasPrefix(ev.Type, "payment."):
		p, err := ev.Payment()
		...
	case strings.HasPrefix(ev.Type, "payout."):
		p, err := ev.Payout()
		...
	case strings.HasPrefix(ev.Type, "refund."):
		r, err := ev.Refund()
		...
	default:
		// webhook.test or a type added later: acknowledge it
	}
	w.WriteHeader(http.StatusOK)
}
```

`ParseWebhook` reads the raw body (at most 1 MiB) and checks, in order: the `X-Project-Uuid`,
`X-Timestamp` and `X-Signature` headers, that the project is yours, that the timestamp is within 300
seconds, and the HMAC-SHA256 signature. Pass it the request before anything else reads the body.

- **Every webhook is signed with the payment key**, `payout.*` and `refund.*` included. A service
  that only sends payouts still needs the payment key to verify webhooks.
- **Key rotation**: pass the new and the old key, `ParseWebhook(r, projectID, newKey, oldKey)`.
- An error tells what is missing: a header (check that your proxy passes custom headers through)
  or a signature that does not match the raw body and the payment key.
- After a valid signature an unknown type is not an error. `ev.Data` keeps the raw object.

## Deduplicate and answer fast

Retries and redeliveries carry the same `ev.ID`, and events may arrive out of order. Answer 200
within 15 seconds; the API retries up to 9 times over about two days.

1. Claim the event: `INSERT INTO webhook_events (event_id) VALUES ($1) ON CONFLICT DO NOTHING`.
   If no row was inserted, the event was already handled: answer 200.
2. Do the work or enqueue it.
3. If that fails, delete the claim and answer 500, so the next delivery tries again.

When the order matters, compare `ev.CreatedAt` or get the current object with `Get`.

## Missed webhooks

The events journal keeps 60 days. Replay failed deliveries through the same handler logic:

```go
for rec, err := range c.Events.All(ctx, &enigmoss.EventQuery{DeliveryStatus: enigmoss.DeliveryFailed}) {
	if err != nil {
		return err
	}
	handle(rec.Payload) // the same Event that ParseWebhook returns
}
```

`c.Events.Redeliver(ctx, id)` and `RedeliverBulk` ask the API to send events again.

## Check your endpoint

- `c.Webhooks.SendTest(ctx, "https://shop.example/webhooks/enigmoss")` queues a `webhook.test`
  event. The delivery is asynchronous: the returned attempt is usually still `pending`, so read the
  result and your endpoint's response a few seconds later with `c.Events.Get(ctx, rec.ID)`.
- Without a tunnel, sign a body yourself and post it to your local server:

```go
body := []byte(`{"event":"payment.paid","event_id":"e-1","created_at":"2026-01-01T00:00:00Z","data":{"id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","status":"paid","is_final":true,"amount":"10.50"}}`)
req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost:8080/webhooks/enigmoss", bytes.NewReader(body))
req.Header = enigmoss.SignWebhook(body, projectID, paymentKey)
res, err := http.DefaultClient.Do(req)
```
