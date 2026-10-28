# Migrating to Flint CLI 0.13.0

This release moves every command to Flint's canonical API paths and removes or renames commands, so it requires a minor version bump before 1.0.

The embedded snapshot is copied byte-for-byte from [`web/apps/developers/src/generated/openapi.json`](https://github.com/flint-pay/flint/blob/0f83faf086dd659e279da39ae653918b38450ae5/web/apps/developers/src/generated/openapi.json) at Flint main `0f83faf086dd659e279da39ae653918b38450ae5`. SHA-256: `e8fff8a6422b34e3142deea2983568beb1a7cccc697f02368b4931e331c6dd1e`.

## Canonical API paths

Every command now calls the canonical paths, which the Flint API serves before it removes the old ones. CLI 0.13.0 needs a Flint API that serves them; earlier CLI versions keep working until the old paths are removed, so upgrade before then.

- Every command checks the credential with `GET /v1/auth-context` instead of `GET /v1/developer/auth-context`, including `flint auth status` and `flint whoami`.
- `flint sandboxes` commands call `/v1/sandboxes`, `flint request-logs` calls `/v1/api-request-logs`, and `flint timeline` calls `/v1/resource-timelines/{resource_id}`. Their command names don't change.
- Commands that send a bot-check token, such as `demo-sessions create`, read it from `FLINT_CHALLENGE_TOKEN` and send it as `Flint-Challenge-Token`. `FLINT_TURNSTILE_TOKEN` and `X-Turnstile-Token` are gone.

Renamed commands, written with dots; invoke `partner-apps.list` as `flint partner-apps list`. MCP tool names replace dots with underscores.

| 0.12.0 | 0.13.0 |
| --- | --- |
| `developer.partner.apps.*`, including `.credentials.*`, `.installs.*`, and `.rotate-secret` | `partner-apps.*`, such as `partner-apps.list` and `partner-apps.installs.revoke` |
| `developer.partner.installs.list` | Removed with its route; list one app's installs with `partner-apps.installs.list` |
| `invoices.checkout-session` | `invoices.checkout-sessions.create` |
| `return-resolutions.checkout-session` | `return-resolutions.checkout-sessions.create` |
| `me.invoices.checkout-session.create` | `me.invoices.checkout-sessions.create` |
| `me.return-resolutions.checkout-session.create` | `me.return-resolutions.checkout-sessions.create` |
| `me.payments.list`, `me.payments.get` | `me.payment-intents.list`, `me.payment-intents.get` |
| `me.flint-wallet.payment-methods.list` | `me.flint-wallet.cards.list`, which returns `flint_wallet_card_id` with nested `card` details |
| `me.flint-wallet.payment-methods.store-setups.create ID` | `me.flint-wallet.cards.store-setups.create FLINT_WALLET_CARD_ID` |
| `demo-sessions.reset` | `demo-sessions.reset DEMO_SESSION_ID`, which names the session to reset |

New commands: `checkout-sessions.delivery-selections.list`, `delivery-revocations.list`, and `tax-connection.disconnect`.

## Union tags in request bodies

Request bodies that accept alternatives now require their tag. Convenience flags set it for you; JSON from `--input` must include it, and a flag that conflicts with the tag in `--input` fails with `CONFLICTING_ARGUMENTS`.

- `payment-intents confirm` sets `credential_type`: `stripe_confirmation_token` with `--confirmation-token`, `token` with `--payment-source-token`, and `payment_method` with `--payment-method`. Without a credential flag or `--input`, it sends `attached` to continue with the payment source already attached, such as after the buyer completes `current_payment_action`.
- `orders pay --payment-source-token` sends `credential_type: token` on each `payment_intents` entry. With `--input`, each `payment_intents` entry and `payment_source` needs its own `credential_type`.
- `payment-links create --item-name/--amount/--currency` sends a `custom_fixed` line item. Line items in `--input` need `type`: `variant`, `bundle`, `custom_fixed`, or `custom_variable`.
- `invoices create --order` sends `type: order`. Quick-pay invoices take `type: quick_pay` in `--input`.
- Payment link line item writes reject `resolved_name`, `resolved_unit_price_money`, `resolved_price_range`, `is_catalog_item_unavailable`, and `product_id`. Remove them before sending a fetched line item back.

Run `flint schema input COMMAND` to see each body's alternatives.

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
- `partner-apps.credentials.create`, `.delete`, `.get`, and `.list`.
- `tax-connection.connect` and `.get`.
- `tax-connection.registrations.create`, `.delete`, `.get`, `.list`, and `.update`.

Removed commands:

- `me.subscriptions.billing-interval`, `.delivery`, `.payment-method`, `.quantity`, and `.reactivate`.
- `subscriptions.billing-schedule.update`, `subscriptions.payment-method`, and `subscriptions.reactivate`.
- `customers.addresses.set-default` and `me.addresses.set-default`.

Use `me.subscriptions.update` or `subscriptions.update` with the current request schema for supported subscription changes. Inspect it with `flint schema input me.subscriptions.update` or `flint schema input subscriptions.update`; resume operations remain separate commands.

Set default addresses with `customers.addresses.update` or `me.addresses.update` and `is_default_billing` or `is_default_shipping` in the request body; `customers.addresses.create` and `me.addresses.create` accept the same fields. `customers.create` and `customers.update` no longer accept `billing_address` or `shipping_address`; save addresses through `customers.addresses.create` instead.
