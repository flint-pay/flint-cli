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

## Additions and relaxations

The refreshed catalog adds:

- `customers email-change-requests list`, `create`, `get`, and `cancel`;
- `invoices late-fees list`;
- `me remembered-buyers list` and `revoke`;
- `subscriptions line-items list`.

Buyer `delivery_options` subscription previews no longer require `destination`. Buyer `next_renewal` previews add `swap`, with required `subscription_line_item_id` and `variant_id`. A preview cannot combine `swap` and `delivery`. `swap` was absent from the old request schema.

Use `flint schema command COMMAND --output json` and `flint schema input COMMAND` to inspect each command's current flags, credential requirements, and complete input schema.
