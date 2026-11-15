# Migrating to 0.20.0

0.20.0 removes subscription quantity multipliers. Before 1.0, breaking flag and contract changes require a minor version bump, so this release advances from 0.19.0 to 0.20.0.

## Subscription seats

A seat count is the quantity of a subscription line. Remove `quantity` from subscription create terms, updates, plan changes, and previews. Remove `quantity_options` from plan creation and updates. Remove `subscription_terms.quantity` from checkout input. Remove their corresponding `--quantity` and `--quantity-options` flags, including requests that previously sent 1 or `[1]`.

Subscription, plan, checkout subscription terms, pending plan changes, and payment-link subscription previews no longer return multiplier fields. Read each line's `quantity` instead. Plan lines expose `allow_quantity_adjustment`, `min_quantity`, and `max_quantity` for buyer quantity changes.

To change an existing subscription, use its subscription line ID (`sli_...`):

```sh
flint subscriptions change-quantity sub_123 \
  --billing-starts period_end \
  --line-item sli_01KEZF7C00PPWVJ2N321V84ASA=137 \
  --line-item sli_01KEZF7C00PPWVJ2N321V84ASB=2 \
  --idempotency-key seats-change-1
```

Repeat `--line-item` for 1 to 50 distinct lines, with whole-number quantities from 1 to 9,999. Omitted lines keep their quantities. The flags replace the complete `line_items` array from `--input`. Use `--expected-version` to reject a change if the subscription has changed since you read it.

`prorated_now` charges the prorated difference now, `next_renewal` changes quantities now without charging, and `period_end` schedules quantities for the next billing boundary. Buyer `flint me subscriptions change-quantity` accepts the same repeated flags with `period_end` or eligible `prorated_now`; the API enforces the frozen adjustable line and its bounds.

The equivalent JSON input and MCP request contains:

```json
{
  "billing_starts": "period_end",
  "line_items": [
    {"subscription_line_item_id": "sli_01KEZF7C00PPWVJ2N321V84ASA", "quantity": 137}
  ]
}
```

Use `subscription_line_item_id` for existing lines when changing quantities or previewing a quantity change. At signup, subscription create `terms.line_items`, checkout `subscription_terms.line_items`, and payment-link resolution `subscription_terms.line_items` use `subscription_plan_line_item_id` to select a plan line. Pass these structured fields with `--input` or through MCP. Direct subscription line-item updates no longer accept `quantity`; move those updates to `change-quantity` and choose the billing timing explicitly.

Line quantities on orders, plan lines, payment-link lines, and other commerce items retain their existing meaning.

## Other public snapshot changes

Buyer email-change requests now have list/get commands under `me email-change-requests`. Customer access-link creation accepts delivery and expiry options. Payment intent responses include `invoice_schedule_entry_id`, and order payment attempts include `order_id`. Billing balances and readiness responses include overdue balances and upcoming requirements.

Merchant Return responses omit `buyer_actions`. Buyer Return, Return line-item, and Return resolution responses omit `supported_actions`; update integrations to the current buyer task and lifecycle fields. Consult `flint schema output COMMAND --output json` for the response shape of each affected command.

## Payment client actions

Payment confirmation now defaults `client_action_type` to `url`. This affects `payment-intents confirm` and the `orders pay` payment, setup, and payment-intent confirmation variants. If your integration expects a Stripe client secret, explicitly send `client_action_type: "stripe"` at the same level as that variant's other payment options. For `payment-intents confirm`, you can use `--client-action-type stripe`. Otherwise, handle the returned URL client action and send the buyer to that URL to complete authentication.
