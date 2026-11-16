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
- `enigmoss.WithFallbackBaseURL(url)` and `enigmoss.WithOnOriginChange(fn)` turn on the
  [fallback API host](#fallback-api-host). Without them nothing changes.

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

## Fallback API host

The API host can become unreachable from your servers: its name stops resolving, or connections
to it are refused or dropped. With a fallback host the client then asks where the API is now and
moves there by itself. The option has no default: without it, or with an empty URL (an unset
environment variable), the SDK behaves exactly as before. A URL that is not empty and not valid
is an error of `NewClient`.

```go
c, err := enigmoss.NewClient("https://api.example.com", projectID,
	enigmoss.WithPaymentKey(os.Getenv("ENIGMOSS_PAYMENT_KEY")),
	enigmoss.WithFallbackBaseURL("https://api.example.org"), // same form as the base URL
	enigmoss.WithOnOriginChange(func(ch enigmoss.OriginChange) {
		log.Printf("enigmoss: API origin %s -> %s until %s", ch.Previous, ch.Current, ch.ExpiresAt)
	}),
)
```

The order of a call is always **retries, then the fallback**:

1. The usual retries on the current API host, unchanged.
2. If the last attempt again could not connect to the API host, the SDK sends one signed
   `GET /api/v4/project/platform-brand?nonce=...` to the fallback host (3 seconds at most, inside
   the time limit of the call) and reads the API origin from the response.
3. A new origin is remembered by the client, and the call is repeated there: the same body, the
   same `Idempotency-Key`, a fresh timestamp and signature, a fresh count of retries. The calls
   that follow go straight to the new origin.

What starts the fallback is only a proof that no connection to the API host was established: a
failed DNS lookup, a refused or timed-out connect, a failed or timed-out TLS handshake. A response
timeout, a connection lost after the request was sent, an error response, a cancelled context and
a failure to reach your outgoing proxy never start it: the fallback never starts from an attempt
that may have been executed, and the repeated request carries the same `Idempotency-Key`.

What the SDK trusts:

- only a response signed with the key that signed the request, with a fresh timestamp
  (`X-Response-Timestamp`, `X-Response-Signature`). The response signature covers the signature
  of the request, and every request carries a random one-time nonce in its signed URI, so a
  recorded response fits no other request;
- only an `https` origin with a host name and the default port: no IP address, no `localhost`,
  no path.

What is remembered and for how long:

- The new origin lives in the client (in memory) for the `refresh_after` of the response, at least
  1 hour and at most 7 days. Then the client goes back to the base URL it was created with, and
  asks the fallback host again if that host is still unreachable. `c.String()` shows the origin in
  use; `WithOnOriginChange` reports every move, so that you can log it or update your settings.
- If the fallback host answers with an error (`401`, `403`, `404`, `429`, `5xx`), without a valid
  signature or without an acceptable origin, that answer is the error of the call: an
  `*enigmoss.Error` whose `errors.Unwrap` is the network error of the call. Nothing is
  remembered. `enigmoss.OutcomeUnknown` still describes your request, not the answer of the
  fallback host: a payout that was never sent is not "maybe created" because the fallback host
  answered `500`.
- If the fallback host names the same origin or cannot be reached either, the call returns its
  own network error.
- After a fallback that gave no new origin the fallback host is not asked again for 5 minutes;
  calls in that time return their network errors as usual.
- Concurrent calls share one request to the fallback host. A call asks it at most once.

Limits:

- Behind an outgoing proxy (`HTTPS_PROXY`, `http.Transport.Proxy`) the transport reports a refused
  or timed-out connection as a proxy failure, which does not start the fallback. Only a failed TLS
  handshake with the API host through the proxy does.
- A timed-out connect is recognized through `net/http/httptrace`. A custom `http.RoundTripper`
  that does not report trace events starts the fallback only with typed errors (`*net.OpError` of
  a dial, `*net.DNSError`, TLS certificate errors).
- The function of `WithOnOriginChange` runs in a goroutine of the SDK before the waiting calls
  continue: keep it fast.

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
