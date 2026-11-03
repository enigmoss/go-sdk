# Payments and wallets

Both need the payment key.

## Invoices

```go
p, err := c.Payments.Create(ctx, &enigmoss.PaymentParams{
	Amount:       "10.50",
	CurrencyCode: "USDT",
	OrderID:      "order-42",
	CallbackURL:  "https://shop.example/webhooks/enigmoss",
	ExpiresIn:    3600,
	Metadata:     map[string]any{"cart": "42"},
})
if err != nil {
	return err
}
http.Redirect(w, r, p.PaymentURL, http.StatusSeeOther)
```

- `Amount`, `CurrencyCode` and `OrderID` are required; empty optional fields are not sent.
- One `order_id` is one invoice, forever. Repeating `Create` with the same `order_id` and the same
  parameters returns the same invoice (within 24 hours), so a double click or a retried job is safe.
- After an invoice expired or was cancelled, a new invoice for the same order needs a new
  `order_id`, such as `order-42-2`.
- `FindByOrderID(ctx, orderID)` returns `nil, nil` when there is no invoice: use it to reconcile
  after an error with `OutcomeUnknown`.
- `Cancel` cancels an unpaid invoice. `MarkPaid` accepts an underpaid invoice as paid.
  `CreateAMLVerificationLinks` returns the AML verification link of a locked payment.

## Fulfilment

Fulfil an order only when `p.IsSuccessful()` is true: the customer paid at least the invoice amount
and the status is final. A paid payment can still be non-final while an automatic conversion
settles.

Decide on the `payment.*` webhook, not on the redirect back from the payment page: the customer may
never come back.

| Status | Final? |
|---|---|
| `pending`, `processing`, `confirming`, `underpaid_waiting`, `locked`, `refunding` | never |
| `paid`, `overpaid`, `underpaid` | maybe: check `IsFinal` |
| `expired`, `cancelled`, `failed`, `refunded`, `refund_failed` | typically |

The server may add statuses. Keep a default branch and rely on `IsFinal`.

## Static wallets

A static wallet is a permanent deposit address bound to your `order_id`, such as a customer.

```go
wallet, err := c.Wallets.Create(ctx, &enigmoss.WalletParams{CurrencyCode: "USDT", NetworkCode: "tron", OrderID: "customer-7"})
// show wallet.Address or send the customer to wallet.PaymentURL
```

Every deposit is a `Payment` with `WalletID` set and no `OrderID`: in your webhook handler branch on
`p.IsDeposit()` first. List the deposits of a wallet with
`c.Payments.List(ctx, &enigmoss.PaymentQuery{WalletID: wallet.ID})`.

`Block(ctx, id, refundIncoming)` stops crediting new deposits. `refundIncoming` is fixed at the
first block. A blocked wallet can be refunded once with `c.Refunds.CreateForWallet`, which needs
the payout key.

## Payment methods

`c.PaymentMethods.List(ctx)` lists the currencies and networks customers can pay with.
`UpdateDiscount(ctx, currency, network, percent)` sets a discount (positive) or a surcharge
(negative), from -99 to 90 percent.
