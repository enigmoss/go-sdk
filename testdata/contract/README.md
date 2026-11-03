# Contract fixtures

A snapshot of the language-neutral fixtures shared by the official Enigmoss SDKs. Never edit them
by hand: they are copied from the shared contract, and `SOURCE` records the revision.

- `signature-vectors.json`: request and webhook HMAC-SHA256 signatures.
- `idempotency-key-vectors.json`: the `Idempotency-Key` every SDK derives for create requests.
- `openapi-v4.json`: the operations of the API and the field names of their responses.
