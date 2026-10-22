# Migrating to Flint CLI 0.9.0

This is a breaking contract refresh from CLI 0.8.0. Use 0.9.0, rather than 0.8.1, because command names, parameters, and observable request and response fields change. Packaging stamps the release version; development package versions stay unchanged.

The embedded snapshot is copied byte-for-byte from [`web/apps/developers/src/generated/openapi.json`](https://github.com/flint-pay/flint/blob/b998a315ad29c278cce9034545c7eb4b2d07bdbd/web/apps/developers/src/generated/openapi.json) at reviewed monorepo main `b998a315ad29c278cce9034545c7eb4b2d07bdbd`. SHA-256: `151b8c3f11a7b80612bd400f37c1155ec483128e7758d9b608e3ec8df8451396`.

## Commands and MCP tools

The catalog changes from 598 to 613 commands. MCP changes from 588 to 603 tools with `--tools all`; the default set keeps its 32 tool names and integration intent. Updated request schemas increase its compact tools/list result from 103,561 to 149,676 bytes. The all-tools result changes from 1,612,665 to 1,837,922 bytes.

See the [MCP payload comparison](mcp-payload-0.9.md) for the per-tool measurements and the contract changes behind the increase.

The disjoint changes are **5 removed, 5 renamed, and 20 added** commands and MCP tools. Thus 10 old names disappear and 25 new names appear. Removed names use ordinary unknown-command suggestions. MCP tool names replace every dot in the command name with an underscore.

### Removed (5)

| Command | MCP tool | Migration |
| --- | --- | --- |
| `capabilities.list` | `capabilities_list` | No direct replacement; the public API removed this operation. |
| `locations.geography.update` | `locations_geography_update` | Use `locations.update` (`flint locations update <location_id> --input location.json`) with the current location schema. |
| `locations.inventory.update` | `locations_inventory_update` | Use `locations.update` (`flint locations update <location_id> --input location.json`) with the current location schema. |
| `organizations.transfer-ownership` | `organizations_transfer-ownership` | No direct replacement; the public API removed this operation. |
| `payouts.entries.list` | `payouts_entries_list` | No direct replacement; the public API removed this operation. |

### Renamed (5)

| Old command / MCP tool | New command / MCP tool |
| --- | --- |
| `gift-cards.adjustments.create` / `gift-cards_adjustments_create` | `gift-cards.adjust-balance` / `gift-cards_adjust-balance` |
| `gift-cards.cash-outs.create` / `gift-cards_cash-outs_create` | `gift-cards.cash-out` / `gift-cards_cash-out` |
| `merchant-subscription-invoices.get` / `merchant-subscription-invoices_get` | `merchant-billing-invoices.get` / `merchant-billing-invoices_get` |
| `merchant-subscription-invoices.list` / `merchant-subscription-invoices_list` | `merchant-billing-invoices.list` / `merchant-billing-invoices_list` |
| `webhook-events.deliveries` / `webhook-events_deliveries` | `webhook-deliveries.list` / `webhook-deliveries_list` |

`webhook-events deliveries <webhook_event_id>` becomes `webhook-deliveries list --webhook-event-id <webhook_event_id>`. This is the one moved filter: the old event positional becomes an optional query flag, enabling listing across events and filtering by endpoint, status, and creation range. Gift card and merchant billing invoice positional IDs retain their positions; merchant billing invoice IDs still use `msinv_`.

### Added (20)

| Command | MCP tool |
| --- | --- |
| `customers.access-links.create` | `customers_access-links_create` |
| `delivery-quotes.get` | `delivery-quotes_get` |
| `developer.partner.installs.list` | `developer_partner_installs_list` |
| `disputes.response-sessions.create` | `disputes_response-sessions_create` |
| `files.complete` | `files_complete` |
| `files.create` | `files_create` |
| `gift-card-funding-dispositions.get` | `gift-card-funding-dispositions_get` |
| `gift-card-funding-dispositions.list` | `gift-card-funding-dispositions_list` |
| `inventory-adjustments.get` | `inventory-adjustments_get` |
| `inventory-allocation-policies.revisions.get` | `inventory-allocation-policies_revisions_get` |
| `inventory-allocation-policies.revisions.list` | `inventory-allocation-policies_revisions_list` |
| `inventory-counts.get` | `inventory-counts_get` |
| `inventory-levels.get` | `inventory-levels_get` |
| `inventory-movements.get` | `inventory-movements_get` |
| `inventory-receipts.get` | `inventory-receipts_get` |
| `inventory-reservations.get` | `inventory-reservations_get` |
| `inventory-transfers.get` | `inventory-transfers_get` |
| `payment-intents.activities.list` | `payment-intents_activities_list` |
| `payment-intents.send-receipt` | `payment-intents_send-receipt` |
| `promotions.codes.get` | `promotions_codes_get` |

## Request and response migration

- `checkout-sessions.customer-verifications.confirm` keeps both positional arguments in place. The second is now `checkout_session_customer_verification_id`, including its MCP argument and history prefix `cscv_`. Creation responses use the same renamed ID field.
- `payment-intents confirm --confirmation-token` now writes `stripe.confirmation_token`. Confirmation accepts exactly one credential: that Stripe object, `payment_source_token`, or `payment_method_id`; an empty body resumes the existing source. The same exclusive shapes apply to order payment source credentials and payment intent selections.
- `refunds create` requires `--refund-type full|amount|targets` (or `refund_type` in `--input`/MCP). `amount` requires money; `full` and `targets` reject incompatible amount/tender fields. `--reason` is optional. Parent statuses are pending, succeeded, partially_succeeded, failed. Only failed is final, so waiting continues through later bank rejections of succeeded or partially succeeded refunds.
- `webhook-endpoints rotate-secret` accepts `--input`, including `expire_previous_secret_at`, and returns the endpoint. Endpoint lifecycle uses `status`, replacing `enabled`; partner mode selection uses `installed_merchant_modes`. Creation requires nonempty `enabled_events` (`["*"]` subscribes to all). DELETE returns the deleted endpoint.
- `listen` forwards only the Standard Webhooks signing and identity headers (`webhook-id`, `webhook-timestamp`, `webhook-signature`); the three legacy X-Flint signature/event headers are removed. Event payloads pass through unchanged: read `data.object`, `payload_version: 2`, and partner envelope `source`. Use the current names from `flint help events`; event filters and test events require current names. The schema also documents endpoint identity headers.
- `oauth authorize` is now a public browser authorization entry point and omits API/session credentials. The CLI conservatively retains live acknowledgement, production confirmation, and mutation idempotency checks. Its environment selector is now `sandbox_id`, replacing `environment_id`; CLI device request schemas use `mode`, replacing `environment`.
- `demo-sessions reset` requires `demo_session_id` in the body. Onboarding POST operations take `sandbox_id` only in the body. Sandbox key responses no longer publish `secret_key`; use the current response schema.
- Replace order revision fences with `expected_version` in requests and `version` on orders. Gift card allocation fences use `order_version`. Location, delivery, inventory, invoice payment term, return preview, and subscription delivery revision fields now use their published version/revision names listed below. Inventory count updates require `expected_version` and each observation requires `expected_physical_version`.
- Customer segmentation/verification uses `segment` and `is_merchant_verified`; creating a customer no longer requires email. Product/bundle categories use `category_handles`; selected product options use the current `selected_options`, `option_name`, and `value` shapes.
- Gift card inputs use `code`, `sale_reference`, and `cash_out_reference`; resources use `last4`, funding source `customer_id`/`external_reference_id`, and typed transaction `source`. Value command results return named gift card and transaction objects, and recovery uses `code_recovery_expires_at`. Notification deliveries expose `events`, `has_more_events`, and event `type`; provider outcome/status fields disappear.
- Subscriptions use `current_period_start_at`, `current_period_end_at`, `trial_end_at`; analytics uses `new_subscriptions` and `period_*_at`. Payouts expose `arrival_date`, `net_money`, and fee/balance source facts; balances and balance transactions use their current held/related-resource shapes. Merchant billing uses invoice terminology.
- Checkout and orders use `closed_reason_message`; checkout recovery, save-card capability, address autocomplete, promotion, and tipping booleans follow the names below. Order ticket data moves from purchased_event/metadata into `ticketed_event`. Buyer reads omit merchant-only metadata and filters.
- Credit notes use `line_items`; invoices and retries use `last_payment_error`. Fulfillment/package mutation results return `events`, `fulfillment_notifications`, and `unchanged`; transitions use `occurred_at`. Returns use resolution ID arrays, automatic resolution types, and typed linked resources.
- Dispute status/reason/boolean names use Flint vocabulary, including `is_action_required`, `is_evidence_deadline_passed`, and `can_respond_with_evidence`. Reviews use `refunded_money`, expose last resolution failures/updated_at, and omit obsolete location fields. Risk lists/rules use `status` rather than archived filters.
- API errors place structured facts in details, require remediation, and expose `remediation.retry` instead of `retryable`. Request logs/timelines expose retry instructions; human request-log output reads `retry`. Warning codes use lower_snake_case with typed warning fields, `resource_type`, and severity. The CLI-owned top-level `error.retryable` remains its transport/status convenience field.
- Closed input schemas enforce the reviewed enums, conditional branches, nullability, length/range/item limits, and ID patterns locally. Response schemas expose current required/nullable fields, warning/error vocabulary, and schema references without transforming JSON responses. Run `flint schema input <command>` and `flint schema output <command>` before updating stored JSON bodies or output selectors. Response-only `readOnly` annotations have been removed upstream.

## Changed command parameters

This table is generated from the before/after command catalogs. It includes removals and additions, including filters that were tightened on buyer routes. Body fields are handled separately above and below.

| Command | Removed argument/flag | Added argument/flag |
| --- | --- | --- |
| `balance-transactions.list` | `--related-object-id`, `--related-object-type` | `--order-id`, `--payout-id`, `--related-resource-id`, `--related-resource-type` |
| `checkout-sessions.customer-verifications.confirm` | `customer_verification_id` | `checkout_session_customer_verification_id` |
| `disputes.list` |  | `--action-required`, `--sort-by`, `--sort-direction` |
| `fulfillments.list` |  | `--lifecycle-status`, `--sort-by` |
| `gift-card-loads.list` | `--source-id`, `--source-type` | `--funding-source-type`, `--source-external-reference-id` |
| `gift-card-redemptions.list` | `--external-reference-id` | `--sale-reference` |
| `gift-card-transactions.list` | `--external-reference-id` | `--cash-out-reference`, `--transaction-type` |
| `gift-cards.list` | `--gift-card-id` | `--customer-id` |
| `inventory-movements.list` | `--order` | `--sort-direction` |
| `invoices.activities.list` | `--type` | `--activity-type` |
| `invoices.list` | `--has-amount-due` | `--has-outstanding-balance` |
| `me.fulfillments.list` | `--external-reference-id`, `--location-id`, `--query` |  |
| `me.invoices.list` | `--external-reference-id`, `--has-amount-due`, `--query` | `--has-outstanding-balance` |
| `me.orders.activities.list` | `--type` | `--activity-type` |
| `me.orders.list` | `--external-reference-id`, `--return-id`, `--return-resolution-id` |  |
| `me.packages.list` | `--external-reference-id`, `--external-system`, `--query` |  |
| `me.payments.list` | `--dispute-status`, `--external-reference-id`, `--origin`, `--query`, `--return-id`, `--return-resolution-id`, `--risk-level` |  |
| `me.refunds.list` | `--external-reference-id`, `--payment-intent-id`, `--query`, `--reason`, `--refund-method`, `--return-resolution-id` |  |
| `me.returns.list` | `--external-reference-id`, `--query`, `--receiving-location-id`, `--resolution-type`, `--work-type` |  |
| `me.shipments.list` | `--external-reference-id`, `--external-system`, `--query` |  |
| `me.subscriptions.list` | `--awaiting-billing-schedule`, `--next-billing-at-after`, `--next-billing-at-before`, `--subscription-offer-id` | `--next-billing-after`, `--next-billing-before` |
| `me.subscriptions.reactivate` |  | `--input` |
| `me.subscriptions.resume` |  | `--input` |
| `merchant-billing-balances.list` | `--page-size`, `--page-token` |  |
| `oauth.authorize` | `--environment-id` | `--sandbox-id` |
| `oauth.authorize.preview.get` |  | `--response-type`, `--sandbox-id`, `--state` |
| `onboarding.advance` | `--sandbox-id` |  |
| `onboarding.api-key.create` | `--sandbox-id` |  |
| `orders.activities.list` | `--type` | `--activity-type` |
| `orders.charges.delete` |  | `--expected-version` |
| `orders.discounts.reprice` |  | `--input` |
| `orders.gift-cards.delete` | `--input` | `--expected-version` |
| `orders.line-items.delete` |  | `--expected-version` |
| `orders.list` |  | `--has-open-fulfillment`, `--order-id`, `--payment-link-id` |
| `payment-intents.list` | `--origin` | `--action-required` |
| `payment-links.list` | `--has-plan` | `--has-subscription-plan`, `--updated-after`, `--updated-before` |
| `payout-settings.destinations.list` | `--include-deleted` |  |
| `promotion-codes.list` |  | `--available` |
| `promotions.list` |  | `--availability`, `--available`, `--unavailable-reason` |
| `refunds.create` |  | `--refund-type` |
| `refunds.list` | `--refund-method` |  |
| `reports.list` |  | `--report-type`, `--status` |
| `return-inspections.list` | `--location-id` | `--inspection-location-id` |
| `returns.line-items.delete` |  | `--expected-version` |
| `returns.list` |  | `--sort-by`, `--sort-direction` |
| `reviews.list` |  | `--created-before` |
| `risk-lists.list` | `--include-archived` | `--status` |
| `risk-rules.list` | `--include-archived` | `--status` |
| `subscriptions.list` | `--next-billing-at-after`, `--next-billing-at-before` | `--next-billing-after`, `--next-billing-before` |
| `subscriptions.reactivate` |  | `--input` |
| `subscriptions.resume` |  | `--input` |
| `webhook-endpoints.rotate-secret` |  | `--input` |

## Schema field inventory

The following inventories changed top-level property declarations and required lists in shared request/response components. Nested fields and union conditions use the reviewed schema referenced above. A declaration moved into `oneOf` is a structural move, rather than removal of every credential alternative; the confirmation migration above describes their supported inputs. New operations also introduce new schemas. Outbound webhook variants all follow the envelope migration above.

| Component | Removed declarations | Added declarations | Newly required |
| --- | --- | --- | --- |
| `APIRequestLog` | `retryable` | `retry` |  |
| `APIRequestLogDetail` | `retryable` | `retry` |  |
| `AddLineItemsRequest` |  | `expected_version` |  |
| `AddOrderChargeRequest` |  | `expected_version` |  |
| `AddRiskListItemsRequest` | `value`, `values` |  |  |
| `AnalyticsOverview` | `active_subscriptions` | `currency`, `new_subscriptions` | `currencies`, `currency`, `new_subscriptions` |
| `AppliedDiscount` |  |  | `order_charge_ids`, `order_line_item_ids` |
| `ApplyDiscountRequest` |  | `expected_version` |  |
| `ApplyOrderGiftCardRequest` | `gift_card_code`, `order_revision` | `code`, `expected_version` | `code` |
| `Balance` | `held_by_type`, `unavailable_money` | `observed_at` | `observed_at` |
| `BalanceListResponse` |  | `next_page_token` |  |
| `BalanceTransaction` | `hold_detail` |  | `related_balance_transaction_ids` |
| `Bundle` |  |  | `categories`, `components` |
| `BuyerCreditNote` | `credit_note_lines` | `line_items` | `buyer_tax_identity`, `line_items`, `seller_tax_identity` |
| `BuyerEffectiveDeliverySelectionResource` |  |  | `delivery_selection` |
| `BuyerGiftCard` | `last_characters` | `last4` | `last4` |
| `BuyerInvoice` |  | `active_payment_attempt`, `balance_rows`, `collection_mode`, `issue_rows`, `merchant_icon`, `merchant_logo`, `merchant_support`, `payment_option_cost_comparison`, `payment_options`, `show_flint_mark`, `theme` |  |
| `BuyerInvoiceLateFee` |  |  | `late_fee_policy` |
| `BuyerRefund` |  |  | `reason` |
| `BuyerSubscriptionPaymentRetry` | `failure` | `last_payment_error` |  |
| `CLIDeviceRequest` | `environment` | `mode` |  |
| `CLIReauthorizeRequest` | `environment` | `mode` |  |
| `CancelSubscriptionRequest` |  | `expected_version` |  |
| `ChangeSubscriptionPaymentMethodRequest` |  | `expected_version` |  |
| `CheckoutCustomerConfig` | `enable_address_autocomplete`, `external_reference_id` | `address_autocomplete_enabled` |  |
| `CheckoutDeliveryPinnedDependency` | `delivery_dependency_id`, `delivery_dependency_type`, `geography_revision` | `dependency`, `location_geography_version` | `dependency` |
| `CheckoutPaymentConfig` | `external_reference_id` |  |  |
| `CheckoutSession` | `closed_reason`, `recovery_mode`, `save_payment_method_offered`, `save_payment_method_phone_offered` | `can_save_payment_method`, `can_save_payment_method_with_phone`, `closed_reason_message`, `is_in_recovery`, `merchant_icon`, `merchant_logo`, `merchant_name`, `payment_link_image`, `show_flint_mark` | `is_in_recovery` |
| `CheckoutSettings` | `promotion_code_entry_enabled` | `promotion_codes_enabled` |  |
| `CheckoutSubscriptionTerms` |  | `delivery_required` | `delivery_required` |
| `CheckoutTipConfig` | `is_custom_tip_enabled`, `is_smart_tips_enabled` | `custom_tip_enabled`, `smart_tips_enabled` |  |
| `CloseOrderRequest` |  | `expected_version` |  |
| `CollectInvoiceResult` |  |  | `invoice_payment_attempt` |
| `ConfirmPaymentIntentRequest` | `confirmation_token`, `payment_method_id`, `payment_source_token` |  |  |
| `CreateAPIKeyRequest` | `sandbox_id` | `scope_preset` |  |
| `CreateBundleRequest` | `categories` | `category_handles` |  |
| `CreateCheckoutSessionRequest` | `tax` |  |  |
| `CreateCreditNoteRequest` | `credit_note_lines` | `line_items` |  |
| `CreateCustomerRequest` | `group_id`, `is_verified` | `is_merchant_verified`, `on_email_conflict`, `segment` |  |
| `CreateDeviceResult` |  |  | `already_existed` |
| `CreateFulfillmentEventRequest` | `buyer_notification_behavior` |  |  |
| `CreateGiftCardRedemptionRequest` | `external_reference_id` | `sale_reference` | `sale_reference` |
| `CreateInventoryAdjustmentRequest` | `external_actor_id`, `lines`, `note`, `occurred_at`, `reason`, `source_system` |  |  |
| `CreateInventoryReservationRequest` | `assignments`, `destination_fingerprint` |  |  |
| `CreateInvoiceQuickPayRequest` |  | `inventory_routing_source` |  |
| `CreateInvoiceRequest` |  | `issue` |  |
| `CreateLocationRequest` | `inventory` | `inventory_allocation_status` |  |
| `CreatePackageRequest` | `buyer_notification_behavior` |  |  |
| `CreatePackageResult` | `fulfillment_event`, `replayed` | `events`, `unchanged` | `events`, `fulfillment_notifications`, `unchanged` |
| `CreatePaymentLinkRequest` |  | `sales_end_at` |  |
| `CreateProductOptionInput` | `client_option_key` |  |  |
| `CreateProductOptionValueInput` | `client_value_key` |  |  |
| `CreateProductRequest` | `categories` | `category_handles` |  |
| `CreateRefundRequest` | `refund_method` | `refund_type` | `refund_type` |
| `CreateRefundResult` |  |  | `reason` |
| `CreateReturnInspectionRequest` | `location_id` | `inspection_location_id` | `inspection_location_id` |
| `CreateSandboxRequest` |  | `scope_preset` |  |
| `CreateSubscriptionPlanRequest` |  | `status` |  |
| `CreateWebhookEndpointRequest` | `enabled`, `mode` | `installed_merchant_modes`, `status` | `enabled_events` |
| `CreditNote` | `credit_note_lines` | `line_items` | `buyer_tax_identity`, `line_items`, `seller_tax_identity` |
| `CustomDomainStatus` |  | `domain_type` | `domain_type`, `last_checked_at` |
| `Customer` | `group_id`, `is_verified` | `is_merchant_verified`, `segment` | `name`, `phone`, `tax_identity` |
| `DeliveryEligibilityExpression` | `customer_group`, `customer_verified` | `customer_merchant_verified`, `customer_segment` |  |
| `DeliveryMethod` |  |  | `display_position`, `type` |
| `DeliveryPreview` | `selection_authority` |  |  |
| `DeliveryPreviewRoutingSource` | `inventory_allocation_policy_version_id` | `inventory_allocation_policy_revision_id` |  |
| `DeliveryPricingRate` |  |  | `when` |
| `DeliveryQuote` | `delivery_quote_revision`, `eligibility_context_revision` | `delivery_quote_version`, `eligibility_context_version` | `delivery_quote_version`, `eligibility_context_version` |
| `DeliveryRevocationTarget` | `location_geography_revision` | `location_geography_version` |  |
| `DeliverySelection` | `delivery_quote_revision`, `eligibility_context_revision` | `delivery_quote_version`, `eligibility_context_version` | `delivery_quote_version`, `eligibility_context_version` |
| `DeliverySelectionChoiceResource` |  |  | `order_charge_id` |
| `DeveloperSandbox` |  | `replaced_by_sandbox_id`, `replaces_sandbox_id` |  |
| `DeveloperSandboxWithAPIKey` | `secret_key` | `replaced_by_sandbox_id`, `replaces_sandbox_id` |  |
| `Dispute` | `action_required`, `evidence_deadline_passed`, `evidence_response_allowed`, `evidence_submission_past_due`, `metadata` | `can_respond_with_evidence`, `is_action_required`, `is_evidence_deadline_passed` | `can_respond_with_evidence`, `evidence_due_at`, `is_action_required`, `is_evidence_deadline_passed`, `payment_option` |
| `EffectiveDeliverySelectionResource` |  |  | `delivery_selection` |
| `ErrorDetail` | `captured_physical_revision`, `current_physical_revision`, `order_revision` | `available_by_source_type`, `base_subtotal_money`, `captured_physical_version`, `checkout_total_money`, `closed_reason`, `current_api_version`, `current_delivery_selection_id`, `current_interval`, `current_physical_version`, `demo_session_id`, `inactive_message`, `line_item_key`, `missing_scopes`, `modifier_choices`, `modifier_total_money`, `modifiers`, `payment_amount_money`, `payout_terms_expires_at`, `resource_id`, `resource_type`, `scope_requirement`, `selectable_merchants`, `subtotal_money`, `total_money` |  |
| `ErrorObject` | `blocking_resource_count`, `blocking_resources`, `capability`, `capturable_money`, `conflict_details`, `conflicting_fields`, `current_checkout_session_id`, `current_money`, `current_resource`, `current_selection_id`, `current_status`, `current_version`, `delivery_method_id`, `existing_checkout_session_id`, `expected_attempt_outstanding_money`, `failure_category`, `gap_money`, `invoice_payment_attempt_id`, `is_resumable`, `limit`, `maximum_money`, `missing_scopes`, `order_charge_id`, `order_line_item_id`, `order_payment_attempt_id`, `order_revision`, `payment_attempt_status`, `payment_intent_ids`, `payment_option`, `previous_key_expires_at`, `product_id`, `quota`, `reason`, `required_money`, `requoted_shipping_money`, `requoted_tax_money`, `return_resolution_id`, `scope_requirement`, `selectable_merchants`, `selectable_payment_intents`, `shipping_money`, `submitted_money`, `subscription_delivery_migration_id`, `subscription_offer_id`, `subscription_plan_line_item_id`, `supported_actions`, `surface`, `tax_money`, `tip_capable_money`, `unavailable_reason`, `variant_id` |  | `remediation` |
| `ErrorRemediation` | `missing_or_invalid_fields`, `retryable` | `retry` | `retry` |
| `ExpandedCustomerSummary` |  |  | `name`, `phone` |
| `ExpandedPayoutSummary` | `arrival_at` | `arrival_date` | `balance_source_type` |
| `ExpandedShipmentSummary` |  | `return_id` |  |
| `ExpandedSubscriptionSummary` | `current_period_end`, `current_period_start` | `current_period_end_at`, `current_period_start_at` |  |
| `FraudWarning` |  | `updated_at` | `updated_at` |
| `Fulfillment` |  | `lifecycle_status`, `order_number`, `shipment_details` |  |
| `FulfillmentCommandResult` | `event` | `events` | `events`, `fulfillment_notifications`, `unchanged` |
| `FulfillmentEventResult` | `fulfillment_notifications` |  | `replayed` |
| `FulfillmentTransitionRequestCancel` |  | `occurred_at` |  |
| `FulfillmentTransitionRequestComplete` | `completed_at` | `occurred_at` |  |
| `FulfillmentTransitionRequestHold` |  | `reason_message` |  |
| `FulfillmentTransitionRequestMarkNoShow` |  | `reason_message` |  |
| `GiftCard` | `last_characters` | `last4`, `restrictions` | `last4`, `restrictions` |
| `GiftCardFundingDisposition` |  | `idempotency_key` | `idempotency_key` |
| `GiftCardFundingDispute` |  |  | `resolution` |
| `GiftCardFundingSource` | `buyer_id`, `reference_id` | `customer_id`, `external_reference_id` | `external_reference_id` |
| `GiftCardNotificationDelivery` | `has_more_provider_outcomes`, `provider_outcomes` | `events`, `has_more_events` | `events`, `has_more_events` |
| `GiftCardNotificationDeliveryAttempt` | `provider_status_code` |  |  |
| `GiftCardPurchaseRefundAllocation` |  |  | `refund_id` |
| `GiftCardPurchaseSnapshot` |  |  | `configuration` |
| `GiftCardRedemption` | `external_reference_id` | `order_payment_attempt_id`, `sale_reference`, `source_type` | `order_payment_attempt_id`, `sale_reference`, `source_type` |
| `GiftCardTransaction` | `external_reference_id`, `source_id`, `source_type` | `cash_out_reference`, `source` | `cash_out_reference`, `source` |
| `InventoryActionRequired` | `next_actions` |  |  |
| `InventoryAllocationPolicy` |  | `current_inventory_allocation_policy_revision_id` | `current_inventory_allocation_policy_revision_id` |
| `InventoryCountLine` | `captured_physical_revision` | `captured_physical_version` | `captured_physical_version` |
| `InventoryCountObservationInput` |  | `expected_physical_version` | `expected_physical_version` |
| `InventoryLevel` | `inventory_level_claim_revision`, `inventory_level_physical_revision` | `claim_version`, `physical_version` | `claim_version`, `physical_version` |
| `InventoryMovement` | `inventory_level_claim_revision`, `inventory_level_physical_revision`, `inventory_level_revision` | `inventory_level_claim_version`, `inventory_level_physical_version`, `inventory_level_version` | `inventory_level_claim_version`, `inventory_level_physical_version`, `inventory_level_version` |
| `InventoryReservationOwner` | `expires_at`, `key`, `type` |  |  |
| `InventoryRoutingDemand` | `bundle_component_id`, `order_line_item_id` |  |  |
| `InventoryRoutingSource` | `inventory_allocation_policy_version_id`, `location_ids` | `inventory_allocation_policy_revision_id` |  |
| `InvoiceLateFee` |  |  | `late_fee_policy` |
| `InvoicePaymentAttempt` | `failure_code`, `failure_message` | `last_payment_error` |  |
| `InvoicePaymentTermsSnapshot` | `revision` | `version` | `version` |
| `InvoiceSnapshot` |  |  | `buyer_tax_identity`, `seller_tax_identity` |
| `IssueSandboxAPIKeyRequest` |  | `scope_preset` |  |
| `IssuedCreditNote` | `credit_note_lines` | `line_items` | `buyer_tax_identity`, `line_items`, `seller_tax_identity` |
| `Location` | `geography_revision`, `inventory` | `geography_version`, `inventory_allocation_status` | `address`, `geography_version`, `inventory_allocation_status` |
| `LocationAddress` |  |  | `city`, `country`, `line1` |
| `LocationCoordinate` |  |  | `latitude`, `longitude` |
| `MeFlintWalletCardListResponse` |  | `next_page_token` |  |
| `MeFlintWalletStoreSetup` |  |  | `client_setup` |
| `Merchant` | `current_deadline_at`, `has_past_due`, `observed_at`, `onboarding_status`, `payments`, `payouts`, `requirements`, `status_reason` | `readiness` | `readiness` |
| `MerchantBillingBalance` |  | `currency` | `currency` |
| `MerchantWebhookEnvelope` |  |  | `test` |
| `ModifierGroup` |  |  | `modifiers` |
| `ModifierSet` |  |  | `modifier_groups` |
| `OnboardingState` | `available_actions`, `capabilities_synced_at`, `pending_steps`, `readiness_observed_at`, `requirements` |  |  |
| `Order` | `closed_reason`, `order_revision`, `purchased_event` | `closed_reason_message`, `governing_delivery_selection_id`, `inventory_exception_reason`, `payment_link_id`, `return_resolution_id`, `ticketed_event`, `version` | `applied_discounts`, `charges`, `checkout_session_ids`, `fulfillments`, `gift_card_settlements`, `gift_card_tender_enabled`, `gift_cards`, `payment_intent_ids`, `refund_ids`, `return_credit_settlements`, `tips`, `version` |
| `OrderGiftCardAllocationAcceptance` | `order_revision` | `order_version` | `order_version` |
| `OrderGiftCardEstimate` | `order_revision` | `order_version` | `order_version` |
| `OrderGiftCardSelection` | `last_characters` | `last4` | `last4` |
| `OrderGiftCardSettlement` | `last_characters` | `last4` | `last4` |
| `OrderInventoryRoutingSourceInput` | `inventory_allocation_policy_version_id`, `location_ids` | `inventory_allocation_policy_revision_id` |  |
| `OrderLineItem` |  | `fulfillment`, `fulfillment_quantities` | `bundle_components`, `categories`, `inventory_snapshot`, `modifiers`, `purchased_gift_cards`, `selected_options` |
| `OrderPaymentAttempt` | `failure_code`, `failure_message` | `last_payment_error`, `status_reason` | `gift_card_redemptions`, `payment_intents`, `pending_actions` |
| `OrderPaymentIntentSelection` | `confirmation_token`, `payment_intent_id`, `payment_method_id`, `token` |  |  |
| `OrderTax` | `failure_reason`, `taxability_reason` | `price_mode` |  |
| `PackageStatusUpdateResult` | `event`, `package_status_update` | `events` | `events`, `fulfillment_notifications`, `unchanged` |
| `PartnerAppInstall` |  | `partner_app_id` | `partner_app_id` |
| `PartnerAuthorizePreviewPermission` |  | `required_scopes` | `required_scopes` |
| `PartnerEnvironmentGrant` | `environment_id` | `sandbox_id` |  |
| `PartnerWebhookEnvelope` |  | `payload_version`, `source`, `test` | `payload_version`, `source`, `test` |
| `PauseSubscriptionRequest` |  | `expected_version` |  |
| `PayOrderRequestConfirmPaymentIntents` |  | `expected_version` |  |
| `PayOrderRequestPay` |  | `expected_version` |  |
| `PayOrderRequestSetup` |  | `expected_version` |  |
| `PaymentAttemptGiftCardRedemption` | `last_characters` | `last4` | `last4` |
| `PaymentAttemptPaymentIntent` |  | `selected_payment_option` |  |
| `PaymentErrorSummary` |  |  | `failed_at`, `remediation` |
| `PaymentIntent` | `origin`, `settlement_status` | `checkout_session_id`, `return_id` | `metadata` |
| `PaymentIntentResult` | `origin`, `settlement_status` | `checkout_session_id`, `return_id` | `metadata` |
| `PaymentLink` |  | `sales_end_at`, `sales_summary` | `delivery_method_ids`, `total_quantity_sold` |
| `PaymentLinkCustomerConfig` | `enable_address_autocomplete` | `address_autocomplete_enabled`, `require_billing_address`, `require_phone` |  |
| `PaymentLinkPaymentConfig` | `external_reference_id` |  |  |
| `PaymentLinkSubscriptionPreview` |  | `billing_interval_options`, `quantity_options` | `billing_interval_options`, `quantity_options` |
| `PaymentMethodDomain` | `payment_options` | `payment_option_statuses` | `payment_option_statuses` |
| `PaymentSourceCredential` | `confirmation_token`, `payment_method_id`, `token` |  |  |
| `PaymentVolumeBucket` | `period_end`, `period_start` | `period_end_at`, `period_start_at` | `period_end_at`, `period_start_at` |
| `PaymentVolumeTimeseries` |  | `currency` | `currencies`, `currency` |
| `Payout` | `arrival_at`, `held_money` | `arrival_date` | `balance_source_type`, `fee_money`, `net_money` |
| `PayoutSettings` |  |  | `blocked_reasons`, `monthly_payout_days`, `next_actions`, `weekly_payout_days` |
| `Product` |  | `variants` | `categories` |
| `ProductVariant` |  |  | `selected_options` |
| `ProductVariantInput` | `selected_option_values` | `selected_options` |  |
| `ProductVariantSelectedOptionInput` | `client_option_key`, `client_value_key`, `option_value_id` | `option_name`, `value` | `option_name`, `value` |
| `Promotion` |  | `available`, `codes`, `unavailable_reason` | `available` |
| `PromotionCode` |  | `available`, `unavailable_reason` | `available` |
| `PublicPaymentLink` |  | `sales_end_at` |  |
| `PublicPaymentLinkResult` |  | `sales_ended`, `show_flint_mark` | `is_sold_out`, `sales_ended`, `show_flint_mark` |
| `PublishReturnPolicyRevisionRequest` | `expected_current_return_policy_revision_id` |  | `expected_version` |
| `PurchasedGiftCard` | `last_characters` | `last4` | `last4` |
| `ReceiptSettings` | `is_auto_email_enabled` | `auto_email_enabled` |  |
| `Refund` |  |  | `reason` |
| `RemoveDiscountsRequest` |  | `expected_version` |  |
| `ReservationLine` | `geography_revision` |  |  |
| `ResourceTimeline` | `environment_id` |  |  |
| `ResourceTimelineEntry` | `environment_id`, `image_revision` | `image_version`, `retry` |  |
| `ResponseMeta` | `request_id` |  |  |
| `ResponseWarning` | `context`, `resource` | `capability`, `current_deadline_at`, `previous_stage`, `removed_scopes`, `resource_type`, `stage` | `severity` |
| `Return` | `disposition_count`, `inspection_count`, `receipt_count`, `resolution_count`, `shipment_count` | `return_disposition_ids`, `return_dispositions`, `return_inspection_ids`, `return_inspections`, `return_receipt_ids`, `return_receipts`, `return_resolution_ids`, `return_resolutions`, `shipment_ids`, `shipments` | `return_disposition_ids`, `return_inspection_ids`, `return_receipt_ids`, `return_resolution_ids`, `shipment_ids` |
| `ReturnInspection` | `location_id` | `inspection_location_id` | `inspection_location_id` |
| `ReturnLineDecision` | `decision_basis`, `resolution_mode`, `selected_resolution_type` | `automatic_resolution_type` |  |
| `ReturnLineItem` | `resolution_count`, `resolution_mode`, `selected_resolution_type` | `automatic_resolution_type`, `return_resolution_ids` | `return_resolution_ids` |
| `ReturnLineItemDecisionProposal` | `resolution_mode`, `selected_resolution_type` | `automatic_resolution_type` |  |
| `ReturnProcessDecision` | `decision_basis`, `resolution_mode`, `selected_resolution_type` | `automatic_resolution_type` |  |
| `ReturnResolutionExecutionBlocker` | `linked_resource_id`, `linked_resource_type` | `linked_resource` |  |
| `ReturnResolutionPreview` | `based_on_return_revision` | `based_on_return_version` | `based_on_return_version` |
| `Review` | `ip_address_location`, `refunded_amount_money` | `last_resolution_failure`, `refunded_money`, `updated_at` | `last_resolution_failure`, `refunded_money`, `updated_at` |
| `RevokeDeliveryDependencyRequest` |  | `expected_version` |  |
| `RiskList` |  | `editable`, `origin`, `status` | `editable`, `origin`, `status` |
| `RiskRule` |  | `status` | `status` |
| `Settings` |  |  | `tax_identity` |
| `Subscription` | `current_period_end`, `current_period_start`, `trial_end` | `cancellation_reason`, `current_period_end_at`, `current_period_start_at`, `missed_billing_at`, `scheduled_resume_at`, `trial_end_at` | `billing_interval`, `billing_interval_count`, `line_items`, `next_retry_at`, `recurring_amount_money`, `scheduled_resume_at` |
| `SubscriptionDelivery` | `revision` | `delivery_version`, `updated_by` | `delivery_version` |
| `SubscriptionPaymentRetry` | `failure` | `last_payment_error` |  |
| `SubscriptionPlanSwapVariant` |  |  | `unit_price_money` |
| `SubscriptionSnapshotMetrics` |  |  | `arr_by_currency`, `mrr_by_currency` |
| `SubscriptionWindowMetrics` |  |  | `subscription_collected_money_by_currency` |
| `TaxIdentity` |  |  | `legal_name`, `registered_address` |
| `Tip` |  |  | `payment_intent_allocations`, `value_settlement_allocations` |
| `TippingSettings` | `is_custom_tip_enabled`, `is_smart_tips_enabled` | `custom_tip_enabled`, `smart_tips_enabled` |  |
| `TippingSettingsPatch` | `is_custom_tip_enabled`, `is_smart_tips_enabled` | `custom_tip_enabled`, `smart_tips_enabled` |  |
| `UpdateBundleRequest` | `categories` | `category_handles` |  |
| `UpdateCreditNoteRequest` | `credit_note_lines` | `line_items` |  |
| `UpdateCustomerRequest` | `group_id`, `is_verified` | `email`, `is_merchant_verified`, `segment` |  |
| `UpdateInventoryCountRequest` |  |  | `expected_version` |
| `UpdateLocationRequest` |  | `address`, `coordinate`, `coordinate_source`, `inventory_allocation_status`, `timezone` |  |
| `UpdateOrderChargeRequest` |  | `expected_version` |  |
| `UpdateOrderRequest` | `order_revision` | `expected_version` |  |
| `UpdatePackageRequest` | `buyer_notification_behavior` |  |  |
| `UpdatePackageResult` | `fulfillment_event` | `events`, `unchanged` | `events`, `fulfillment_notifications`, `unchanged` |
| `UpdatePaymentLinkRequest` |  | `sales_end_at` |  |
| `UpdateProductRequest` | `categories` | `category_handles` |  |
| `UpdateProductVariantRequest` |  | `external_reference_id`, `selected_options` |  |
| `UpdateRefundRequest` |  | `external_reference_id` |  |
| `UpdateReturnRequest` |  | `expected_version` |  |
| `UpdateSettingsRequest` |  | `payment_limits` |  |
| `UpdateSubscriptionPlanRequest` |  | `status` |  |
| `UpdateWebhookEndpointRequest` | `enabled`, `mode` | `installed_merchant_modes`, `status` |  |
| `VoidPackageResult` | `event` | `events` | `events`, `fulfillment_notifications`, `unchanged` |
| `VoidShipmentResult` |  |  | `events`, `fulfillment_notifications`, `packages`, `unchanged` |

## Product question

FLI-257 comments ask whether the MCP history clear action should remain reachable. This contract refresh conservatively preserves the existing local history tool and its confirmation requirements; removing that local feature needs a separate product decision. No server operation is retained as a stub.
