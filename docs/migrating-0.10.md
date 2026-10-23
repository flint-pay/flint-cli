# Migrating to Flint CLI 0.10.0

This is a breaking change from CLI 0.9.0. Use 0.10.0, rather than 0.9.1, because writes with required idempotency keys now reject calls that omit the key, and retry behavior changes.

The changes since `cli/v0.9.0` are in [#40](https://github.com/flint-pay/flint-cli/pull/40). The embedded API snapshot and coverage report are unchanged.

## Commands and MCP tools

Command and MCP tool names stay the same. The following commands now require `--idempotency-key KEY`. Command names below use dots; invoke `orders.pay` as `flint orders pay`. MCP tool names replace every dot with an underscore, such as `orders_pay`.

| Command group | Actions requiring a key |
| --- | --- |
| `credit-notes` | `allocations.create`, `issue`, `refunds.create` |
| `customer-sessions` | `refresh` |
| `gift-card-funding-dispositions` | `create` |
| `gift-card-redemptions` | `create` |
| `gift-cards` | `create`, `adjust-balance`, `cash-out`, `loads.create` |
| `inventory-adjustments` | `create` |
| `inventory-receipts` | `create` |
| `inventory-reservations` | `create` |
| `inventory-transfers` | `create` |
| `invoices` | `create`, `collect`, `manual-payments`, `manual-payments.reverse` |
| `me.returns` | `create` |
| `me.subscriptions` | `payment-retries.create`, `renew` |
| `orders` | `pay`, `payment-intents.create` |
| `payment-intents` | `create` |
| `payouts` | `create` |
| `refunds` | `create` |
| `returns` | `create`, `dispositions.create`, `inspections.create`, `process`, `receipts.create` |
| `subscriptions` | `create`, `payment-retries.create`, `renew` |

The requirement also applies to aliases, `payment-intents create --order`, and raw `flint api` calls to the same routes. It comes from each operation's required `Idempotency-Key` header in the embedded API contract.

## Request and response migration

- For required-key writes, without a nonempty `--idempotency-key`, the CLI exits 2 with `IDEMPOTENCY_KEY_REQUIRED` before sending any request, including authentication requests. This also applies to `--dry-run=client`. Save a key with each action before its first call, and reuse that key and request on retry. Use a new key for a new action.
- Writes with optional keys, such as `customers create`, keep generating a `flint-cli-…` key when you omit one. The CLI reuses it across its own retries. A later invocation generates a new key, so pass your saved key explicitly when retrying from a script.
- Operations with no `Idempotency-Key` header in their contract send no key, even if you supply one. Examples include `oauth token` and `discount-previews create`. OAuth protocol POST operations are sent once. `oauth authorize` still requires live acknowledgement and production confirmation when applicable.
- Every error from a keyed write includes `error.details.idempotency_key`, rather than only network failures and 5xx responses. Reuse that key when retrying the same request. A valid `Retry-After` response header is also reported as `error.retry_after_ms` in JSON errors, including response-read failures. Existing server details and remediation are preserved.

For example, save `checkout-payment-1` with the payment action before running:

```sh
flint payment-intents create --amount 2500 --currency USD --payment-option card --idempotency-key checkout-payment-1
```

If the command times out, retry with the same arguments and `checkout-payment-1`. Do not generate a fresh key for each attempt.

## Retry rules

- Reads and keyed writes make at most 3 attempts within `--timeout`. Writes that take no key are sent once.
- `error.retryable` reports whether sending the same request again is safe and may succeed. For reads and keyed writes, no usable response or Flint's `remediation.retry: "same_request"` permits retrying. This replaces 0.9.0's transport/status convenience flag for 429, 5xx, network errors, and timeouts.
- A Flint error envelope controls the decision through `remediation.retry`, including on 409 or 5xx responses. `after_refresh`, `none`, or a missing retry instruction does not trigger an automatic retry. Follow the returned remediation before sending another request.
- Responses without a Flint error envelope retry on HTTP 429, 500, 502, 503, or 504, subject to the same replay-safety and attempt limits.
- Retries honor `Retry-After` and otherwise use jittered backoff. If the delay would exhaust `--timeout`, the CLI returns the error instead. Cancellation stops retrying.

Update scripts that retry every 429 or 5xx response to inspect `error.retryable` and the returned remediation. A retryable error does not extend the CLI's attempt limit or timeout.

## MCP migration

Pass the saved key as `_flint.idempotency_key` in tool arguments. Required routes now require both `_flint` and its nonempty `idempotency_key` in the tool input schema. Omitting the key is rejected before sending, including with `_flint.dry_run: "client"`; the same required-key validation applies through MCP.

```json
{
  "amount_money": { "amount": 2500, "currency": "USD" },
  "payment_options": ["card"],
  "_flint": {
    "idempotency_key": "checkout-payment-1",
    "dry_run": "client"
  }
}
```

The `_flint.idempotency_key` description no longer says that the server generates omitted keys. The CLI generates keys only for optional routes; the API never generates them. Errors from keyed writes include the key. Preview output no longer requires an `Idempotency-Key` header when the operation takes no key.
