// Package enigmoss is the official Go SDK of the Enigmoss Project API v4: invoices, static
// wallets, payouts, refunds, transfers and signed webhooks.
//
// # Quickstart
//
//	c, err := enigmoss.NewClient("https://api.example.com", projectID, // the API host from your dashboard
//		enigmoss.WithPaymentKey(os.Getenv("ENIGMOSS_PAYMENT_KEY")),
//		enigmoss.WithPayoutKey(os.Getenv("ENIGMOSS_PAYOUT_KEY")), // only for payouts, refunds, transfers
//	)
//	if err != nil { ... }
//
//	p, err := c.Payments.Create(ctx, &enigmoss.PaymentParams{Amount: "10.50", CurrencyCode: "USDT", OrderID: "order-42"})
//	// redirect the customer to p.PaymentURL
//
//	func webhook(w http.ResponseWriter, r *http.Request) {
//		ev, err := enigmoss.ParseWebhook(r, projectID, paymentKey)
//		if err != nil { w.WriteHeader(400); return }
//		// dedupe by ev.ID, enqueue, answer 200 within 15 s
//		if ev.Type == "payment.paid" { p, _ := ev.Payment(); if p.IsSuccessful() { fulfil(p.OrderID) } }
//		w.WriteHeader(200)
//	}
//
// # The five rules of money
//
//  1. Money is a decimal string ("10.50"), never a float. The SDK sends and returns it as is.
//  2. One order_id is one object, forever, even a failed one. A repeated create with the same
//     order_id returns the same object: the Idempotency-Key is derived from it.
//  3. When OutcomeUnknown(err) is true the request may have run: keep your record "pending",
//     reconcile later with FindByOrderID, and never create a new order_id for it. When
//     IsOrderIDTaken(err) is true the object exists: get it with FindByOrderID. Any other error
//     means nothing was created: fix the cause and retry with the same order_id.
//  4. Fulfil an order only when Payment.IsSuccessful() is true.
//  5. Every webhook is signed with the payment key, payout.* and refund.* included. Creating a
//     payout or a refund may take up to 180 seconds: call it from a background worker with a
//     context of at least 10 minutes.
//
// The SDK signs requests, derives idempotency keys, retries only when it is safe, waits long
// enough for payouts and corrects a drifting clock. None of it needs configuration. There is
// no sandbox: the first safe call against the live API is c.Project.Get(ctx).
//
// The rules, all signatures and the statuses are also in docs/llms.txt of the module.
package enigmoss
