# Changelog

All notable changes to this module are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the module follows
[Semantic Versioning](https://semver.org/).

## [Unreleased]

- **The module path is `github.com/enigmoss/go-sdk`** (it was `github.com/enigmoss/enigmoss-go`), the address of
  the repository: `go get github.com/enigmoss/go-sdk`. The package name is still `enigmoss`.
- `Error.RetryAfter`: how long the server asked to wait (Retry-After) on a rate limit.
- Fallback API host, off by default (and with an empty URL): `WithFallbackBaseURL` and
  `WithOnOriginChange` with `OriginChange`. When the usual retries are used up and the last attempt
  again could not connect to the API host (DNS, connect, TLS handshake), the SDK asks the fallback
  host for the API origin with one signed `GET /api/v4/project/platform-brand?nonce=...` (a
  one-time nonce in the signed URI), accepts only a response signed with the key of the request
  and a plain `https` origin, remembers it in the client for the `refresh_after` of the response
  (1 hour to 7 days) and repeats the call there with the same body and `Idempotency-Key`. An error
  answer of the fallback host is returned with the network error as its cause and pauses the
  fallback for 5 minutes; concurrent calls share one fallback request. `Client.String()` shows the
  origin in use. Limits: a failure to reach an outgoing proxy and a transport without `httptrace`
  events on timeouts do not start the fallback. See
  [docs/getting-started.md](docs/getting-started.md#fallback-api-host).

## [0.1.0]

First beta of the Go SDK of the Project API v4.

- `NewClient` with `WithPaymentKey`, `WithPayoutKey` and `WithHTTPClient`.
- All 34 operations of the API, with `List` and lazy `All` pagination.
- `*Error` with `OutcomeUnknown` and `IsOrderIDTaken` for money logic.
- `ParseWebhook`, `Event` and `SignWebhook`.
- Request signatures, idempotency keys, retries and timeouts at parity with the other official SDKs.
- Requires Go 1.24 or newer; no dependencies.
