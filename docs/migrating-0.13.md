# Migrating to Flint CLI 0.13.0

This snapshot refresh removes subscription action commands, so it requires a minor version bump before 1.0. Package versions are stamped during release preparation; this change does not publish a release.

The embedded snapshot is copied byte-for-byte from [`web/apps/developers/src/generated/openapi.json`](https://github.com/flint-pay/flint/blob/982c09aeb7dc1f590d0cfc29c678679fe4001137/web/apps/developers/src/generated/openapi.json) at Flint main `982c09aeb7dc1f590d0cfc29c678679fe4001137`. SHA-256: `721c43ac19311195389a5ee8d2b8058779f3f0c71d54738d9a3b606a768465eb`.

## Webhook signing

- `webhook-endpoints create` and `webhook-endpoints update` accept repeatable `--signing-method jwt|hmac` flags, mapped to `signing_methods`. Creation defaults to `["jwt"]`; a shared `secret` is returned only when `hmac` is included. Save the secret if present.
- JWT deliveries carry `Authorization: Bearer <token>`. Verify with [Flint's JWKS](https://api.withflintpay.com/.well-known/jwks.json), checking issuer, endpoint audience, expiry, and the raw body hash.
- Updating `signing_methods` replaces the set and requires `--expected-version`, like `--enabled-event`. Omit signing methods to leave them unchanged. Removing `hmac` deletes shared secrets. Adding `hmac` when no secret has been issued returns `409 WEBHOOK_SECRET_NOT_ISSUED`; `webhook-endpoints rotate-secret` issues the first secret and enables `hmac`, or rotates an existing secret.
- `listen --forward-to` continues signing locally with HMAC and its listen secret regardless of the registered endpoint's signing methods. JWT handlers need an HMAC verification path or a local test bypass while using `listen`. No local JWT signing key is created.

```sh
flint webhook-endpoints create --url https://example.com/webhooks/flint --enabled-event payment_intent.succeeded --signing-method jwt
flint webhook-endpoints update whep_123 --signing-method jwt --expected-version 3
flint webhook-endpoints rotate-secret whep_123 --confirm
```

## Other snapshot-driven command changes

Command names below use dots; invoke `tax-connection.get` as `flint tax-connection get`. MCP tool names replace dots with underscores.

New commands:

- `developer.partner.apps.credentials.create`, `.delete`, `.get`, and `.list`.
- `tax-connection.connect` and `.get`.
- `tax-connection.registrations.create`, `.delete`, `.get`, `.list`, and `.update`.

Removed commands:

- `me.subscriptions.billing-interval`, `.delivery`, `.payment-method`, `.quantity`, and `.reactivate`.
- `subscriptions.billing-schedule.update`, `subscriptions.payment-method`, and `subscriptions.reactivate`.

Use `me.subscriptions.update` or `subscriptions.update` with the current request schema for supported subscription changes. Inspect it with `flint schema input me.subscriptions.update` or `flint schema input subscriptions.update`; resume operations remain separate commands.
