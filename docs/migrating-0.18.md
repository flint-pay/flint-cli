# Migrating to 0.18.0

0.18.0 refreshes the embedded API snapshot to the Flint commit pinned in `internal/spec/provenance.json`. This minor release includes breaking changes to generated commands, request schemas, and local scope errors.

## API keys and login

Mint sandbox keys with the single key route:

```sh
flint api-keys create --sandbox-id test_01KEZF7C00JVWYYN2SBWFFSA36 --name sandbox-dev --access restricted --scopes customers.read --live
```

Use a credential authorized to mint keys for that sandbox. `--live` acknowledges a live caller credential; the new key is still a test key because `sandbox_id` selects its sandbox. When the active credential already belongs to the target sandbox, omit `--live`.

`flint sandboxes api-keys create` and `flint onboarding api-key create` are removed. MCP callers use `api-keys.create` through `flint_write`, with `sandbox_id` in the input. The typed tool is `api_keys_create`. The retired onboarding and sandbox mint tools are no longer listed.

Login keeps its client-owned default scopes and sends the public `mode` field (`test` or `live`) for device authorization and reauthorization. Context lists prefer `mode` and still accept the older `environment` field. Onboarding-session and partner-app auth contexts may omit scopes.

## Other removed commands

The refreshed server snapshot also removes the commands below. Inspect current operations with `flint schema commands --output json` and their input with `flint schema input COMMAND` before updating scripts. MCP operation IDs use the same dotted names.

- `flint checkout-sessions delivery-quotes create`
- `flint checkout-sessions delivery-quotes get`
- `flint checkout-sessions delivery-selections create`
- `flint checkout-sessions delivery-selections current delete`
- `flint checkout-sessions delivery-selections current get`
- `flint checkout-sessions delivery-selections get`
- `flint checkout-sessions delivery-selections list`
- `flint fulfillments events create`
- `flint fulfillments shipments create`
- `flint inventory-counts transitions create`
- `flint inventory-reservations transitions create`
- `flint inventory-transfers transitions create`
- `flint invoices checkout-sessions create`
- `flint me invoices checkout-sessions create`
- `flint me return-resolutions checkout-sessions create`
- `flint orders delivery-selections current get`
- `flint orders fulfillments create`
- `flint organizations create`
- `flint organizations delete`
- `flint organizations get`
- `flint organizations list`
- `flint organizations memberships create`
- `flint organizations memberships delete`
- `flint organizations memberships list`
- `flint organizations update`
- `flint return-resolutions checkout-sessions create`
- `flint shipments packages create`

Create a quote with `flint delivery-quotes create --checkout-session-id CS_ID`, then read it with `flint delivery-quotes get DQUOTE_ID`. Create a choice with `flint delivery-selections create --delivery-quote-id DQUOTE_ID --input choices.json`. Use `flint delivery-selections get DSEL_ID` or `flint delivery-selections list --checkout-session-id CS_ID` to read choices. Replace current-selection deletion with `flint delivery-selections release DSEL_ID --confirm`. Read `flint checkout-sessions get CS_ID` for `delivery.delivery_selection_id`; there is no current-selection command. Organization administration has moved out of the merchant credential API.

## Request migrations

The table compares the 0.17.x request surface with 0.18.0. JSON examples are fragments of the body supplied through `--input`; keep the command's other required fields. MCP callers use the same public JSON fields.

| Command or input | Before | After |
| --- | --- | --- |
| `checkout-sessions create` | `--surface embedded` or `"surface":"embedded"` | Use `--surface headless` or `"surface":"headless"`. The existing headless rules still apply: omit `redirects`, and provide `payment_return_url` when enabling Affirm. Hosted checkout still uses `hosted`. |
| `checkout-sessions create --type quick_pay` or `--type subscription_plan` | `inventory_reservation_ids` could accompany these variants. | Remove it. Only the order variant accepts `--inventory-reservation-ids`. Do not mix the new invoice or return-resolution fields into order, quick-pay, or subscription-plan bodies. |
| `orders line-items create ORDER_ID` and `orders charges create ORDER_ID` | `--expected-version 3` or `"expected_version":3` | Use `--expected-order-version 3` or `"expected_order_version":3`. |
| `return-policies create` and `return-policies revisions create POLICY_ID` | `"revision":{"eligibility_result":"ineligible"}` could omit a reason. | Include `"revision":{"eligibility_result":"ineligible","ineligibility_reason":"final_sale"}`. Other allowed reasons are `product_excluded`, `category_excluded`, `market_restricted`, `channel_restricted`, `outside_return_window`, and `other`. |
| Returnable policy revisions | `ineligibility_reason` could accompany `eligibility_result: "eligible"` or `"review_required"`. | Remove `ineligibility_reason` for these variants; it belongs only to `ineligible`. |
| `delivery-previews create` | A body could include `pricing_context`. | Remove `pricing_context`. For caller-selected delivery tiers, create a checkout delivery quote with `delivery-quotes create --checkout-session-id CS_ID --tier-key 10` and configure the method's tier basis as `caller.tier_key`. |
| `fulfillments update FUL_ID` | Pickup or local-delivery details could include `expires_at`. | Use `pickup_details.pickup_due_at` or `local_delivery_details.delivery_due_at`, respectively. For example, replace `"pickup_details":{"expires_at":"2026-12-01T16:00:00Z"}` with `"pickup_details":{"pickup_due_at":"2026-12-01T16:00:00Z"}`. Omit a due date when leaving it unchanged. |
| `settings update` | `"invoices":{"timezone":"America/New_York"}` | Remove `invoices.timezone`. Set the merchant timezone with `flint merchant update --timezone America/New_York`; new invoices use the merchant timezone, falling back to UTC. |
| API key create/update and workload identity create/update scopes | `accounts.organizations.read` and `accounts.organizations.write` | Remove these retired scopes from scope lists. Organization commands are unavailable through the merchant API. Remove `accounts.organizations.read` from sandbox test-key scope lists too. |
| `sandboxes create` without test-key issuance | `test_key_expires_at` could appear while `issue_test_key` was false or omitted. | Remove `test_key_expires_at`, or request a test key with `issue_test_key: true` and its required access settings. |
| `webhook-endpoints test-events create` | `delivery_location_set.activated` / `.deactivated`, `delivery_profile.activated` / `.deactivated`, or `delivery_zone.activated` / `.deactivated` | Use the corresponding `.updated` event type, such as `--event-type delivery_zone.updated`. |
| `partner-apps create` and `partner-apps update APP_ID` | Names longer than 100 characters were accepted by the snapshot. | Shorten `--name` to at most 100 characters. |
| `risk-rules create` and `risk-rules update RULE_ID` | Descriptions longer than 512 characters were accepted by the snapshot. | Shorten `--description` to at most 512 characters. |
| `payment-method-domains create` | `domain_name` had no hostname pattern in the snapshot. | Use a dotted hostname such as `--domain-name checkout.example.com`, without a URL scheme, path, port, or wildcard. Each label must start and end with a letter or digit and contain at most 63 characters. |
| Delivery method rate tables, including method overrides in `subscription-previews create` | A rate table could have zero or more than 100 `rates`. | Supply 1 to 100 rates. |
| Delivery method tiered pricing, including method overrides in `subscription-previews create` | Tiered pricing could have zero or more than 25 `bands`. | Supply 1 to 25 bands. |
| `inventory-adjustments create` with reason `counted` or `correction` | A line's `source_observation_sequence` did not require an external source in the snapshot. | Remove sequence numbers for manual observations. To retain them, send `source_system` with `type: "pos"`, `"wms"`, `"erp"`, or `"other"` and an `external_source_id`, plus the required `occurred_at`. |
| `refunds create --refund-type targets` | `line_items`, `charges`, and `tax_breakdown_refunds` could all be absent or empty in the snapshot. | Include at least one nonempty target array, such as `"line_items":[{"order_line_item_id":"li_...","quantity":1}]`. |
| `orders create`, `orders update ORDER_ID`, and `invoices create` tax locations | A full-address tax location could omit state or postal code. | Supply `state` for US/CA locations and `postal_code` for every country except IE. For example, `"tax":{"location":{"address_type":"tax_address","address":{"line1":"1 Main St","city":"New York","country":"US","state":"NY","postal_code":"10001"}}}`. |
| Order and invoice tax input with `enabled: false` | `exemption` could accompany disabled tax in the snapshot. | Remove `tax.exemption` when `tax.enabled` is false; keep exemption settings only when tax is enabled. |
| `orders line-items update ORDER_ID LINE_ITEM_ID` | `expected_ship_date` could be combined with quantity, name, price, description, tax, metadata, modifiers, gift-card details, or subscription changes. | Send the expected-ship-date update separately, then send other edits in a second request with the appropriate current version. |
| `payment-intents create`, `orders payment-intents create ORDER_ID`, and `orders pay ORDER_ID` | Bodies could combine `splits` and `platform_fee`. | Choose one. Remove `platform_fee` when sending `splits`, or omit `splits` to use a platform fee. |
| `orders pay ORDER_ID` | `splits` could accompany `save_payment_method: true` or a `payment_source` containing `token` / `payment_method_id`. | Omit `splits` for those flows; use a supported split-payment input when allocating payment parts. |
| Update request bodies | An empty `{}` was accepted by many update schemas. | Send at least one field to change, such as `customers update CUSTOMER_ID --input '{"name":"New name"}'`, or skip the call when there is no update. Existing commands that already required a changed field keep that requirement. |
| `products create` variant inputs | A variant could include both `inventory_item` and `inventory_item_id`. | Choose an inline `inventory_item` or an existing `inventory_item_id`, and remove the other field. |
| `products variants update PRODUCT_ID VARIANT_ID` | An inline `inventory_item` could accompany a non-null `inventory_item_id`. | Choose one. `inventory_item_id: null` may still accompany `inventory_item` when replacing the link with an inline item. |
| `orders charges update ORDER_ID CHARGE_ID` | `amount_money` and `percent` could appear together. | Send one pricing form, such as `"amount_money":{"amount":500,"currency":"USD"}`, and omit `percent`; use `percent` alone for percentage pricing. |
| `orders payment-intents create ORDER_ID` | Nonempty `payment_options` could accompany `payment_source_selection`. | Choose the payment options or the source selection, and omit the other field. |
| `subscriptions update SUBSCRIPTION_ID` | `billing_schedule` could accompany other edits. | Send `billing_schedule` in its own request. Apply tax, end conditions, gift-recipient, plan-change, payment-method, cancellation, metadata, external-reference, delivery, interval, or quantity edits separately. |
| `merchant get` and `fulfillments update FUL_ID` | The routes accepted an `expand` query parameter. | Remove `--expand` or the raw `expand` query parameter from these calls. |

Run `flint schema input COMMAND` for the complete current body, including new variants. `coverage.json` records every current public operation and its CLI classification.

## Scope diagnostics

Local terminal scope refusals now use `INSUFFICIENT_SCOPE`, matching the API. The local error's `details` is an array. Each local scope detail has `reason: "scope_not_granted"`, `scope_requirement: {mode, scopes}`, and `missing_scopes`. Replace checks for `MISSING_REQUIRED_SCOPES`, `scope_mode`, and `required_scopes` with this representation. Client permission previews also publish `scope_requirement`.

`flint schema command COMMAND --output json` publishes the same `scope_requirement` before a call. `flint auth status` and `flint doctor` include `scope_access`, with scoped-command counts, blocked commands, and blocked resource groups. Each group includes its total scoped commands and the blocked subset, so a partially accessible group stays visible. These counts cover scoped commands accepted by the credential class. Credentials that do not expose scopes report `status: "unknown"`.

Scope remediation names the active credential override and preserves the selected profile. Browser sessions use context reauthorization when available; a fresh login requests the complete requirement. Workload tokens use workload grants and `FLINT_OIDC_SCOPE` narrowing. MCP continues to pass API errors through unchanged.
