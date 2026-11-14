# Migrating to 0.19.0

0.19.0 refreshes the embedded public API snapshot to the Flint commit pinned in `internal/spec/provenance.json`. Before 1.0, breaking changes require a minor version bump. This release changes buyer credit-note commands, request version fields, and subscription quantity constraints.

## Buyer credit notes

Buyer credit notes now use the top-level `/v1/me/credit-notes` collection. The old invoice-nested routes and commands are removed, with no compatibility aliases.

| Before | After |
| --- | --- |
| `flint me invoices credit-notes list INV_ID` | `flint me credit-notes list --invoice-id INV_ID` |
| `flint me invoices credit-notes get INV_ID CN_ID` | `flint me credit-notes get CN_ID` |
| `flint me invoices credit-notes pdf get INV_ID CN_ID --save-to credit-note.pdf` | `flint me credit-notes pdf get CN_ID --save-to credit-note.pdf` |

The new list command's `--invoice-id` is optional. Keep it to preserve invoice-scoped listing; omit it to list across the buyer's invoices. Get and PDF commands take only the credit-note ID. Buyer-session authentication is unchanged.

For MCP, replace `me.invoices.credit-notes.list`, `me.invoices.credit-notes.get`, and `me.invoices.credit-notes.pdf.get` with `me.credit-notes.list`, `me.credit-notes.get`, and `me.credit-notes.pdf.get`. Typed tool names change from `me_invoices_credit_notes_*` to `me_credit_notes_*`. List callers can retain `invoice_id` as a filter; get and PDF callers must remove `invoice_id`.

## Order payment intents and return children

The nested order payment-intent create command is removed. Both standalone and order-owned payments use `POST /v1/payment-intents`. The order-owned branch requires `payments.payment_intents.write`, replacing the old `commerce.orders.write` requirement.

| Before | After |
| --- | --- |
| `flint orders payment-intents create ORDER_ID` | `flint payment-intents create --order-id ORDER_ID` |
| `flint returns inspections create RETURN_ID --input inspection.json` | `flint return-inspections create --return-id RETURN_ID --input inspection.json` |
| `flint returns receipts create RETURN_ID --input receipt.json` | `flint return-receipts create --return-id RETURN_ID --input receipt.json` |
| `flint returns dispositions create RETURN_ID --input disposition.json` | `flint return-dispositions create --return-id RETURN_ID --input disposition.json` |
| `flint returns resolutions create RETURN_ID --input resolution.json` | `flint return-resolutions create --return-id RETURN_ID --input resolution.json` |

Keep each mutation's idempotency key and other input fields. Return-child creation now sends required `return_id` in the body. Existing child list/get commands keep their names and filters. For order-owned payment intents, `amount_money` and `payment_options` remain optional; do not add the standalone-only `customer_id`, `receipt_email`, `tip_money`, or `transaction_purpose`. Keep an existing `expected_order_version` fence. `payment_source_selection` cannot accompany nonempty `payment_options`.

MCP callers replace `orders.payment-intents.create` with `payment-intents.create` and retain `order_id` in the input. Replace `returns.inspections.create`, `returns.receipts.create`, `returns.dispositions.create`, and `returns.resolutions.create` with the corresponding `return-inspections.create`, `return-receipts.create`, `return-dispositions.create`, and `return-resolutions.create` operations. Typed tool names follow these replacements with underscores.

## Payment credentials

`payment-intents confirm` now places supplied credentials inside `payment_source`. JSON and MCP callers must make these replacements:

| Before body fragment | After body fragment |
| --- | --- |
| `"credential_type":"token","payment_source_token":"pm_card_visa"` | `"payment_source":{"credential_type":"stripe_payment_method","stripe":{"payment_method":"pm_card_visa"}}` |
| `"credential_type":"stripe_confirmation_token","stripe":{"confirmation_token":"ctoken_example"}` | `"payment_source":{"credential_type":"stripe_confirmation_token","stripe":{"confirmation_token":"ctoken_example"}}` |
| `"credential_type":"payment_method","payment_method_id":"PM_ID"` | `"payment_source":{"credential_type":"payment_method","payment_method_id":"PM_ID"}` |
| `"credential_type":"reader","reader_id":"READER_ID"` | `"payment_source":{"credential_type":"reader","reader_id":"READER_ID"}` |
| `"credential_type":"attached"` | Omit `payment_source` to continue with the attached source. |

The terminal shortcuts `--confirmation-token`, `--payment-method-id`, and `--payment-source-token` write these nested forms. The first uses a Stripe confirmation token, the second a Flint saved payment method, and the third a Stripe test payment method such as `pm_card_visa`. Remove `--credential-type attached`; `flint payment-intents confirm PI_ID` now sends an empty body for continuation. If no source is attached, supply one.

For `orders pay` with `action: confirm_payment_intents`, each `payment_intents` entry retains `payment_intent_id` and nests any supplied credential under `payment_source`. Replace `{"credential_type":"attached","payment_intent_id":"PI_ID"}` with `{"payment_intent_id":"PI_ID"}`. Replace a token entry with `{"payment_intent_id":"PI_ID","payment_source":{"credential_type":"stripe_payment_method","stripe":{"payment_method":"pm_card_visa"}}}`. `--payment-source-token PI_ID=pm_card_visa` writes this new form.

For `orders pay` with `action: pay` or `action: setup`, replace `{"credential_type":"token","token":"pm_card_visa"}` inside `payment_source` or `setup_payment_source` with `{"credential_type":"stripe_payment_method","stripe":{"payment_method":"pm_card_visa"}}`. A reader credential cannot save a payment method or include buyer IP/return URL/client-action options. On confirmation, `save_payment_method: true` needs a newly collected Stripe card credential; omit it for an attached or saved-method continuation.

## Request version fields

Replace `expected_version` in JSON input and MCP calls with the field below. Replace `--expected-version` with its corresponding flag for terminal commands. These names identify the parent resource whose version is checked.

| Command | New JSON field | New flag |
| --- | --- | --- |
| `returns line-items create RETURN_ID` | `expected_return_version` | `--expected-return-version` |
| `credit-notes allocations create CN_ID` | `expected_credit_note_version` | `--expected-credit-note-version` |
| `invoices manual-payments create INV_ID` | `expected_invoice_version` | `--expected-invoice-version` |
| `subscriptions line-items create SUB_ID` | `expected_subscription_version` | `--expected-subscription-version` |
| `return-policies revisions create POLICY_ID` | `expected_return_policy_version` | `--expected-return-policy-version` |

For example, replace `"expected_version":3` with `"expected_invoice_version":3` when recording an invoice manual payment. Return-policy revision creation still requires its version field. The other four fields remain optional. Variant and bundle subscription line-item inputs use the same renamed subscription field.

## Subscription quantities

The 0.18.0 embedded snapshot allowed subscription multipliers from 1 to 100. The refreshed schema limits the multiplier to 1. Use 1, or omit the field where optional, for:

- `terms.quantity` in subscription creation and `subscription.terms.quantity` in creation previews;
- `quantity` in merchant and buyer subscription updates and plan changes, merchant quantity changes, and their preview variants;
- `subscription_terms.quantity` in checkout-session creation and updates.

For example, replace `"terms":{"type":"subscription_plan","subscription_plan_id":"PLAN_ID","quantity":3}` with `"terms":{"type":"subscription_plan","subscription_plan_id":"PLAN_ID","quantity":1}` when creating a subscription. Set units on the individual subscription line items instead. Line-item quantities remain independent of the subscription multiplier.

For `subscription-plans create` and `subscription-plans update`, the old embedded schema allowed up to ten `quantity_options`, each at most 100. The new schema accepts an omitted field, `[]`, or `[1]`. Replace values such as `"quantity_options":[1,2,3]` with `"quantity_options":[1]`, or remove the field. These comparisons describe the CLI snapshots; they do not imply an older deployed server accepted every value allowed by the old snapshot.

## Other request and filter changes

- For `invoices list` and `me invoices list`, replace `--sort-by outstanding_money` with `--sort-by settlement_amounts.outstanding_money`.
- Return resolution filters rename `fulfilled` to `completed` and `partially_fulfilled` to `partially_completed`: update `--resolution-status` on `returns list` and `me returns list`, `--resolution-status fulfilled` to `--resolution-status completed` on `returns line-items list`, and `--status fulfilled` to `--status completed` on `return-resolutions list`.
- Replace the webhook event `invoice_payment_attempt.settled` with `invoice_payment_attempt.succeeded` in event filters, listener subscriptions, and `webhook-endpoints test-events create --event-type`.
- Return-policy revisions require nonnegative `priority`. Scope arrays allow at most 100 unique entries; channel, location, product, and variant entries have a maximum length of 255. Deduplicate and shorten inputs before sending them. A percentage restocking fee is from 1 through 100; a return window's `duration_seconds` is from 1 through 158112000. These constraints were absent from the 0.18.0 embedded schema.

## Custom-line product types

Custom lines add optional `product_type`: `physical`, `digital`, `service`, or `fee`. Omission keeps them unclassified; catalog variant and bundle lines reject this field. Set it when the line's classification should determine its ACH purpose.

`orders create`, `orders line-items create`, `invoices create` for quick-pay invoices, and `payment-links create`/`update` expose `--product-type` for the first custom line. `orders line-items update` exposes the same flag for the addressed line. Supply other required fields through `--input`; use JSON `product_type` on each line when changing multiple lines, custom variable-price payment links, order edits, or previews. For payment-link updates, retain the required `--expected-version` fence. Issued invoice snapshots stay frozen; void and reissue an invoice to change its classification.

For example, `flint orders line-items create ORDER_ID --input custom-lines.json --product-type service` sets `line_items[0].product_type`. On `invoices create`, the field is `quick_pay.line_items[0].product_type`. On payment links it is `line_items[0].product_type`; both `custom_fixed` and `custom_variable` inputs accept it.

## Additions and relaxations

The refreshed catalog adds:

- `customers email-change-requests list`, `create`, `get`, and `cancel`;
- `invoices late-fees list`;
- `me remembered-buyers list` and `revoke`;
- `subscriptions line-items list`.

Buyer `delivery_options` subscription previews no longer require `destination`. Buyer `next_renewal` previews add `swap`, with required `subscription_line_item_id` and `variant_id`. A preview cannot combine `swap` and `delivery`. `swap` was absent from the old request schema.

Use `flint schema command COMMAND --output json` and `flint schema input COMMAND` to inspect each command's current flags, credential requirements, and complete input schema.
