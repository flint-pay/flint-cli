# Migrating to Flint CLI 0.13.0

This snapshot refresh removes subscription action commands, so it requires a minor version bump before 1.0. Package versions are stamped during release preparation; this change does not publish a release.

The embedded snapshot is copied byte-for-byte from [`web/apps/developers/src/generated/openapi.json`](https://github.com/flint-pay/flint/blob/2c72e968fc4bc14005de219cd7a7b3ae4d168a58/web/apps/developers/src/generated/openapi.json) at Flint main `2c72e968fc4bc14005de219cd7a7b3ae4d168a58`. SHA-256: `429475c94aba920a8ce9f0a29b8a7d14b653eee8864258fb900b94058aabfc41`.

## Webhook signing

- `webhook-endpoints create` and `webhook-endpoints update` accept repeatable `--signing-method jwt|hmac` flags, mapped to `signing_methods`. Creation defaults to `["jwt"]`; a shared `secret` is returned only when `hmac` is included. Save the secret if present.
- JWT deliveries carry `Authorization: Bearer <token>`. Verify with [Flint's JWKS](https://api.withflintpay.com/.well-known/jwks.json), checking issuer, endpoint audience, expiry, and the raw body hash.
- Updating `signing_methods` replaces the set and requires `--expected-version`, like `--enabled-event`. Omit signing methods to leave them unchanged. Removing `hmac` deletes shared secrets. Adding `hmac` when no secret has been issued returns `409 ACTION_NOT_ALLOWED` with `details[0].reason` `secret_not_issued` and param `signing_methods`; `webhook-endpoints rotate-secret` issues the first secret and enables `hmac`, or rotates an existing secret.
- `listen --forward-to` continues signing locally with HMAC and its listen secret regardless of the registered endpoint's signing methods. JWT handlers need an HMAC verification path or a local test bypass while using `listen`. No local JWT signing key is created.

```sh
flint webhook-endpoints create --url https://example.com/webhooks/flint --enabled-event payment_intent.succeeded --signing-method jwt
flint webhook-endpoints update whep_123 --signing-method jwt --expected-version 3
flint webhook-endpoints rotate-secret whep_123 --confirm
```

## Delivery rate callback signing

- `delivery-rate-callbacks create` and `delivery-rate-callbacks update` accept the same repeatable `--signing-method jwt|hmac` flags, mapped to `signing_methods`. Creation defaults to `["jwt"]`; `secret` and `key_id` are returned only when `hmac` is included. Save them if present.
- With `jwt`, callback requests carry `Authorization: Bearer <token>`. Verify it with [Flint's JWKS](https://api.withflintpay.com/.well-known/jwks.json), checking that `aud` is the callback's `dcb_` ID and that `body_sha256` matches the exact request bytes. With `hmac`, requests carry `webhook-signature` and `Flint-Signing-Key-Ids`.
- Updating `signing_methods` replaces the set and requires `--expected-version`, like a configuration change. Omit signing methods to leave them unchanged. Removing `hmac` deletes shared secrets. Adding `hmac` when no secret has been issued returns the same `409 ACTION_NOT_ALLOWED` with reason `secret_not_issued`; `delivery-rate-callbacks rotate-secret` issues the first secret and enables `hmac`, or rotates an existing secret.

```sh
flint delivery-rate-callbacks create --input callback.json --signing-method jwt
flint delivery-rate-callbacks update dcb_123 --signing-method jwt --signing-method hmac --expected-version 3
flint delivery-rate-callbacks rotate-secret dcb_123 --confirm
```

## API key access

API keys now declare `access`: `full`, `read_only`, or `restricted`. `scope_preset` is gone from API key and sandbox request bodies, and `scopes` is allowed only with `restricted`.

- `api-keys create` requires `--access`; `--scope` is now optional and valid only with `--access restricted`. For example, `flint api-keys create --name local-dev --access restricted --scope payments.payment_intents.read` or `flint api-keys create --name ci-readonly --access read_only`.
- `sandboxes api-keys create` and `sandboxes create` with `issue_test_key` take `access` in the `--input` body instead of `scope_preset`.
- `api-keys update` accepts `access`; changing `access` or `scopes` requires `expected_version`. The command is now classified as a sensitive write, so live-mode updates need `--confirm`.

## Other snapshot-driven command changes

Command names below use dots; invoke `tax-connection.get` as `flint tax-connection get`. MCP tool names replace dots with underscores.

New commands:

- `disputes.accept`, which accepts a chargeback without contesting it. It can't be undone, so it requires `--confirm`.
- `developer.partner.apps.credentials.create`, `.delete`, `.get`, and `.list`.
- `tax-connection.connect` and `.get`.
- `tax-connection.registrations.create`, `.delete`, `.get`, `.list`, and `.update`.

Removed commands:

- `me.subscriptions.billing-interval`, `.delivery`, `.payment-method`, `.quantity`, and `.reactivate`.
- `subscriptions.billing-schedule.update`, `subscriptions.payment-method`, and `subscriptions.reactivate`.
- `customers.addresses.set-default` and `me.addresses.set-default`.

Use `me.subscriptions.update` or `subscriptions.update` with the current request schema for supported subscription changes. Inspect it with `flint schema input me.subscriptions.update` or `flint schema input subscriptions.update`; resume operations remain separate commands.

Set default addresses with `customers.addresses.update` or `me.addresses.update` and `is_default_billing` or `is_default_shipping` in the request body; `customers.addresses.create` and `me.addresses.create` accept the same fields. `customers.create` and `customers.update` no longer accept `billing_address` or `shipping_address`; save addresses through `customers.addresses.create` instead.
