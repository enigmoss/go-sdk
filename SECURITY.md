# Security policy

## Supported versions

Security fixes are released for the latest minor version of the current major release.

## Reporting a vulnerability

Please **do not open a public issue** for a security problem.

Email **security@example.com** with:

- a description of the problem and its impact,
- steps or a minimal program to reproduce it,
- the SDK version and the Go version.

You will get an acknowledgement within a few working days. We will keep you informed while the
issue is fixed, publish a patched release and credit you in the changelog if you wish.

Never include real API keys in a report. If you think a key leaked, rotate it in the dashboard
right away.

## Handling API keys

- Keep keys in environment variables or a secrets manager, never in source control.
- Pass only the keys you use. Keep the payout key out of web-facing services.
- During a rotation of the payment key, pass both the new and the old key to `ParseWebhook` until
  the rotation is complete.
- Do not log request or response bodies. The SDK never prints the keys.
