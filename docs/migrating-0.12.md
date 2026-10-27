# Migrating to Flint CLI 0.12.0

This is a breaking change from CLI 0.11.0: the embedded API snapshot was refreshed against the current public API, which renames and removes commands. Before 1.0, breaking changes bump the minor version.

The changes since `cli/v0.11.0` are in [#47](https://github.com/flint-pay/flint-cli/pull/47) and [#48](https://github.com/flint-pay/flint-cli/pull/48).

## Commands and MCP tools

Command names below use dots; invoke `payment-intents.capture` as `flint payment-intents capture`. MCP tool names replace every dot with an underscore, such as `payment_intents_capture`.

| 0.11.0 | 0.12.0 |
| --- | --- |
| `orders.payment-intents.capture`, `orders.payment-intents.cancel` | `payment-intents.capture`, `payment-intents.cancel`, which take only the payment intent ID |
| `invoices.late-fees.assess` | `invoices.late-fees.create` (`flint invoices late-fees assess` still works as an alias) |
| `merchant-account-sessions.refresh` | `merchant-account-session-refreshes.create`, which returns HTTP 200 instead of 201 |
| `orders.charges`, `orders.line-items` | `orders.charges.create`, `orders.line-items.create` |
| `payout-settings.destinations.list`, `.get`, `.update`, `.delete` | `payout-destinations.list`, `.get`, `.update`, `.delete` |
| `return-policies.revisions` | `return-policies.revisions.create` |
| `returns.line-items` | `returns.line-items.create` |
| `risk-lists.items` | `risk-lists.items.create` |
| `settings.custom-domains.validate DOMAIN_TYPE` | `custom-domains.validate cdom_...`, which takes the custom domain ID |
| `payment-methods.set-default` | `customers.update` with `default_payment_method_id` in the request body |
| `me.payment-methods.set-default` | `me.update` with `default_payment_method_id` in the request body |

New commands: `custom-domains.create`, `.get`, `.list`, and `.delete`, `developer.partner.apps.delete`, `me.fulfillments.get`, `me.packages.get`, `me.payments.get`, `me.refunds.get`, `me.return-resolutions.get`, `me.shipments.get`, `me.subscriptions.update`, `me.subscriptions.payment-retries.list`, `partner-app-installs.list`, and `partner-app-installs.revoke`.

## Other changes

- `flint organizations create` requires `--parent-organization org_...` (or `parent_organization_id` in `--input`), since the API now requires a parent organization.
- `flint api-keys revoke` accepts `--expected-version` and `--input`, and `flint webhook-endpoints update` accepts `--expected-version`. The API requires `expected_version` when an update changes `enabled_events`, `event_sources`, or `installed_merchant_modes`.
- `flint sandboxes api-keys create` sends only `POST /v1/developer/sandboxes/{sandbox_id}/api-keys`; the fallback to the retired `/test-key` route is gone. `flint sandboxes test-key` remains an alias.
