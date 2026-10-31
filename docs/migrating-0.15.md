# Migrating to Flint CLI 0.15.0

This release has two redesigns that break scripts and MCP clients: API commands and request flags now follow one rule derived from the API contract, and the MCP server exposes five discovery tools instead of a fixed set of typed tools. It also changes when the terminal asks for confirmation. Because the CLI is before 1.0, this is a minor version bump. Read the sections that apply to you; each one shows the old form, the new form, and what to do.

`--input` bodies keep working. The embedded API snapshot was refreshed twice; the commands it adds and removes are listed under [Command names](#command-names).

## Upgrading from 0.14.x

```sh
flint upgrade --check
flint upgrade
```

`flint upgrade` in 0.14.x can fail on this release. It verifies the downloaded binary with `flint version --field data.cli_version`, and 0.15.0 rejects `data.` at the start of an output path (see [Output paths](#output-paths)). If the upgrade fails, use the package manager that installed the CLI:

```sh
npm install -g @flintpay/cli@latest
brew upgrade flint
```

Standalone installations can rerun [`scripts/install.sh`](../scripts/install.sh). Once you are on 0.15.0, `flint upgrade` works as before. Check the result with `flint version --output json`.

## Command names

API command names are now a pure function of the route. Literal path segments are kept, path IDs are positional arguments, and the last word is `get`, `list`, `create`, `update`, `replace`, or `delete` (or the route's action, such as `orders pay`). A GET on a collection is `list` when the response `data` is an array, even without page tokens. Each command also keeps one operation: a command no longer switches to a different route depending on its flags.

MCP operation IDs are these command names joined with dots, so the renames below also rename MCP operations (`merchants.get` is now `merchant.get`).

### Renamed commands

| 0.14.x | 0.15.0 |
| --- | --- |
| `flint merchants get`, `flint merchants update` | `flint merchant get`, `flint merchant update` |
| `flint request-logs get`, `flint request-logs list` | `flint api-request-logs get`, `flint api-request-logs list` |
| `flint timeline RESOURCE_ID` | `flint resource-timelines get RESOURCE_ID` |
| `flint webhook-deliveries attempts ID` | `flint webhook-deliveries attempts list ID` |
| `flint webhook-endpoints test ID` | `flint webhook-endpoints test-events create ID` |
| `flint delivery-rate-callbacks test-deliveries ID` | `flint delivery-rate-callbacks test-deliveries create ID` |
| `flint delivery-profiles assign-to-unconfigured create ID` | `flint delivery-profiles assign-to-unconfigured ID` |
| `flint orders discounts ORDER_ID` | `flint orders discounts create ORDER_ID` |
| `flint orders gift-cards ORDER_ID` | `flint orders gift-cards create ORDER_ID` |
| `flint organizations memberships ORG_ID` | `flint organizations memberships create ORG_ID` |
| `flint me gift-cards` | `flint me gift-cards create` |
| `flint inventory-counts transitions ID` | `flint inventory-counts transitions create ID` |
| `flint inventory-reservations transitions ID` | `flint inventory-reservations transitions create ID` |
| `flint inventory-transfers transitions ID` | `flint inventory-transfers transitions create ID` |
| `flint gift-cards loads create GIFT_CARD_ID` | `flint gift-card-loads create --gift-card-id GIFT_CARD_ID` |
| `flint me gift-cards transactions list GIFT_CARD_ID` | `flint me gift-card-transactions list --gift-card-id GIFT_CARD_ID` |
| `flint promotions codes create PROMOTION_ID` | `flint promotion-codes create --promotion-id PROMOTION_ID` |
| `flint promotions codes get\|update\|delete PROMOTION_ID CODE_ID` | `flint promotion-codes get\|update\|delete CODE_ID` |

The old names fail with `UNKNOWN_COMMAND` and a "Did you mean" suggestion. To update a script, search it for each old name above. The last four rows also change their arguments: the parent's ID moved from a positional argument into a flag, or was dropped because the promotion code ID alone identifies the code.

### Removed commands

| Removed | What to do |
| --- | --- |
| `flint gift-cards transitions create` | Use `flint gift-cards close`, `flint gift-cards freeze`, or `flint gift-cards unfreeze`. |
| `flint oauth authorize`, `oauth authorize preview`, `oauth contexts`, `oauth device authorize`, `oauth device reauthorize`, `oauth revoke`, `oauth token` | These unauthenticated OAuth protocol routes are no longer commands. `flint login`, `flint reauth`, and `flint auth logout` run the protocol for you. A partner install flow can still call the route with `flint api`, for example `flint api POST /v1/oauth/revoke`. |
| `flint me flint-wallet cards list`, `flint me flint-wallet cards store-setups create` | These work only with Flint's hosted account sign-in, so the CLI no longer offers them. Root help says that hosted-account operations are excluded. |
| `flint payment-intents create --order ORDER_ID` | Use `flint orders payment-intents create ORDER_ID`. The flag silently called a different route. |

### Removed aliases

Seven aliases are gone. The canonical command works in 0.14.x too, so scripts can switch before upgrading.

| Removed alias | Use |
| --- | --- |
| `flint checkout create`, `checkout get`, `checkout list` | `flint checkout-sessions create`, `get`, `list` |
| `flint payment create` | `flint payment-intents create` |
| `flint invoices late-fees assess` | `flint invoices late-fees create` |
| `flint sandboxes test-key` | `flint sandboxes api-keys create` |
| `flint update` | `flint upgrade` |

Only `login`, `logout`, `reauth`, and `whoami` remain as shortcuts for `auth login`, `auth logout`, `auth reauth`, and `auth status`.

### Added commands

- `flint gift-cards close`, `flint gift-cards freeze`, and `flint gift-cards unfreeze`
- `flint customers email-preferences get` and `flint email-preferences lookup`
- `flint trigger` (see [New in 0.15.0](#new-in-0150))
- `flint completion bash|zsh|fish`

`flint help` groups commands as Developer tools, Merchant resources, and Buyer session. `flint help --all` lists every command, and `flint schema commands --output json` adds each command's `section`. Commands that need a buyer session credential, such as `flint me orders list`, now stop before any request when you run them with a merchant credential, and merchant commands stop when `FLINT_ACCESS_TOKEN` is a customer session secret. The error is `BUYER_CREDENTIAL_REQUIRED`, `MERCHANT_CREDENTIAL_REQUIRED`, or `PARTNER_ACCESS_NOT_ALLOWED` with exit code 3. In 0.14.x the CLI sent the request and the API decided.

## Request flags

Every top-level scalar or scalar array in a request body now has a flag, generated from the API contract and named after the field. Help and `flint schema commands --output json` list each flag's allowed values and whether it is required. Nested money, quick-pay, payment link item, and confirmation-token shortcuts (`--amount`, `--currency`, `--item-name`, `--confirmation-token`, `orders pay --payment-source-token`) are unchanged.

### Flag renames

Flag names now match field names, so ID flags keep `_id` and array flags are plural. The old names fail with `UNKNOWN_FLAG` and a suggestion.

| Command | 0.14.x | 0.15.0 |
| --- | --- | --- |
| `api-keys create`, `api-keys update`, `sandboxes create`, `sandboxes api-keys create` | `--scope` | `--scopes` |
| `customer-verifications create` | `--customer` | `--customer-id` |
| `customers link-guest-purchases` | `--customer-verification` | `--customer-verification-id` |
| `delivery-rate-callbacks create\|update` | `--signing-method` | `--signing-methods` |
| `invoices create` | `--order` | `--order-id` |
| `invoices late-fees waive` | `--reason` | `--reason-message` |
| `organizations create` | `--parent-organization` | `--parent-organization-id` |
| `payment-intents confirm` | `--payment-method` | `--payment-method-id` |
| `payment-intents create` | `--customer` | `--customer-id` |
| `payment-intents create` | `--payment-option` | `--payment-options` |
| `payment-intents create` | `--order` | removed (use `orders payment-intents create`) |
| `refunds create` | `--payment-intent` | `--payment-intent-id` |
| `refunds create` | `--order` | `--order-id` |
| `webhook-endpoints create\|update` | `--enabled-event` | `--enabled-events` |
| `webhook-endpoints create\|update` | `--signing-method` | `--signing-methods` |
| `invoices list`, `orders list`, `payment-intents list` (filters) | `--customer` | `--customer-id` |
| `refunds list` (filters) | `--payment-intent` | `--payment-intent-id` |
| `refunds list` (filters) | `--order` | `--order-id` |

Array flags still accept comma-separated values or repeats: `--scopes customers.read,orders.read` and `--scopes customers.read --scopes orders.read` are the same.

The API snapshot refresh also replaced one query flag: `webhook-events list --include test_events` is now `--test`. Omit it to return all events, `--test true` returns only synthetic test events, and `--test false` excludes them.

### Flags and `--input` together

Flags now replace the matching `--input` field instead of being ignored or appended:

- A flag replaces the field in `--input`. Repeated array flags replace the whole input array; they no longer append to it. Nested shortcuts replace their field inside an object.
- Two conflicting flags for the same scalar field fail with `DUPLICATE_FLAG`.
- A flag cannot change a union tag that `--input` already sets, and a flag that belongs to a different variant fails locally.
- `--dry-run=client` lists the replaced fields as `overridden_fields`. `--debug` prints each override.

### Explicit choices for key access

Flags infer a union tag only when the schema names a discriminator, as with `api-keys create --scopes customers.read`, which selects restricted access. Where a variant cannot be inferred, the CLI exits 2 and names the flag to add. Key access is a security choice, so these calls need it spelled out:

| 0.14.x | 0.15.0 |
| --- | --- |
| `flint api-keys update KEY --scope customers.read --expected-version 3` | `flint api-keys update KEY --access restricted --scopes customers.read --expected-version 3` |
| `flint sandboxes create --name qa --scope customers.read` | `flint sandboxes create --name qa --access restricted --scopes customers.read --issue-test-key true` |
| `flint sandboxes create --name qa --access full` | `flint sandboxes create --name qa --access full --issue-test-key true` |
| `flint sandboxes create --name qa --test-key-name ci` | `flint sandboxes create --name qa --access full --test-key-name ci --issue-test-key true` |

In 0.14.x, `--scope`, `--access`, or `--test-key-name` made `sandboxes create` issue a test key. Now `--issue-test-key true` is required, and a key name alone also needs `--access`. `--input` bodies that set `scopes` must also set `access`, as in 0.14.1 and later.

`flint payment-intents confirm PAYMENT_INTENT_ID` with no credential flag now requires `--credential-type attached`.

### Time values in body flags

Body time flags, such as `--expires-at` and `test-clocks create --frozen-at`, accept RFC 3339, a date in the reporting timezone, `now`, and signed relative times: `+30d` is 30 days from now and `-2h` is two hours ago. A bare `30d` in a body flag exits 2 with `AMBIGUOUS_RELATIVE_TIME`. These flags are new in 0.15.0. Times inside `--input` bodies are still sent as written, so keep RFC 3339 there.

## Time filters

Query filters accept the same inputs as before, with changes to dates and calendar words:

| Input | 0.14.x | 0.15.0 |
| --- | --- | --- |
| `YYYY-MM-DD` | Midnight UTC | Midnight in the merchant's reporting timezone |
| `yesterday` | 24 hours before now | Midnight at the start of the previous calendar day, in the reporting timezone |
| `today`, `this_week` | Not accepted | Midnight today, or midnight on Monday of this week, in the reporting timezone |
| `2h`, `7d`, `2w` | Rolling duration in the past | Unchanged |

The CLI reads the timezone from `GET /v1/merchant`, caches it for one hour, and handles daylight saving boundaries. Without `merchants.profile.read`, or offline with no cached value, it falls back to UTC and prints a note. Each resolved filter appears on stderr and, over MCP, in the result notes. Scripts that passed dates and relied on UTC boundaries return different windows for merchants outside UTC.

## Output paths

`--field`, `--select`, and `--wait-for` now resolve from the resource, which is the response's `data` object, or each element when `data` is an array. In 0.14.x the CLI tried the path against the whole response, then with `data.` added, then inside each nested object under `data`.

| 0.14.x | 0.15.0 |
| --- | --- |
| `--field data.email` | `--field email` |
| `--select data.customer_id,data.email` | `--select customer_id,email` |
| `--wait-for data.status=active` | `--wait-for status=active` |
| `--field next_page_token` (envelope field) | `--jq .next_page_token` |
| `--field request_id` (envelope field) | `--jq .request_id` |

- The CLI checks every path against the command's output schema before it sends the request, so a bad path no longer follows a committed write. An unknown path exits 2 with `UNKNOWN_OUTPUT_FIELD` and a suggestion. A leading `data.` exits 2 with `OUTPUT_FIELD_ROOT`. Webhook event resources declare their own `data` field, so `--field data.object.id` still works on them. These replace `FIELD_NOT_FOUND`, `INVALID_SELECT`, and `WAIT_FIELD_UNKNOWN`.
- `--field` on an object or array prints compact JSON instead of failing with `FIELD_NOT_SCALAR`. On a list it prints one line per element.
- A declared field that is absent or null prints and selects as `null`.
- `--select` keeps the envelope, including `next_page_token`, `request_id`, and `meta`. `--jq` still sees the whole response.

Paths that relied on the old nested search, such as reading a field of a nested object without naming the object, now need the full path inside the resource.

## Confirmation in the terminal

The terminal and MCP now share one safety classification, built from the contract's `x-flint-destructive` and secret metadata. Operations are sensitive, irreversible, or neither. What requires confirmation depends on the environment:

| Environment | Requires confirmation |
| --- | --- |
| Sandbox | Irreversible operations, except payment collection (`pay`, `confirm`, `capture`, `collect`) |
| Live or unverified | Every sensitive or irreversible operation |

In an interactive terminal, the CLI asks "Continue? [y/N]". Otherwise it exits 4 with `CONFIRMATION_REQUIRED` (sandbox), `PRODUCTION_CONFIRMATION_REQUIRED` (live), or `UNVERIFIED_ENVIRONMENT_CONFIRMATION_REQUIRED`. Pass `--confirm` in scripts; it is accepted on every mutation. Sensitive but reversible sandbox creates, such as `orders create`, `checkout-sessions create`, and `payment-intents create`, and sandbox payment collection still run without a prompt, so quickstarts and test scripts keep working. Raw `flint api` calls to known routes follow the same table. For routes missing from the embedded snapshot, a DELETE always needs confirmation and other writes need it in live or unverified environments.

The lists below compare 0.15.0 with the 0.14.3 catalog.

### Sandbox: commands that now ask

| Kind | Commands |
| --- | --- |
| Money out and gift cards | `refunds create`, `payouts create`, `credit-notes refunds create`, `gift-cards create`, `gift-cards adjust-balance`, `gift-cards cash-out`, `gift-cards rotate-code`, `gift-card-loads create`, `gift-card-redemptions create`, `gift-card-funding-dispositions create` |
| Cancel | `subscriptions cancel`, `me subscriptions cancel`, `returns cancel`, `me returns cancel`, `returns line-items cancel`, `return-dispositions cancel`, `return-resolutions cancel`, `payouts cancel`, `invoices payment-attempts cancel`, `orders payment-attempts cancel`, `gift-card-notifications cancel`, `gift-card-redemptions cancel` |
| Void | `credit-notes void`, `invoices void`, `packages void`, `shipments void` |
| Revoke and rotate | `customer-sessions revoke`, `customers sessions revoke`, `me session revoke`, `partner-app-installs revoke`, `partner-apps installs revoke`, `partner-apps installs environment-grants revoke`, `partner-apps rotate-secret` |
| Invoices and credit notes | `invoices create`, `invoices issue`, `invoices mark-uncollectible`, `invoices regenerate-public-link`, `invoices manual-payments reverse`, `invoices late-fees waive`, `credit-notes issue`, `credit-notes allocations reverse` |
| Subscriptions | `subscriptions create`, `subscriptions renew`, `me subscriptions renew`, `subscriptions change-plan`, `subscriptions payment-retries create`, `me subscriptions payment-retries create` |
| Other | `test-clocks advance`, `payment-reviews approve`, `payment-reviews decline`, `checkout-sessions close`, `return-resolutions release`, `return-resolutions retry`, `customer-deletion-requests resolve`, `demo-sessions reset` |

`orders close`, `webhook-endpoints rotate-secret`, and the other commands that already asked in 0.14.3 still ask.

### Sandbox: commands that no longer ask

`me gift-cards delete`, `orders gift-cards delete`, and `organizations memberships delete`.

### Live or unverified: commands that now ask

`test-clocks advance`, `invoices create`, `invoices mark-uncollectible`, `invoices regenerate-public-link`, `invoices manual-payments reverse`, `invoices late-fees waive`, `invoices void`, `credit-notes allocations reverse`, `credit-notes void`, `packages void`, `shipments void`, `returns cancel`, `me returns cancel`, `returns line-items cancel`, `return-dispositions cancel`, `return-resolutions cancel`, `return-resolutions confirm`, `return-resolutions release`, and `return-resolutions retry`.

### Live or unverified: commands that no longer ask

`me gift-cards delete`, `orders gift-cards delete`, `demo-sessions create`, `customer-sessions refresh`, `payment-links resolve`, `onboarding start`, and `onboarding verify-email`. None of these is classified as sensitive or irreversible. A test checks that no gated route loses live confirmation.

## MCP server

`flint mcp serve` now follows one design for every client. Server startup, credentials, and the profile are unchanged.

| | 0.14.x | 0.15.0 |
| --- | --- | --- |
| Default tools | About 34 typed tools (`customers_create`, `refunds_create`, `schema_input`, and so on), about 226 KB for `tools/list` | Five discovery tools: `flint_search`, `flint_describe`, `flint_read`, `flint_write`, `flint_help`, about 6 KB |
| Reaching an operation | One tool per operation in the default set; others needed `--tools` | `flint_search` finds it, `flint_describe` shows its arguments, `flint_read` or `flint_write` calls it by operation ID |
| `--tools` | Tool groups or tools, plus a fixed default set, `--tools default,orders` | Opt-in typed tools for operation IDs or groups; `default` is removed |
| Typed tool names | `payment-intents_create` or `payment-intents.create` | `payment_intents_create` only |
| `--read-only` | Not available | Serves no write tool and refuses writes |
| Controls | `_flint` object (`_flint.dry_run`, `_flint.confirm`, `_flint.live`, `_flint.merchant`) | Top-level arguments (`dry_run`, `confirm`, `live`, `idempotency_key`, `select`) |
| Results | Text with a `Flint environment` line and warnings | One compact JSON text block and one notes block; reads capped at 49,152 bytes |
| Errors | Text | One JSON object with `error` (the API error, unchanged) and `cli` (who acts next, and how) |

What to change:

- **Default clients.** Remove per-operation tool names from prompts and allowlists. Agents call `flint_search`, then `flint_describe`, then `flint_read` or `flint_write` with `operation` set to the dotted command name, such as `refunds.create`. `flint_read` and `flint_write` also accept space-separated command names and typed tool names.
- **`--tools`.** Entries are operation IDs (`refunds.create`) or groups (`orders`, `payment-intents`), comma-separated or repeated. `--tools default`, `--tools help`, and entries like `payment-intents_create` stop the server at startup; use `payment-intents.create` instead, and drop `help` because `flint_help` is always available. `--tools all` selects every operation. Typed names replace every dot and hyphen with an underscore and use only lowercase letters, digits, and underscores. Old dotted or hyphenated tool names are rejected. Typed mode has no `flint_read` or `flint_write`; search, describe, and help stay, limited to the same scope. Keep the client's server name at most five characters so prefixed names fit 64 characters.
- **Sizes.** `tools/list` is about 6 KB by default, under 50 KB for `--tools orders`, and under 780,000 bytes for `--tools all` (about 548 tools). No single tool exceeds 10,000 bytes. The previous `--tools all` result was about 3 MB.
- **Controls.** Move `_flint.*` fields to top-level arguments, outside `arguments`. `_flint.dry_run: "client"` and `_flint.preview` are now `dry_run: true`. Per-call `merchant` and profile guards are removed; the server's profile, merchant guard, context, and timeout are fixed at startup. `live` appears only when the server runs with `--allow-live`.
- **Removed tools.** `schema_command`, `schema_commands`, `schema_errors`, `schema_input`, `schema_output`, `schema_events`, and `help_search` are replaced by `flint_describe` (arguments, `kind: "result"`, nested `path`, `depth`, `variant`, and `event` for webhook payloads), `flint_search`, and the `errors` and `events` topics of `flint_help`. The `help` tool is `flint_help`, with the topics `safety`, `errors`, `events`, and `test-cards`. The `doctor` and `init` tools, and `auth_status`, are now local operations (`doctor`, `init`, `auth.status`) that you reach through `flint_read` and `flint_write`. The raw `api` tool is removed; call the typed operation, or use `flint api` in a terminal.
- **Credential class.** A merchant API key no longer sees buyer-session operations, and a partner access token reaches only install-scoped operations. Discovery and calls follow the same filter.

### Confirmation over MCP

Sensitive or irreversible operations need `confirm: true` in every environment. In 0.14.x sandbox calls needed no confirmation unless the operation was destructive.

| Environment | Reads and ungated writes | Sensitive or irreversible writes |
| --- | --- | --- |
| Sandbox | Run | Need `confirm: true`; no user prompt |
| Live, elicitation | Need `--allow-live` and `live: true` | Also need `confirm: true` and user approval |
| Live, no elicitation | Need `--allow-live` and `live: true` | Return `CONFIRMATION_NEEDS_USER` with an exact terminal command |
| Live, `--allow-destructive` | Need `--allow-live` and `live: true` | Also need `confirm: true` |

A write without `confirm` returns `CONFIRMATION_REQUIRED` and a ready-to-send call. Run the write with `dry_run: true` first; the prepared request includes an `effect` with the confirmation rule, resource IDs, and formatted amounts. The user's approval applies to the same prepared request and idempotency key that is sent. Destructive hints on typed tools follow the contract's `x-flint-destructive` flag.

The server also rejects fractional, exponent, and out-of-range request IDs and invalid UTF-8, omits `id` from an error response when the request ID cannot be determined, and recovers for the next request.

## Help and search

- `flint help search WORDS` ranks the installed command, event, and reference-topic catalog offline. It needs no credential and makes no network request, and returns up to eight results with links to command help.
- `FLINT_HELP_URL` is no longer read; setting it has no effect. `flint support open` still builds a Flint Help link and keeps every flag.
- Command help and `flint schema commands --output json` show allowed values, for example `--status active|inactive`. An invalid value fails locally with a suggestion and an upgrade hint before any request. `flint listen --event-type` is the exception: it passes newer event names to the server.
- `flint help test-cards` lists twelve sandbox aliases, including dispute, risk-level, block, and available-balance scenarios.

## Configuration and identity output

### Project configuration

Project `.flint/config.json` keys are now `merchant_id`, `sandbox_id`, and `context_id`, plus `profile`. The old `merchant`, `sandbox`, and `context` keys fail instead of being ignored.

```json
{"merchant_id": "mer_123", "sandbox_id": "sbx_123", "context_id": "ctx_development"}
```

- Unknown fields in global, project, and profile configuration are errors that name the file and key. In 0.14.x unknown fields were ignored, and `flint config validate` only warned. A typo in a guard used to drop the safeguard silently.
- An unknown selected profile is an error, including when `FLINT_API_KEY`, `FLINT_ACCESS_TOKEN`, or a workload identity is set. Create profiles with `flint login`, `flint auth import`, or `flint signup` first.
- `flint init` writes the verified merchant and sandbox guards, and validates and preserves an existing file. `flint init --force` replaces it; it is available only in a terminal, and MCP rejects it. `flint init --dry-run=client` previews the path without network requests or file changes.
- `flint config set profile NAME` selects an existing profile, `flint config unset profile` resets the default, and `flint config get` lists saved profiles without an API request.

To migrate, rename the three keys in each `.flint/config.json` and commit the result.

### Identity and doctor output

`flint whoami`, `flint auth status`, `flint doctor`, and `flint config get` now show the API server, credential source, and selected profile, in human output and under `data.cli` in JSON.

`flint doctor --output json` returns an object under `data` instead of an array:

| 0.14.x | 0.15.0 |
| --- | --- |
| `{"data": [{"name": "config", ...}, ...]}` | `{"data": {"checks": [{"name": "config", ...}, ...], "cli": {...}}}` |
| `flint doctor --jq '.data[]'` | `flint doctor --jq '.data.checks[]'` |

Auth-context reads `mode`, falling back to `environment`, and accepts `access` and `auth_type: "cli_login"`. Signup requests a full-access key from `POST /v1/sandboxes/{default_sandbox_id}/api-keys` and falls back to `POST /v1/api-keys` only on HTTP 404.

## Listen

`flint listen` aligns with the webhook envelope Flint delivers:

- Forwarded bodies are the merchant webhook envelope published for the event in the embedded spec, at the caller's API version, including `event_source` and `sandbox_id`. HMAC signs the exact bytes that are forwarded.
- `--event-type` accepts repeated flags and comma-separated values, and passes newer event names through to the server. The listener record's `event_type` is now an array. In 0.14.x it was one string and a listener took one type. Update scripts that read `event_type` from the record.
- `--webhook-endpoint-id` sets the `Flint-Webhook-Endpoint-Id` header. The default is `whep_cli_listen`.
- Secrets supplied with `--webhook-secret` or `FLINT_WEBHOOK_SECRET` are no longer included in the listener record or the human banner. `--print-secret` still prints the selected secret.
- Forwarding retries transport errors, timeouts, 429, and 5xx, up to three attempts with 1-second and 2-second delays. Other failures are not retried. Delivery is at-least-once, so handlers should deduplicate by webhook event ID.

## Human output

`--output human` now formats Money objects in currency units (including zero- and three-decimal currencies), aligns detail fields, and shows lists as compact tables. Scripts that parse human output should use `--output json`, which still carries complete records and minor-unit amounts. Reference topics such as `flint help test-cards` render as tables.

## New in 0.15.0

These additions need no changes to existing scripts.

- **`flint trigger EVENT`** creates sandbox resources through the public API to produce a webhook event, and prints the expected events, resource IDs, and request IDs. `flint trigger --list` works offline. Live and unverified credentials are refused before any write. Writes use per-step idempotency keys, so an interrupted run can resume with the printed command.
- **`flint api-request-logs list --follow`** polls recorded requests every 2 seconds. Add `--created-after 10m` to backfill first. JSON output needs `--max-events` or `--for`; `--output ndjson` streams.
- **Generated flags** for every body scalar and scalar array, and `--add-FIELD` and `--remove-FIELD` for scalar arrays on readable, version-guarded PATCH routes. For example, `flint webhook-endpoints update whep_123 --add-enabled-events refund.succeeded` reads the current list and version, merges, and sends the PATCH, retrying once on a version conflict.
- **`flint completion bash|zsh|fish`** prints an offline completion script for commands, flags, and allowed values.
- **`flint schema input COMMAND --example`** prints a documented request body as raw JSON, ready to save or pipe to `--input`. Generated help examples now use the same documented bodies. Without `--example`, `schema input` prints the schema as before.
- **Test clocks and dispute simulation.** `flint test-clocks create|get|list|advance|delete` and `flint disputes simulate DISPUTE_ID --outcome won|lost|inquiry_closed|escalated` move sandbox customers and subscriptions through time, and decide a sandbox dispute. The commands arrived in the 0.14.3 snapshot; 0.15.0 adds `flint trigger` recipes that use them.
- **Faster startup.** The CLI loads a compact generated command index instead of the full API document. In the pull request's measurements, `flint version` went from a median of 0.50 s to 0.03 s and `flint customers list` from 1.11 s to 0.18 s on a shared host, with peak memory near 35 MB instead of 180 MB.
- **`--credential-store keyring|file`** and `FLINT_CREDENTIAL_STORE` apply to every command that reads credentials, including `flint mcp serve`. They are unchanged from 0.14.3.

The default total timeout of 150 seconds came in 0.14.3.
