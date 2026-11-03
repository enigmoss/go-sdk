# Payouts, refunds and transfers

All three need the payout key, and the server IP must be in the IP whitelist of the project. They
move money, so the rules below matter.

## One order_id, one operation, forever

The `order_id` identifies the operation. The SDK derives the `Idempotency-Key` from it, so the same
`order_id` never pays twice, even when the call is repeated by your job queue or by another
official SDK. A failed payout keeps its `order_id`: a new attempt to pay the same withdrawal after
a *failed* status needs a new `order_id`; a repeat after an *error* must use the same one.

## Run them in a worker

Creating a payout or a refund may take up to 180 seconds per attempt. Use a background job with a
context of at least 10 minutes:

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
defer cancel()

p, err := c.Payouts.Create(ctx, &enigmoss.PayoutParams{
	OrderID:      "withdrawal-42",
	Amount:       "100.00",
	CurrencyCode: "USDT",
	NetworkCode:  "tron",
	Address:      "TRecipient...",
})
switch {
case err == nil:
	// created: save p.ID, wait for the payout.* webhook
case enigmoss.OutcomeUnknown(err):
	// maybe created: keep your record "pending", reconcile later with FindByOrderID; never a new order_id
case enigmoss.IsOrderIDTaken(err):
	p, err = c.Payouts.FindByOrderID(ctx, "withdrawal-42")
default:
	// definitely not created: fix the cause and retry with the SAME order_id
}
```

Check `p.Charged` after creating: `Quote` is a calculation, not a promise.

## Reconcile

When the outcome is unknown, do not call `Create` again in the same request. Reconcile later, from
a cron job or when the `payout.*` webhook arrives:

```go
p, err := c.Payouts.FindByOrderID(ctx, orderID)
switch {
case err != nil:
	// try again later
case p == nil:
	// not created: repeating Create with the SAME order_id is safe
default:
	// created: store p.ID and follow its status
}
```

Give the API a few minutes before deciding that nothing was created.

## Rejected or failed

- An **error** of `Create` that is not `OutcomeUnknown` (for example `payout.insufficient_funds`)
  means the payout was not created. Fix the cause (top up the balance) and repeat with the same
  `order_id`.
- A **status** `failed` or `cancelled` means the payout was created and then not paid out. Read
  `FailureCode` and check `c.Balances.List` and `c.Transactions.All` before you re-credit your
  user. The `order_id` is used up: another attempt needs a new `order_id` and a deliberate decision.

| Payout status | Final? |
|---|---|
| `pending`, `processing`, `awaiting_approval` | never |
| `completed`, `failed`, `cancelled` | typically |

| Refund status | Final? |
|---|---|
| `pending`, `processing` | never |
| `completed`, `failed` | typically |

Rely on `IsFinal` and keep a default branch for statuses added later.

## Refunds

```go
r, err := c.Refunds.CreateForPayment(ctx, paymentID, &enigmoss.PaymentRefundParams{
	OrderID: "refund-42",
	Address: "TCustomer...",
	Amount:  "5.00", // empty: the whole paid amount
})
```

The same three branches apply. `CreateForWallet` refunds everything paid to a blocked static
wallet, once. Creating refunds is limited to one request per minute per project.

## Transfers

`c.Transfers.Create(ctx, &enigmoss.TransferParams{OrderID: "transfer-1", Amount: "25.00", CurrencyCode: "USDT"})`
moves money from the business balance to the owner's personal account, without a fee. It is
synchronous: a returned transfer means the money has moved.

## Balances

`c.Balances.List(ctx)` needs the payment key. `c.Transactions.All(ctx, query)` lists the balance
movements; debits have a negative amount.
