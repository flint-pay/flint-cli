# Flint CLI

`flint` is the command-line client for Flint's public API. Every command maps to a documented `/v1` route, so anything you can do over HTTP you can do from a shell, a script, or a CI job.

- Guide: https://developers.withflintpay.com/docs/guides/cli
- Command reference: https://developers.withflintpay.com/docs/cli
- npm package source: [`npm/`](npm/)

## Install

```bash
npm install -g @flintpay/cli      # resolves a prebuilt binary for your platform
brew install flint-pay/tap/flint   # macOS and Linux
```

After installation, `flint upgrade` (or its alias `flint update`) checks for the latest stable release using GitHub's `releases/latest` web redirect. If that lookup fails, it falls back to the GitHub REST API and uses `GITHUB_TOKEN` when set. Run `flint upgrade --check` to see the current and latest stable versions without changing anything; it exits 0 whether or not an update is available, and `--output json` reports `update_available`. On macOS and Linux it uses npm or Homebrew when they own the installation, or securely replaces a standalone binary after verifying the release checksum. On Windows it prints the exact npm or manual update step to run after closing the CLI. Progress appears on stderr, with an elapsed-time update every 10 seconds during longer steps. Use `--quiet` to hide it, or `--output json --progress json` for structured results and progress on separate streams.

Prebuilt archives for macOS, Linux, and Windows are attached to each `cli/v*` [release](https://github.com/flint-pay/flint-cli/releases) with a `checksums.txt`. [`scripts/install.sh`](scripts/install.sh) downloads and verifies the right one. The release workflow also publishes a container image to `ghcr.io/flint-pay/flint-cli`.

For manual installation on macOS or Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/flint-pay/flint-cli/main/scripts/install.sh -o /tmp/flint-install.sh
sh /tmp/flint-install.sh
```

The installer uses `/usr/local/bin` when you can write to it and `$HOME/.local/bin` otherwise, and tells you if that directory is not on your `PATH`. Set `FLINT_INSTALL_DIR` to choose another directory, or `FLINT_CLI_VERSION=X.Y.Z` to install a specific version. Without `FLINT_CLI_VERSION`, the installer resolves the latest stable release through GitHub's `releases/latest` web redirect, avoiding the REST API's anonymous rate limit. If the redirect lookup fails, it falls back to the REST API; set `GITHUB_TOKEN` to raise the fallback rate limit. The token is sent only with the API request. On Windows, download the ZIP and `checksums.txt` from the same release, verify the ZIP with `Get-FileHash -Algorithm SHA256`, and extract `flint.exe` into a directory on `PATH`.

## Sign up and make a test payment

`flint signup` creates an account without an invitation. In an interactive terminal it asks for your name, email, and the emailed verification code. Incorrect codes can be retried up to five times per code; expired codes offer to send a new one.

For scripts, start signup first, then finish after receiving the email:

```sh
flint signup --email dev@example.com --first-name Ada --last-name Lovelace --no-input --output json
flint signup --verification-code 482193 --no-input --output json
```

The first command succeeds with `data.status` set to `verification_required`. The CLI stores the temporary verification token, email, and name in the private profile config and prints the command to finish. Resume using the same profile and API server; no name or email flags are needed. The pending signup retains the selected credential store, and file-store continuation commands include `--credential-store file`. Successful verification saves the onboarding session and its expiry until signup finishes, so retrying after a later failure does not consume another code attempt. If progress is missing, the code expires, or five attempts are used, start again without `--verification-code`. MCP's `signup` operation, called through `flint_write`, uses the same two calls: first pass `email`, `first_name`, and `last_name`, then pass only `verification_code`. It uses the server's profile and refuses to run when that profile already has a saved credential.

Successful signup saves and activates the new sandbox key, replacing the profile's previous login context. JSON output includes `data.next_command`. For a custom API server, the printed continuation and payment commands include the required `FLINT_BASE_URL`; keep that environment setting when using the saved key. If the project pins an OAuth context in `.flint/config.json`, run signup outside the project or remove that pin first. Make your first test payment with:

```sh
flint payment-links create --name "Test payment" --item-name "Test payment" --amount 1000 --currency USD --open
```

`--open` opens the payment link in your browser in an interactive terminal with human output. The URL is printed either way; JSON output, non-interactive runs, and `--no-input` never open a browser. Pay with card 4242 4242 4242 4242, any future expiry date, and any CVC.

## Sign in (recommended)

For local development, start with `flint login`. It opens the Flint website, displays a confirmation code, and saves OAuth access and refresh tokens in your OS keychain. The printed approval link includes the pairing code, and the code also appears on its own line. Use `--no-open` to open the printed link yourself, `--profile NAME` to use or create a profile without changing the default profile, or `--live` to explicitly request production access. Login waits up to 10 minutes by default; `--timeout 5m` overrides that limit. With `--no-input`, JSON output, or no TTY, it prints the approval link and code without opening a browser. JSON polling emits a `login_pending` JSON record on stderr with `verification_url`, `user_code`, and `expires_at`.

Use `flint login --no-wait --output json` to start approval and exit successfully with `data.status: "approval_pending"`, the link, code, expiry, and `next_command`. Run `next_command` to resume polling with the same profile, credential store, `--live` acknowledgement, scopes, and context guards; keep the same API server and project sandbox guard. Conflicting environment or guard flags fail before redeeming approval and preserve the pending login. Pending device approval is saved in its own private 0600 file in the config directory and removed when completed or expired. Successful login saves OAuth tokens in the selected credential store.

If browser login is unavailable, use `flint auth import` instead. Browser approval requires a person; automation should use [workload identity](#workload-identity-and-automation), with an API key as a fallback.

Browser login grants baseline permissions for common development workflows. Some command groups, including subscriptions, payouts, reports, risk, and advanced commerce operations, require additional access. Add scopes to the active context with `flint reauth --scope SCOPE`; repeat `--scope` when an operation requires multiple scopes. The browser may approve only part of a request, and the CLI reports what was approved. Known command requirements are checked before a request is sent.

Missing scopes that the public API limits to live keys cannot be added to a sandbox key or browser context. The CLI reports `MISSING_REQUIRED_SCOPES` with `details.reason: "live_mode_required"` when those scopes are necessary, and asks you to use a live key. When any one of several scopes is sufficient, sandbox guidance lists only scopes available in sandbox mode. Doctor excludes live-only scopes from its missing sandbox baseline list and explains the live-mode limitation.

Access tokens refresh automatically as needed. `flint auth logout --confirm` revokes the OAuth session before removing its local credentials. If the server does not confirm revocation, logout reports failure and retains the credentials for retry. Imported API-key logout still removes only the local key. A credential-store override on logout must match the profile's saved backend; omit the override to use that backend.

To pass a short-lived bearer token to another local tool, run `flint auth token --output json`. The flat JSON contains `access_token`, `token_type` (`Bearer`), and `expires_at` (RFC3339) when the expiry is known. Saved OAuth sessions refresh before export; configured workload identities exchange their OIDC token. The command validates the active context, merchant and sandbox guards, and live-mode acknowledgement before exporting. Imported and environment API keys fail with `API_KEY_NOT_EXPORTABLE`. Other session token types cannot be exported. Treat the output as a secret and never write it to logs. Token export is unavailable through MCP.

`flint login` is a shortcut for `flint auth login`. Related shortcuts: `flint logout` for `flint auth logout`, and `flint whoami` for `flint auth status`.

### Sessions and contexts

Approve access to selected merchants, sandboxes, and live contexts once in the browser, then select where commands run:

```sh
flint login
flint context list
flint context switch                        # interactive selector
flint context switch ctx_development        # select an authorized sandbox
flint context switch ctx_production --live   # explicitly select a live default
flint auth status --context ctx_development  # one command, without changing the default
flint reauth                                # change browser-approved access
```

`flint login` reuses a valid saved OAuth session and starts browser approval again when the server confirms that the session expired or was revoked. Temporary connection failures or loss of access to one context do not automatically replace the session. Use `flint login --new-session` to replace it after browser approval; the CLI then attempts to revoke the previous session. `flint logout --confirm` revokes the entire session across its contexts.

Manage and revoke sessions on the [CLI sessions page](https://app.withflintpay.com/developers/cli).

Context selection uses `--context ID`, then a project's `.flint/config.json` `context` field, then the profile default. For example, `{"context":"ctx_development"}` pins a project. Switching changes the profile default, not a project pin. Each running command retains its starting context. Access tokens remain restricted to one context and refresh automatically. LIVE contexts are labeled in the selector and command diagnostics; selected multi-context sessions do not need `--live` on every command, while sensitive/destructive operations still require confirmation.

Environment credentials (`FLINT_API_KEY`, `FLINT_ACCESS_TOKEN`, or `FLINT_WORKLOAD_IDENTITY_ID`) override configured browser-session contexts. An explicit `--context` cannot be combined with these overrides. Merchant and sandbox guards still apply.

If `flint context list` reports that contexts are unavailable, your existing login remains usable and keeps its live-mode safeguards. Run `flint reauth` later to authorize multiple contexts.

### Workload identity and automation

For CI and containers, configure a Flint workload identity that trusts your platform's OIDC issuer, subject, and audience. Set the non-secret `FLINT_WORKLOAD_IDENTITY_ID` and `FLINT_NO_INPUT=1`. The CLI exchanges the platform token for a short-lived Flint access token, without saving credentials or using the OS keyring. The Flint server must support workload identity token exchange.

For GitHub Actions, grant the job `permissions: id-token: write` and set `FLINT_WORKLOAD_IDENTITY_ID` in its environment. For GitLab CI, issue a token into `FLINT_OIDC_TOKEN`:

```yaml
id_tokens:
  FLINT_OIDC_TOKEN:
    aud: https://api.withflintpay.com
```

`FLINT_OIDC_SOURCE` defaults to `auto`, trying `FLINT_OIDC_TOKEN`, `FLINT_OIDC_TOKEN_FILE`, GitHub Actions, GitLab CI, then Kubernetes when its environment marker is present. Kubernetes uses a projected token at `/var/run/secrets/flint/token`; mount a token with the Flint audience there or set `FLINT_OIDC_TOKEN_FILE`. Files are read again on each exchange. The CLI never discovers AWS or Azure token files or the default Kubernetes service account token. You can select `token`, `token_file`, `github_actions`, `gitlab_ci`, or `kubernetes` explicitly. `none` disables source discovery and reports an error while a workload identity is configured. GCP, Azure, and AWS sources are not supported in this version.

The audience defaults to the origin of `FLINT_BASE_URL`, or `https://api.withflintpay.com`. Use `FLINT_OIDC_AUDIENCE` to override it and `FLINT_OIDC_SCOPE` to request narrower scopes. Live identities require `--live`. Merchant and sandbox guards apply to workload identities.

Environment access tokens take precedence over API keys, with a warning when both are set. API keys and access tokens take precedence over workload identity, which takes precedence over the saved login. Workload identity overrides configured contexts and cannot be combined with `--context`.

If your platform cannot provide a Flint audience OIDC token, supply `FLINT_API_KEY` through your secret store as a fallback. For manual setup, create a key in the [Flint dashboard](https://app.withflintpay.com/developers/api-keys) and run `flint auth import`. Unset environment credentials before signup or browser login.

The OS keyring is the default credential store. To save API keys or OAuth sessions without a keyring, explicitly choose `--credential-store file` on `flint login`, `flint auth import`, or `flint signup`, or set `FLINT_CREDENTIAL_STORE=file`. A flag takes precedence over the environment, then the profile's saved choice, then `keyring`. Successful authentication persists the selected backend per profile. File credentials are separate from `config.json`, under `credentials/store.json` in the Flint config directory, with owner-only directory and file permissions (0700 and 0600 on Unix). File updates are locked and atomic. The file is not encrypted by the OS keyring; each committed save prints that notice. There is no automatic plaintext fallback.

Fresh profiles skip keyring lookup. Existing profiles without a presence marker receive one bounded legacy lookup; a configured but unavailable store reports its failure. Doctor continues public connectivity, API-version and CLI-version checks when credentials are absent or unavailable. A locked or unavailable keyring names `--credential-store file` and `FLINT_API_KEY` as alternatives. `flint auth import --profile NAME`, `flint login --profile NAME`, and `flint signup --profile NAME` create missing profiles without changing the default profile.

API-key auth-context metadata is cached in memory and in a separate 0600 file for five minutes, keyed by a SHA-256 hash of the key and API base URL. The cache contains no API key. Commands whose scope checks pass use the cached metadata. Before refusing a command for missing scopes in cached metadata, the CLI fetches the current context once and checks it again, including environment, merchant, sandbox, and confirmation requirements. A failed refresh stops the command before its target request. `flint auth status` (also `flint whoami`) always fetches the current context, so scope, expiry, and revocation changes are visible immediately. A later HTTP 401 or 403 invalidates the cache; OAuth credential handling is unchanged.

Configuration selection uses `--profile`, `FLINT_PROFILE`, a project pin in `.flint/config.json`, then the default profile. With `FLINT_API_KEY`, `FLINT_ACCESS_TOKEN`, or `FLINT_WORKLOAD_IDENTITY_ID`, a missing profile is allowed and scopes resource history. Empty config files are accepted and unknown fields are ignored; `flint config validate` reports unknown fields as warnings. Use `flint config unset merchant` to clear the selected profile's saved merchant guard. Project and environment guards still apply, and mismatch errors identify their source.

## Use

```bash
flint login         # recommended: approve browser sign-in; credential saved in the OS keychain
flint doctor        # config, credential, connectivity, environment, merchant, scopes, version
flint payment-links create --name T-shirt --item-name T-shirt --amount 2500 --currency USD --open
flint listen --forward-to http://localhost:8080/webhooks/flint
```

`flint listen --forward-to` always signs forwarded events locally with HMAC using its listen secret, regardless of the registered endpoint's signing methods. Handlers written for JWT need an HMAC verification path or a local test bypass when using `listen`. The CLI saves and reuses the listen secret across runs; print it with `flint listen --print-secret`, or pin your own with `--webhook-secret` or `FLINT_WEBHOOK_SECRET`. Without `--forward-to` it prints events instead. `--forward-to` accepts localhost, loopback IPs, and `host.docker.internal`. With `--max-events` or `--for`, it exits 1 if any delivery failed, 5 if it never connected, and 6 if fewer than `--max-events` events arrived before `--for` ended; the checkpoint's `replay_cursor` resumes from the first failure with `--cursor`. `--save-to -` writes a downloaded file to stdout.

Create a webhook endpoint with JWT signing (the default):

```sh
flint webhook-endpoints create --url https://example.com/webhooks/flint --enabled-event payment_intent.succeeded --signing-method jwt
```

Repeat `--signing-method` to select `jwt`, `hmac`, or both. JWT deliveries carry `Authorization: Bearer <token>`; verify the token with [Flint's JWKS](https://api.withflintpay.com/.well-known/jwks.json), including its issuer, endpoint audience, expiry, and raw body hash. Creation returns `secret` only when `hmac` is included; save it if present. `webhook-endpoints update --signing-method ... --expected-version N` replaces the set; omitting the flag leaves it unchanged. Removing `hmac` deletes shared secrets. Adding `hmac` without an issued secret returns `409 ACTION_NOT_ALLOWED` with reason `secret_not_issued` (param `signing_methods`); run `flint webhook-endpoints rotate-secret whep_... --confirm` to issue the first secret and enable `hmac`.

Delivery rate callbacks take the same repeatable `--signing-method jwt|hmac` flag on `delivery-rate-callbacks create` and `delivery-rate-callbacks update`, also defaulting to `jwt`. JWT callback requests carry `Authorization: Bearer <token>` whose `aud` is the callback's `dcb_` ID. Creation returns `secret` and `key_id` only when `hmac` is included. Updating signing methods replaces the set and requires `--expected-version`, and `flint delivery-rate-callbacks rotate-secret dcb_... --confirm` issues the first secret and enables `hmac`, or rotates an existing one.

`flint`, `flint --help`, and `flint help` list starting points, API resource groups, and examples for creating primitives. `flint schema commands customers --output json` returns the commands under one group as data, such as `customers` or `customers addresses`; without a group, `flint schema commands --output json` returns the whole catalog, which is large. Root help with `--output json` or `FLINT_OUTPUT=json` returns a compact index instead. `flint schema command customers create --output json` describes one command; dotted names such as `customers.create` work too. Command descriptions and offline help topics return a `{"data": ...}` envelope in JSON mode. `schema input` and `schema output` return raw JSON Schema documents. Human schema output is indented. `flint help events` lists event types with short descriptions; `flint help events customer.created` shows one event's details. `flint help errors` describes the error envelope; `flint help exit-codes` lists exit codes.

`flint schema errors --output json` lists CLI and public API error types, exit codes, and listen error codes under `data`. `flint schema events --output json` returns a compact event list under `data`; pass an exact type such as `payment_intent.succeeded` or a prefix such as `payment_intent` to get a raw OpenAPI document with the matching payload schemas and their referenced components.

Primitives use their resource names, such as `customers`, `products`, `orders`, and `subscription-plans`. Start with `flint <resource> --help` to list operations, then `flint <resource> create --help` to see flags, examples, and required top-level request fields when creation is supported. Some groups are read-only or use nested command paths; their resource help lists the exact commands.

Commands with convenience flags can create a resource directly. Other commands take a JSON request body through `--input FILE.json`, `--input -` for stdin, or inline JSON such as `--input '{"email":"jane@example.com"}'`, which the CLI sends as the API request body. Use `flint schema input <resource>.create --output json` to see the JSON Schema of that body, prepare the file, then run the create command. Path arguments such as IDs and execution controls such as `--live` or `--idempotency-key` are not part of the body; pass them as arguments and flags. Convenience flags set their body fields on top of the file.

Writes that require an idempotency key, such as `flint refunds create` and `flint orders pay`, require `--idempotency-key KEY`, including with `--dry-run=client`. Save the key with the action before the first command and reuse it on retry. For writes with an optional key, the CLI generates a `flint-cli-…` key and reuses it across its own retries. Operations that take no key send none. The API never generates keys.

```sh
flint payment-intents create --amount 2500 --currency USD --payment-option card --idempotency-key quickstart-pi-1
```

Running the command again with the same key returns the same payment intent; change the key to create another.

```bash
flint customers create --email jane@example.com
flint products create --help
flint schema input products.create --output json  # schema for product.json

cat > product.json <<'JSON'
{
  "name": "T-shirt",
  "product_type": "physical",
  "default_variant": {
    "unit_price_money": {"amount": 2500, "currency": "USD"}
  }
}
JSON
flint products create --input product.json
```

The product example sets a $25.00 USD price; money amounts use the smallest currency unit. Before sending, the CLI checks the final JSON body, after merging convenience flags, against the same schema `flint schema input` prints. A missing required field, wrong type, invalid value, or unknown field fails locally with exit code 2 and `INVALID_REQUEST_BODY`. `error.details.problems` lists each problem and its JSON path, such as `$.default_variant.unit_price_money.currency`; likely misspellings include a "did you mean" suggestion. Problems caused by convenience flags identify the flag and set `error.param` to its name. Currency convenience flags accept lowercase values and send uppercase currency codes. The retired `_flint` control object is rejected; terminal commands use flags and MCP calls use top-level controls. Invalid JSON reports its line and column. Use the raw `flint api` command to send undocumented fields without local schema validation.

Add `--dry-run=client` to validate and inspect the prepared request locally, with or without credentials:

```bash
flint products create --input product.json --dry-run=client --output json
```

Client dry runs make no API requests. When no credential is configured, they use a sandbox context, or live with `--live`, and warn that scope, merchant-guard, and sandbox-guard checks were skipped. Configured credentials retain their environment and guard checks; an unreadable credential store still reports an error. Workload identity dry runs validate source configuration without requesting or exchanging tokens. Client dry runs do not check server permissions or server-side business rules, such as whether referenced IDs exist. Remove `--dry-run=client` to submit the create request; execution requires a credential.

The same workflow applies to other primitives. Inspect each schema, prepare the corresponding JSON request body file, then create it:

```bash
flint schema input orders.create --output json
# Prepare order.json using the fields and requirements in the schema.
flint orders create --input order.json

flint schema input subscription-plans.create --output json
# Prepare plan.json using the fields and requirements in the schema.
flint subscription-plans create --input plan.json
```

`flint support open` opens the Flint Help composer; you review and post in the browser. Use `--private` for a private thread or `--no-open` to print the link only.

```bash
flint support open --title "Webhook delivery failed" --body "My endpoint returned 500." --area webhooks --private
```

The CLI sends `--title` and `--body` as URL parameters, preserving multiline text, and Flint Help prefills the composer with them. Accepted product areas are `checkout`, `payments`, `payouts`, `disputes`, `invoices`, `subscriptions`, `payment_links`, `api`, `webhooks`, `dashboard`, `documentation`, `flint_billing`, `account`, and `sales`.

Add `--ai-agent` to identify the reporter as an AI agent. It defaults to false and accepts `--ai-agent=false`. The CLI emits `ai_agent=true` in the link when enabled and a boolean `ai_agent` in JSON output. Flint Help records the thread as reported by an AI agent; this is self-reported metadata, not verified identity.

`flint api METHOD /v1/PATH` also accepts canonical relative public paths absent from the embedded snapshot. It warns with the CLI version and snapshot hash, skips unavailable body and scope validation, and keeps credential, environment, redirect, and write-confirmation checks. Unknown writes forward `--idempotency-key` when supplied, generate no key, and run once without automatic retries. Absolute URLs, fragments, traversal, and ambiguous path separators are rejected. MCP operations remain limited to the snapshot.

`flint openapi get` and `flint api get /v1/openapi.json` accept up to 64 MiB after decompression; other API responses retain the 16 MiB limit. Doctor discovers the server's current API version through an unauthenticated, unpinned GET of the configured base URL's OpenAPI endpoint, reads its version header, and closes the body. Discovery warnings do not stop the later CLI-version check.

Live partner OAuth installs require `--mode live --live` and confirmation (`--confirm` for scripts), including when using `flint api get /v1/oauth/authorize`. Raw API pagination (`--all` or `--paginate`) accepts only read-only GET operations.

### Trigger sandbox events

`flint trigger` creates sandbox resources through the public API to produce a webhook event:

```bash
flint trigger --list
flint listen --event-type order.paid
# In another terminal, with the same sandbox selected:
flint trigger order.paid
flint trigger dispute.won --for 2m
flint trigger test_clock.ready --for 5m
```

Trigger refuses live and unverified credentials before creating resources. `--list` works offline.
The output names the primary expected event, related events that also arrive, resource IDs, and
request IDs. Event delivery can finish after the command returns. Disputes need Stripe webhook
forwarding; subscription and test-clock recipes need background workers.

Every write uses a separate idempotency key under one run prefix. For resumable recipes, use the
printed recovery command to replay completed writes and continue. It preserves the resolved
profile, OAuth context, merchant guard, explicit credential store, and wait limit. Keep the same
API server, environment credential (if any), and project sandbox guard when resuming.
Checkout-session recipes cannot resume: a paid session credential cannot replay payment.
They omit recovery commands and reject `--idempotency-key` before any request; start a new run.
`--dry-run=client` authenticates and prints the recipe without writing.
Resuming accepts wait states that have already progressed. The monthly clock recipe advances to
one calendar month after the clock's original frozen time (clamped to the month's last day), plus
one minute to include due work. That target stays the same when the run is resumed. `--output json` returns one
result; `--output ndjson` streams finished steps and bounded wait observations. Trigger creates new
test resources and does not delete them. Reset the sandbox when finished.

## Scripts and agents

Lists return one page. Check `next_page_token`; it is omitted when no more pages remain. Pass it with `--page-token TOKEN`, use `--all` for every page, or `--limit N` to collect up to N resources automatically. `--limit` requests `min(remaining, 100)` items on each page and preserves the exact continuation token. If a server returns more items than requested, the CLI truncates the result and omits the token to avoid skipping resources.

Use `--output ndjson` on paginated list commands to print one resource per line as each page arrives, without an envelope or collection array. Timeline output prints individual `data.entries`. It fetches one page by default; add `--all` or `--limit N` to fetch more. Continuation tokens are never printed in this mode. `--select` chooses resource fields on each page; `--jq`, `--field`, and `--wait-for` are rejected. Errors use the normal JSON error shape on stderr and a nonzero exit code; resources already printed remain on stdout if a later page fails.

Use `--expand a,b` for documented relationships; command help lists valid expansions, and `flint schema command <name> --output json` includes an `expansions` array. Time filters accept RFC 3339 instants, dates, rolling durations, and calendar words such as `today` in the reporting timezone.

`@last` and `@last.<prefix>` (for example `@last.pi`) refer to the most recent matching resource this profile created or changed. Reads do not update history. History is shared by every process using the profile; use explicit IDs when running commands in parallel. `--field`, `--select`, and `--wait-for` paths start inside the resource: use `--field email`, not `--field data.email`. On lists, paths apply to each element. Timeline NDJSON paths refer to each emitted entry. Numeric indexes (`line_items.0.name`) and metadata keys are supported. The CLI checks paths against the output schema before sending a request; an unknown field exits 2 with `UNKNOWN_OUTPUT_FIELD` and a suggestion. A mistaken `data.` prefix returns `OUTPUT_FIELD_ROOT`; webhook resources that declare their own `data` field keep it. Declared but absent or null fields render as `null`. `--select` preserves the envelope, including `next_page_token`, `request_id`, and `meta`. `--field` prints strings raw and other values as compact JSON, with one line per resource on lists. Use `--jq` to read the envelope, for example `--jq .next_page_token`.

`FLINT_OUTPUT` accepts human, json, or ndjson (streams and paginated lists), case-insensitively. Its ndjson setting falls back to JSON for other commands; explicit `--output ndjson` requires a stream or paginated list. `FLINT_NO_INPUT` accepts `1`, `true`, `yes`, and `on`, case-insensitively. Boolean flags accept `--flag=true` and `--flag true` forms.

JSON errors use `{"error": {...}}` with `type`, `code`, `message`, optional `param`, object-shaped `details`, and `request_id` when available. HTTP responses add `http_status`. `retryable` is true when sending the same request again is safe and may succeed: no usable response arrived, or Flint returned `remediation.retry: "same_request"`. Responses without a Flint error envelope retry on HTTP 429, 500, 502, 503, or 504. `--timeout` (fixed at MCP server startup) is the total request budget, including retries, and defaults to 150 seconds for every API operation. Each attempt gets the remaining budget. Reads and keyed writes make at most 3 attempts within that budget, honoring `Retry-After` and using jittered backoff otherwise. Writes that take no key are sent once. CLI remediation appears at `error.remediation` and also at `error.details.remediation` for compatibility. Unknown-command and unknown-flag details contain `input` and a `suggestions` array, which can be empty. Failed waits return `WAIT_CONDITION_UNMET` or `WAIT_CONDITION_IMPOSSIBLE` with `details.conditions`, `details.observed`, and `details.last`. Exit codes are 0 success, 1 API or webhook delivery error, 2 usage, 3 auth/config (including HTTP 403), 4 confirmation required, 5 network/timeout (including streams that never connect), 6 wait or expected event count unmet, and 70 internal error.

CLI-local timeout and cancellation codes are `CLIENT_TIMEOUT` and `CLIENT_CANCELED`. Invalid local API-key formats use `CREDENTIAL_FORMAT_INVALID`; API responses named `INVALID_CREDENTIAL` pass through unchanged. When retries end without a definite result after a timeout or ambiguous transport failure, or a keyed write ends on a 5xx, `IDEMPOTENCY_KEY_IN_PROGRESS`, `IDEMPOTENCY_RECOVERY_REQUIRED`, or reason `outcome_unknown`, the error reports `details.outcome: "unknown"`, `details.retry: "none"`, and `retryable: false`. It retains `details.idempotency_key` for writes, `attempt_count`, per-attempt `duration_ms`, HTTP statuses and error codes, the `request_ids` seen, and `recovery_reads`. A final API error is retained unchanged in `details.last_error`, including its own remediation. Confirm and capture recover by reading the payment intent; order pay reads the order and its payment attempts filtered by the key; refund create lists refunds filtered by the key. An empty recovery list does not prove that the write had no effect. Read before deciding whether to call the write again. For operations in the snapshot that accept idempotency keys, retrying with the same key is safe and returns the original result. Unknown raw routes have no local replay guarantee.

Failures after a keyed write is dispatched include `details.idempotency_key` as recovery information. Follow `remediation.retry`: `same_request` means back off and resend the identical request with the same key; `after_refresh` means read current state, rebuild, and use a new key; `none` means follow the supplied step without retry advice. Human output includes the server's `next_steps`. Local validation, confirmation, and scope failures do not add key or retry advice. `Retry-After` is also reported as `retry_after_ms` on JSON errors. `--confirm` is accepted on every mutation, so scripts can pass it defensively. Operations the shared operation index classifies as irreversible require confirmation in the terminal: an interactive terminal asks "Continue? [y/N]"; scripts need `--confirm`. Existing confirmation requirements still apply. Before merchant-route mutations, `FLINT_ACCESS_TOKEN` is introspected for environment and guard checks. If introspection cannot verify the environment, sensitive operations require `--confirm` with `UNVERIFIED_ENVIRONMENT_CONFIRMATION_REQUIRED`; network failures stop the command.


Time filters accept RFC 3339 instants, dates (`YYYY-MM-DD`), `today`, `yesterday`, `this_week`, and rolling durations such as `2h`, `7d`, or `2w`. Dates and calendar words start at midnight in the merchant's reporting timezone; weeks start on Monday. The CLI caches the timezone from `GET /v1/merchant` for one hour. Without `merchants.profile.read`, or during an offline dry run without a cached value, it uses UTC and prints a note. Resolved values appear on stderr and in MCP notes. Body timestamps still require RFC 3339. Query enum flags accept only the values in the API schema; repeat array flags to send more than one value.

## MCP server

`flint mcp serve` exposes five Model Context Protocol tools over stdio: `flint_search`, `flint_describe`, `flint_read`, `flint_write`, and `flint_help`. The server uses your CLI credential and profile.

Find an operation, inspect its public arguments, then call it:

```json
{"name":"flint_search","arguments":{"query":"refund an order"}}
{"name":"flint_describe","arguments":{"operation":"refunds.create"}}
{"name":"flint_write","arguments":{"operation":"refunds.create","arguments":{"order_id":"ord_123","refund_type":"full"},"idempotency_key":"refund-ord-123-1","dry_run":true}}
```

Operation IDs follow CLI commands, such as `orders.list` and `checkout-sessions.delivery-quotes.create`. Read and write calls also accept space-separated command names and typed tool names. Use public JSON argument names from `flint_describe`; CLI convenience flags are unavailable over MCP. Pass execution controls such as `select`, `idempotency_key`, and `dry_run` alongside `operation`, outside `arguments`. The former `_flint` object is removed.

Successful results contain one compact JSON text block and, when needed, one notes block with the environment, notes, and warnings. No result is duplicated in `structuredContent`. Reads have a 49,152-byte cap; narrow larger results with `select`, filters, or a smaller `page_size`. MCP lists default to 10 items per page. `all: true` stops fetching as soon as the projected result exceeds the limit. Completed writes return their full result with a warning if oversized.

`select` paths are checked before sending. Selected writes retain the resource's ID and status. If a declared optional field is absent from a completed write, the result remains successful and includes a warning.

Failed calls return one JSON text block with `error` and `cli`. API errors pass through unchanged, including their detail arrays and `error_source`. Local failures use `error_source: "caller"`; invalid inputs use `INVALID_REQUEST` with public field paths in `details[].param`. The `cli` block identifies the origin, operation, exit code, environment, and who must act next. Follow agent actions directly; tell the user about actions marked `user`. Unknown write outcomes include the sent idempotency key and a recovery read when available.

`flint_describe` supports `kind: "result"`, nested `path`, `depth` from 1 to 3, and request `variant`. Pass `event` instead of `operation` to inspect a webhook payload. `flint_help` lists resource groups or explains `safety`, `errors`, `events`, and `test-cards`.

Add the server to Claude Code:

```sh
claude mcp add --transport stdio flint -- flint mcp serve
claude mcp add --transport stdio flint-read -- flint mcp serve --read-only
```

Cursor and other clients that use `mcpServers`:

```json
{
  "mcpServers": {
    "flint": {
      "command": "npx",
      "args": ["-y", "@flintpay/cli", "mcp", "serve"],
      "env": {"FLINT_PROFILE": "development"}
    }
  }
}
```

- `--read-only` removes the write tool and refuses writes, including direct calls.
- `--tools orders,refunds` opts into one typed tool per operation in those groups, plus search, describe, and help restricted to that scope. `--tools all` selects every available operation. `--tools default` is removed. Typed names use lowercase letters, digits, and underscores for both dots and hyphens, such as `payment_intents_create`; old dotted or hyphenated tool names are rejected. Keep the client server name at most five characters (for example, `flint`) so prefixed names fit 64 characters. Typed mode has no `flint_read` or `flint_write` tools. Its compact schemas show two levels of fields; use `flint_describe` to open deeper objects. Calls still validate the full schema, including variant requirements. Execution controls are top-level properties, with required idempotency keys and confirmation rules shown per operation.
- Credential class restricts discovery and calls. A merchant API key cannot reach buyer-session operations; a partner access token reaches only install-scoped operations. Raw `api`, `help_search`, and `schema_*` tools are removed.
- `--allow-live` permits live credentials; each live call must also carry `live: true`. The server's profile, merchant guard, context, and timeout are fixed at startup.

Use `dry_run: true` to validate a write without sending it. The prepared request includes an `effect` with its confirmation rule, resource IDs, and amounts formatted in the currency's minor units. Dry runs need neither `confirm` nor live acknowledgement. Required idempotency keys are checked on dry runs too. Reuse the same key when retrying an uncertain write. Sensitive or irreversible operations need `confirm: true` in every environment, including sandbox refunds. In sandbox the agent confirms after checking the effect; the server never asks the user. In live or unverified environments, `confirm: true` also asks the user through MCP elicitation. Only an accepted approval runs the request. Without elicitation, `CONFIRMATION_NEEDS_USER` supplies an exact terminal command using the same request and idempotency key. Run it with the same credential this server uses and from the same project directory; the command omits `--confirm` so the terminal prompts. `--allow-destructive` permits agent confirmation in live or unverified environments; it does not change sandbox gating.

| Environment | Reads and ungated writes | Sensitive or irreversible writes |
|---|---|---|
| Sandbox | Run | Require `confirm: true`; no user prompt |
| Live, elicitation | Require `--allow-live` and per-call `live: true` | Also require `confirm: true` and user approval |
| Live, no elicitation | Require `--allow-live` and per-call `live: true` | Return `CONFIRMATION_NEEDS_USER` and a terminal command |
| Live, `--allow-destructive` | Require `--allow-live` and per-call `live: true` | Also require `confirm: true` |
| Read-only server | Reads run under the environment rules | All writes are unavailable, including dry runs |

Unverified credentials follow live confirmation rules, without the verified-live access checks. Local operations such as `history.clear` count as sandbox. Typed tools include the canonical operation ID in `_meta["com.withflintpay/operation"]` and mark sensitive operations separately. `destructiveHint` describes irreversible writes and overwrites; the confirmation gate covers sensitive or irreversible operations.

Local commands are operations reached through read or write, including `version`, read-only `doctor`, `history`, `history.clear`, and sandbox-only `trigger`. Trigger waits are capped at 60 seconds. Agents cannot change credential stores, update config, or log out through MCP.

## Migrating from 0.13.0

0.14.1 adds `--access` and `--scope` to `api-keys create`, `api-keys update`, `sandboxes api-keys create`, and `sandboxes create`; `--scope` alone selects restricted access. 0.14.2 fixes `flint listen` withholding every event from current Flint servers. 0.14.3 raises the total timeout budget to 150 seconds, adds explicit file credential storage and NDJSON lists, and renames CLI-local error codes to `CLIENT_TIMEOUT`, `CLIENT_CANCELED`, and `CREDENTIAL_FORMAT_INVALID`. Scripts matching the old codes must be updated. See [the 0.14.3 migration notes](docs/migrating-0.14.md) for details.

## Migrating from 0.12.0

0.13.0 moves every command to Flint's canonical API paths, so it needs a Flint API that serves them; for example, it checks credentials with `GET /v1/auth-context`, `developer partner apps` commands are now `partner-apps`, and `me payments` commands are now `me payment-intents`. Convenience flags set the union tags request bodies now require, such as `credential_type` on `payment-intents confirm`, and bot-check tokens come from `FLINT_CHALLENGE_TOKEN`. It also adds webhook endpoint and delivery rate callback signing method flags, which default to JWT and return a shared secret only when HMAC is included, partner-app credential, tax-connection, and `disputes accept` commands, requires `--access` on `api-keys create`, and removes older subscription actions and the address `set-default` commands. See [the 0.13.0 migration notes](docs/migrating-0.13.md) for details.

## Migrating from 0.11.0

0.12.0 refreshes the embedded public API snapshot, which renames and removes commands; for example, `payout-settings destinations` commands are now `payout-destinations`, `merchant-account-sessions refresh` is now `merchant-account-session-refreshes create`, and `flint organizations create` requires `--parent-organization`. See [the 0.12.0 migration notes](docs/migrating-0.12.md) for the command and MCP tool changes.

## Migrating from 0.10.0

0.11.0 refreshes the embedded public API snapshot, which renames and removes commands; for example, `reviews` commands are now `payment-reviews` and `flint sandboxes test-key` is now `flint sandboxes api-keys create` (the old name still works). See [the 0.11.0 migration notes](docs/migrating-0.11.md) for the command and MCP tool changes.

If upgrading from 0.9.0, also follow [the 0.10.0 migration notes](docs/migrating-0.10.md) for required idempotency keys, retry rules, and error fields. If upgrading from 0.8.0, also follow [the 0.9.0 migration notes](docs/migrating-0.9.md) for the public API refresh's command and MCP tool lists, parameter changes, and schema field inventory. In particular, use `flint gift-cards adjust-balance`, `flint gift-cards cash-out`, and `flint webhook-deliveries list --webhook-event-id whev_123`; create refunds with an explicit `--refund-type full|amount|targets`.

## Develop

```bash
make build      # bin/flint
make test       # go test ./...
make contract   # tests, then diff the API coverage report against coverage.json
make coverage   # regenerate coverage.json after intentionally changing the surface
```

After `make build`, run `./bin/flint --help` to inspect workspace changes. Use `./bin/flint` for subsequent commands to run the binary you just built; `flint` resolves through your shell's `PATH`.

Layout:

| Path | What it holds |
| --- | --- |
| `cmd/flint` | Entry point; version, commit, build date, and schema hash are injected at link time. |
| `cmd/flint-coverage` | Emits `coverage.json`: every public route and whether a first-class command covers it. |
| `internal/cli` | Command registry, parsing, transport, output, and the local commands. |
| `internal/opindex` | Offline operation, event, and topic metadata with shared credential classification and search ranking. |
| `internal/schemaview` | Reference resolution, variant-aware schema views, and bounded schemas for discovery tools. |
| `internal/spec` | The embedded public OpenAPI snapshot every command is built against. |
| `internal/contract` | Coverage report generation. |
| `e2e` | Tests that run the built binary against a local server. |

Sandbox and live requests both use `https://api.withflintpay.com`. Browser login binds the OAuth session to the approved environment; imported API keys retain their own environment. Imported API keys and legacy sessions require `--live` to acknowledge production access; multi-context sessions use the selected context. Set `FLINT_BASE_URL` before signing in to a development server, using a separate profile. Saved OAuth sessions stay bound to that server and reject a conflicting URL override. A trailing `/v1` in `FLINT_BASE_URL` is stripped with a warning on stderr unless `--quiet`.

`internal/spec/openapi.json` is the public API snapshot used to build the command catalog. The reviewed source commit, export path, SHA-256, operation count, and operation-set SHA-256 are pinned in `internal/spec/provenance.json` and checked offline by `TestSnapshot`. Intentional lag is recorded there; startup and tests never fetch a newer export. When updating it, use `git show <reviewed-commit>:web/apps/developers/src/generated/openapi.json` from a local Flint repository, update the provenance pin and coverage classifications, run `make coverage`, and run `make contract`. Set `FLINT_SPEC_REPO` to that local repository when running `go test ./internal/spec` to also compare the pinned source bytes and operation set. The coverage report checks that every public operation has a dedicated command or an explicit exclusion with a reason. Feedback commands are retired; use `flint support open` to start a thread on Flint Help.

Tests use local HTTP servers and synthetic credentials. They do not require an API key or write to a Flint account.

## Release

Follow [RELEASING.md](RELEASING.md) for one-time publishing setup, release PRs, tagging a reviewed commit on `main`, installation verification, and recovery from partial publication.

Push a `cli/vX.Y.Z` tag. [`.github/workflows/cli-release.yml`](.github/workflows/cli-release.yml) runs the tests and a sandbox listener check, builds every platform with GoReleaser, then publishes the GitHub release with build provenance, the npm platform packages plus the `@flintpay/cli` wrapper, the container image, and the Homebrew formula in `flint-pay/homebrew-tap`. A tag with a prerelease suffix publishes to the npm `next` tag and skips Homebrew.

GitHub and npm publication verify the contents of an existing release before accepting a rerun.

Before tagging a release, create `flint-pay/homebrew-tap` and configure `FLINT_TEST_API_KEY` in the `cli-release-staging-sandbox` GitHub environment, npm trusted publishing for the six packages, and `HOMEBREW_TAP_TOKEN` for the tap. Use a disposable staging sandbox key for the listener check against `https://api.staging.withflintpay.com`, which creates a customer; never use a live key. Configure the environment to require release approval as described in [RELEASING.md](RELEASING.md#one-time-setup).

The npm trusted publisher must name the `npm` GitHub environment and the `cli-release.yml` workflow. Release installation tests run before publication; public npm, manual, and stable Homebrew installations are verified afterward.

## License

Apache-2.0. See [LICENSE](LICENSE).
