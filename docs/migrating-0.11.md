# Migrating to Flint CLI 0.11.0

This is a breaking change from CLI 0.10.0: the embedded API snapshot was refreshed against the current public API, which renames and removes commands. Before 1.0, breaking changes bump the minor version.

The changes since `cli/v0.10.0` are in [#42](https://github.com/flint-pay/flint-cli/pull/42), [#43](https://github.com/flint-pay/flint-cli/pull/43), [#44](https://github.com/flint-pay/flint-cli/pull/44), [#45](https://github.com/flint-pay/flint-cli/pull/45), and [#46](https://github.com/flint-pay/flint-cli/pull/46).

## Commands and MCP tools

Command names below use dots; invoke `payment-reviews.list` as `flint payment-reviews list`. MCP tool names replace every dot with an underscore, such as `payment_reviews_list`.

| 0.10.0 | 0.11.0 |
| --- | --- |
| `customers.deletion-requests.create`, `customers.deletion-requests.get` | `customer-deletion-requests.create`, `customer-deletion-requests.get` |
| `inventory-counts.apply`, `inventory-counts.cancel` | `inventory-counts.transitions` |
| `inventory-reservations.commit`, `inventory-reservations.consume`, `inventory-reservations.release` | `inventory-reservations.transitions` |
| `invoices.manual-payments` | `invoices.manual-payments.create` |
| `me.payment-methods` | `me.payment-methods.create` |
| `reviews.approve`, `reviews.decline`, `reviews.get`, `reviews.list` | `payment-reviews.approve`, `payment-reviews.decline`, `payment-reviews.get`, `payment-reviews.list` |
| `sandboxes.test-key` | `sandboxes.api-keys.create` (`flint sandboxes test-key` still works as an alias) |

New commands: `files.get`, `inventory-reservations.update`, `invoices.manual-payments.get`, `invoices.manual-payments.list`, `me.session.revoke`, `merchant-billing-entries.list`, `returns.activities.list`, `subscriptions.change-plan`, `webhook-deliveries.create`, and `workload-identities.create`, `.get`, `.list`, `.update`, and `.delete`.

## Other changes

- `FLINT_WORKLOAD_IDENTITY_ID` authenticates with workload identity federation. `FLINT_CHECKOUT_SESSION_SECRET` and `FLINT_CHECKOUT_SESSION_ID` are no longer read.
- Fraud warning IDs use the `frw_` prefix, so the history reference is `@last.frw` instead of `@last.fw`.
- `flint sandboxes api-keys create` sends `POST /v1/developer/sandboxes/{sandbox_id}/api-keys`. Against a server that does not have that route yet, it retries the older `/test-key` route once with the same idempotency key.
- `flint signup` follows the onboarding next step's `action_type` and `owner`: it submits `advance_onboarding` to `POST /v1/onboarding/advance`, issues the key at `create_api_key`, and stops with `SIGNUP_HUMAN_ACTION_REQUIRED` for `create_merchant_account_session` or any step another party owns. It still accepts the older `code` and `submit_endpoint` fields, and no longer sends `sandbox_id` when issuing the initial key, which is always in the default sandbox.
