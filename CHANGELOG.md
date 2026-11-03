# Changelog

All notable changes to this module are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the module follows
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0]

First beta of the Go SDK of the Project API v4.

- `NewClient` with `WithPaymentKey`, `WithPayoutKey` and `WithHTTPClient`.
- All 34 operations of the API, with `List` and lazy `All` pagination.
- `*Error` with `OutcomeUnknown` and `IsOrderIDTaken` for money logic.
- `ParseWebhook`, `Event` and `SignWebhook`.
- Request signatures, idempotency keys, retries and timeouts at parity with the other official SDKs.
- Requires Go 1.24 or newer; no dependencies.
