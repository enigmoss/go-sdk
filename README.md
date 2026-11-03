# Enigmoss Go SDK

The official Go SDK of the Enigmoss Project API v4: accept crypto payments, run static wallets,
send payouts and refunds, and receive signed webhooks.

[![Go Reference](https://pkg.go.dev/badge/github.com/enigmoss/enigmoss-go.svg)](https://pkg.go.dev/github.com/enigmoss/enigmoss-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

- **Small surface.** `NewClient` with three options, resource methods that take a `context.Context`,
  one error type with two checks for money logic, one function for webhooks.
- **Money-safe inside.** Request signatures, an `Idempotency-Key` derived from your `order_id`,
  retries only when they cannot pay twice, long enough timeouts for payouts, clock correction.
  Nothing to configure.
- **Standard library only.** Go 1.24 or newer, no dependencies.

## Install

```bash
go get github.com/enigmoss/enigmoss-go
```

## Which key you need

- **Payment key**: invoices, static wallets, balances, transactions, the events journal, and
  **verifying every webhook** (`payout.*` and `refund.*` are signed with it too).
- **Payout key**: payouts, payout quotes, refunds and transfers. Requests with it are accepted only
  from the IP addresses allowed for the project.
- Pass only the keys you use. Either key reads the project, payment and payout methods, and rates.

## Quickstart

The API host is white-label: use the host shown in your dashboard. `https://api.example.com` below
is a placeholder.

```go
c, err := enigmoss.NewClient("https://api.example.com", projectID,
	enigmoss.WithPaymentKey(os.Getenv("ENIGMOSS_PAYMENT_KEY")),
)
if err != nil {
	log.Fatal(err)
}

// 1. The first safe call: checks the key, the permissions and the clock. Changes nothing.
project, err := c.Project.Get(ctx)

// 2. An invoice. Money is always a decimal string; one order_id is one invoice, forever.
p, err := c.Payments.Create(ctx, &enigmoss.PaymentParams{
	Amount:       "10.50",
	CurrencyCode: "USDT",
	OrderID:      "order-42",
	CallbackURL:  "https://shop.example/webhooks/enigmoss",
})
// redirect the customer to p.PaymentURL

// 3. The webhook.
func webhook(w http.ResponseWriter, r *http.Request) {
	ev, err := enigmoss.ParseWebhook(r, projectID, os.Getenv("ENIGMOSS_PAYMENT_KEY"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// dedupe by ev.ID in your database, enqueue, answer 200 within 15 s
	if ev.Type == "payment.paid" {
		p, err := ev.Payment()
		// 4. Fulfil only on IsSuccessful(): paid in full and final.
		if err == nil && p.IsSuccessful() {
			fulfil(p.OrderID)
		}
	}
	w.WriteHeader(http.StatusOK)
}
```

## Money errors: three branches

Every create (invoice, wallet, payout, refund, transfer) ends in exactly one of these:

```go
p, err := c.Payouts.Create(ctx, params)
switch {
case err == nil:
	// created: save p.ID, wait for the payout.* webhook
case enigmoss.OutcomeUnknown(err):
	// maybe created: keep your record "pending", reconcile later with FindByOrderID; never a new order_id
case enigmoss.IsOrderIDTaken(err):
	p, err = c.Payouts.FindByOrderID(ctx, params.OrderID)
default:
	// definitely not created: fix the cause and retry with the SAME order_id
}
```

The SDK has already retried what was safe to retry. Every error that reached the network is an
`*enigmoss.Error` with `Status`, `Code`, `Message`, `RequestID` and validation `Fields`; switch on
`Code` (`enigmoss.CodePayoutInsufficientFunds` and others), never on the message.

## Payouts belong in a worker

Creating a payout or a refund calls the blockchain gateway synchronously and may take up to 180
seconds. Call it from a background job with a context of at least 10 minutes, not from an HTTP
handler. A short context does not make the payout safer, only its outcome unknown. If you pass
your own `http.Client`, leave its `Timeout` at 0 or set it to at least 180 seconds.

## There is no sandbox

The API has no test mode. Develop against a fake API with `httptest.NewTLSServer` and
`enigmoss.WithHTTPClient(srv.Client())`, sign test webhooks with `enigmoss.SignWebhook`, and make
`c.Project.Get(ctx)` your first live call. See [docs/testing.md](docs/testing.md).

## More

- [Getting started](docs/getting-started.md): client, keys, errors, pagination.
- [Payments and wallets](docs/payments-and-wallets.md)
- [Payouts, refunds and transfers](docs/payouts-refunds-transfers.md): reconcile, statuses.
- [Webhooks](docs/webhooks.md): verification, deduplication, local checks.
- [Testing](docs/testing.md)
- [docs/llms.txt](docs/llms.txt): the rules and every signature on one page, for AI coding assistants.

The API reference is `go doc github.com/enigmoss/enigmoss-go`.

## License

MIT, see [LICENSE](LICENSE).
