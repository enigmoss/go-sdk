# Getting started

## The client

```go
c, err := enigmoss.NewClient("https://api.example.com", projectID,
	enigmoss.WithPaymentKey(os.Getenv("ENIGMOSS_PAYMENT_KEY")),
	enigmoss.WithPayoutKey(os.Getenv("ENIGMOSS_PAYOUT_KEY")), // only if you send payouts, refunds or transfers
)
```

- The first argument is the API host from your dashboard. The SDK appends `/api/v4`; a host that
  already ends with `/api/v4` is accepted too. It must be `https`, without a query or a fragment.
- The project ID is the project UUID from the dashboard.
- Pass at least one key. A method whose key is missing fails before sending:
  `payouts need the payout key: pass enigmoss.WithPayoutKey`.
- `enigmoss.WithHTTPClient(hc)` sets your own `*http.Client`, for a proxy, mTLS or tests. The SDK
  uses a copy that never follows redirects. Leave `hc.Timeout` at 0 or at least 180 seconds.

Create one client per process and share it: it is safe for concurrent use. Printing the client
never shows the keys.

Every method takes a `context.Context` first. The context cancels the call and limits its total
time; the SDK applies its own limit per attempt (30 seconds, 180 for payouts and refunds).

## What the SDK does for you

You do not configure any of this:

- signs every request with HMAC-SHA256 over the exact bytes it sends;
- derives the `Idempotency-Key` of a create from its `order_id`, so a repeated create returns the
  same object instead of a duplicate (the same key the other official SDKs derive);
- retries at most twice, and only when it cannot cause a second effect: the request was not sent,
  or the server says it was not executed (rate limit, busy lock, still in progress, gateway
  unavailable);
- waits for `Retry-After` up to 10 seconds (60 seconds while walking pages with `All`);
- corrects the request timestamp once when your clock drifts.

## Errors

Errors found before sending (a malformed amount, an order_id with spaces, an ID that is not a UUID,
a missing key) are plain errors that start with `enigmoss:`. Nothing was sent.

Every other error is an `*enigmoss.Error`:

```go
var e *enigmoss.Error
if errors.As(err, &e) {
	log.Printf("status=%d code=%s request=%s fields=%v", e.Status, e.Code, e.RequestID, e.Fields)
}
```

- `Status` is 0 when no response was received; `errors.Unwrap(err)` is then the `net/http` error.
- `Code` is stable: compare it with the `enigmoss.Code*` constants and keep a default branch. A
  response from a proxy without the API error body has the code `http.<status>`.
- `Error()` adds a hint when there is one: which key is needed, which permission to enable, that
  the clock needs syncing.

For money logic you need only two checks, `enigmoss.OutcomeUnknown(err)` and
`enigmoss.IsOrderIDTaken(err)`; see [the three branches](../README.md#money-errors-three-branches).

## Pagination

`List` returns one page, newest first; `All` walks every page lazily:

```go
for p, err := range c.Payments.All(ctx, &enigmoss.PaymentQuery{Status: enigmoss.PaymentPaid}) {
	if err != nil {
		return err
	}
	fmt.Println(p.ID, p.Amount)
}
```

The next page is fetched only when the previous one is consumed, and `break` stops the requests.
To page by hand, pass the `NextCursor` of a page as the `Cursor` of the next query.

## Statuses and types

- Amounts are decimal strings, exactly as the API sends them. Do not convert them to `float64`.
- Times are `time.Time` in UTC; the zero time means the API sent none.
- Statuses are string types with constants (`enigmoss.PaymentPaid`, `enigmoss.PayoutFailed`). The
  server may add values: keep a `default` branch and decide by `IsFinal`.
- Optional fields of parameters are left out when empty. For flags and numbers where `false` or 0
  means something, the field is a pointer: `enigmoss.Bool(false)`, `enigmoss.Int(0)`.

## Versions

The module needs Go 1.24 or newer and is tested on the four latest Go releases. The minimum Go
version rises only in a minor release, with a changelog entry. `enigmoss.Version` is sent in the
`User-Agent` header.
