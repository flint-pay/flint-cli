# MCP payload comparison for CLI 0.9.0

The increase is contract growth. Keep the request constraints and meaningful descriptions, and keep the default tool set unchanged. Changing the default set to meet a smaller payload budget remains a product decision.

## Measurement

Built `./cmd/flint` from 0.8.0 commit `1e22066` and the 0.9.0 contract refresh at `3218147`, then drove each binary's `flint mcp serve` over stdio, both without `--tools` and with `--tools all`. Each session sent `initialize`, `notifications/initialized`, and `tools/list`, keeping stdin open until the responses arrived. Runs used an isolated temporary HOME/XDG_CONFIG_HOME, a dummy sandbox credential, and a local mock on a free port above 29000. No API requests were needed.

Sizes below count only the parsed `tools/list` result, compacted with Python `json.dumps(result, separators=(",", ":"), ensure_ascii=True)` and encoded as UTF-8. They exclude the JSON-RPC envelope and newline. This matches the migration guide's measurement convention. Without Unicode escaping, the all-tools results are 1,612,661 and 1,837,915 bytes; default sizes are identical under either convention.

| Selection | 0.8.0 tools | 0.9.0 tools | 0.8.0 bytes | 0.9.0 bytes | Change |
| --- | ---: | ---: | ---: | ---: | ---: |
| Default | 32 | 32 | 103,561 | 149,676 | +46,115 (+44.5%) |
| All | 588 | 603 | 1,612,665 | 1,837,922 | +225,257 (+14.0%) |

The final binary retains these sizes: the follow-up changes only registry wording, documentation, and manifest tests.

## Sources of default growth

`internal/cli/mcp.go` and `internal/cli/schema.go` are identical between the two measured commits. The refreshed OpenAPI snapshot adds fields, validation structure, and descriptions that the existing generator preserves.

- `webhook-events_list` grows by 15,938 bytes. Its `event_type` property adds 15,278 bytes, including 14,928 bytes of enum descriptions; `resource_type` adds 660 bytes. The event descriptions include delivery semantics, finality, and guidance such as which payment event to use, beyond the names of the events.
- `refunds_create` grows by 11,543 bytes: 111 in its tool description and 11,432 in its input schema. The new `oneOf` adds 9,402 bytes, root properties add 1,773, and definitions add 141; remaining root keyword changes add 116. The full/amount/targets branches include different required fields, forbidden combinations, and constraints for gift card funding loads and tender allocations. Repeated branch fields accompany closed objects and preserve those distinctions.
- `payment-links_create` grows by 3,056 bytes: 836 in root properties and 2,220 in definitions. Examples include sales end timing, customer address requirements, event configuration, and image/line-item constraints.
- `payment-intents_list` grows by 2,734 bytes. Its status and dispute status descriptions contribute 2,392 bytes; these explain action required, deadlines, finality, and unknown states. The operation also gains the `action_required` filter and revised sorting/filter descriptions.
- `products_create` grows by 2,542 bytes: 689 in root properties and 1,853 in definitions. Product option selections now specify option names/values, bounds, required fields, and matching rules; category handles, metadata, images, and variant limits also change.

Across the default result, `x-enum-descriptions` account for 18,313 new bytes, `description` keywords for 19,271, and all other structure for 8,531. Those categories are disjoint and include each keyword's JSON syntax. Of the total 46,115-byte increase, 45,639 is input schemas and 476 is tool descriptions; names and annotations do not change.

No `x-flint-*` bookkeeping or `readOnly` fields remain in either default result. Examples have the same 171-byte contribution in both. Every default `$defs` entry is reachable, and no tool contains identical definitions under different names. Existing enum compaction still removes enum descriptions that normalize to their literal value. The added descriptions and request unions do not bypass that rule. There is no compaction regression to fix under the existing stripping policy.

## Per-tool default breakdown

Each row counts the entire tool object (`name`, `description`, `inputSchema`, and `annotations`). The last two columns explain its delta. Names and annotations have zero delta for every tool. The result wrapper and array separators contribute another 43 bytes in each version.

| Tool | 0.8.0 bytes | Final bytes | Delta | Input schema delta | Description delta |
| --- | ---: | ---: | ---: | ---: | ---: |
| `webhook-events_list` | 9,231 | 25,169 | +15,938 | +15,938 | +0 |
| `refunds_create` | 7,053 | 18,596 | +11,543 | +11,432 | +111 |
| `payment-links_create` | 13,563 | 16,619 | +3,056 | +3,056 | +0 |
| `payment-intents_list` | 4,835 | 7,569 | +2,734 | +2,734 | +0 |
| `products_create` | 7,453 | 9,995 | +2,542 | +2,542 | +0 |
| `customers_update` | 4,471 | 6,397 | +1,926 | +1,926 | +0 |
| `payment-intents_create` | 4,782 | 6,486 | +1,704 | +1,704 | +0 |
| `customers_create` | 3,637 | 5,265 | +1,628 | +1,578 | +50 |
| `checkout-sessions_create` | 10,067 | 11,461 | +1,394 | +1,394 | +0 |
| `payment-intents_confirm` | 1,500 | 2,724 | +1,224 | +1,027 | +197 |
| `webhook-endpoints_create` | 2,474 | 3,300 | +826 | +761 | +65 |
| `refunds_list` | 3,893 | 4,404 | +511 | +511 | +0 |
| `payment-links_list` | 2,584 | 2,940 | +356 | +356 | +0 |
| `payment-intents_capture` | 2,040 | 2,382 | +342 | +346 | -4 |
| `products_list` | 3,370 | 3,564 | +194 | +194 | +0 |
| `customers_list` | 2,603 | 2,727 | +124 | +124 | +0 |
| `payment-links_get` | 1,438 | 1,542 | +104 | +16 | +88 |
| `checkout-sessions_get` | 1,453 | 1,453 | +0 | +0 | +0 |
| `customers_get` | 1,375 | 1,375 | +0 | +0 | +0 |
| `help` | 1,151 | 1,151 | +0 | +0 | +0 |
| `payment-intents_get` | 1,547 | 1,547 | +0 | +0 | +0 |
| `products_get` | 1,341 | 1,341 | +0 | +0 | +0 |
| `refunds_get` | 1,450 | 1,450 | +0 | +0 | +0 |
| `schema_command` | 922 | 922 | +0 | +0 | +0 |
| `schema_commands` | 898 | 898 | +0 | +0 | +0 |
| `schema_errors` | 733 | 733 | +0 | +0 | +0 |
| `schema_events` | 1,072 | 1,072 | +0 | +0 | +0 |
| `schema_input` | 936 | 936 | +0 | +0 | +0 |
| `schema_output` | 924 | 924 | +0 | +0 | +0 |
| `webhook-endpoints_list` | 1,716 | 1,716 | +0 | +0 | +0 |
| `webhook-events_get` | 1,282 | 1,282 | +0 | +0 | +0 |
| `payment-intents_cancel` | 1,724 | 1,693 | -31 | +0 | -31 |
