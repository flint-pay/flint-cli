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

The first command succeeds with `data.status` set to `verification_required`. The CLI stores the temporary verification token, email, and name in the private profile config and prints the command to finish. Resume using the same profile and API server; no name or email flags are needed. Successful verification saves the onboarding session and its expiry until signup finishes, so retrying after a later failure does not consume another code attempt. If progress is missing, the code expires, or five attempts are used, start again without `--verification-code`. MCP's `signup` tool (enable it with `--tools signup` or `--tools all`) uses the same two calls: first pass `email`, `first_name`, and `last_name`, then pass only `verification_code`. It uses the server's profile and refuses to run when that profile already has a saved credential.

Successful signup saves and activates the new sandbox key, replacing the profile's previous login context. JSON output includes `data.next_command`. For a custom API server, the printed continuation and payment commands include the required `FLINT_BASE_URL`; keep that environment setting when using the saved key. If the project pins an OAuth context in `.flint/config.json`, run signup outside the project or remove that pin first. Make your first test payment with:

```sh
flint payment-links create --name "Test payment" --item-name "Test payment" --amount 1000 --currency USD --open
```

`--open` opens the payment link in your browser in an interactive terminal with human output. The URL is printed either way; JSON output, non-interactive runs, and `--no-input` never open a browser. Pay with card 4242 4242 4242 4242, any future expiry date, and any CVC.

## Sign in (recommended)

For local development, start with `flint login`. It opens the Flint website, displays a confirmation code, and saves OAuth access and refresh tokens in your OS keychain. The printed approval link includes the pairing code, and the code also appears on its own line. Use `--no-open` to open the printed link yourself, `--profile NAME` to use or create a profile without changing the default profile, or `--live` to explicitly request production access. Login waits up to 10 minutes by default; `--timeout 5m` overrides that limit. With `--no-input`, JSON output, or no TTY, it prints the approval link and code without opening a browser. JSON polling emits a `login_pending` JSON record on stderr with `verification_url`, `user_code`, and `expires_at`.

Use `flint login --no-wait --output json` to start approval and exit successfully with `data.status: "approval_pending"`, the link, code, expiry, and `next_command`. Run `next_command` to resume polling with the same profile, `--live` acknowledgement, scopes, and context guards; keep the same API server and project sandbox guard. Conflicting environment or guard flags fail before redeeming approval and preserve the pending login. Pending device approval is saved in its own private 0600 file in the config directory and removed when completed or expired. Saved OAuth tokens remain in the OS keyring.

If browser login is unavailable, use `flint auth import` instead. Browser approval requires a person; automation should use [workload identity](#workload-identity-and-automation), with an API key as a fallback.

Browser login grants baseline permissions for common development workflows. Some command groups, including subscriptions, payouts, reports, risk, and advanced commerce operations, require additional access. Add scopes to the active context with `flint reauth --scope SCOPE`; repeat `--scope` when an operation requires multiple scopes. The browser may approve only part of a request, and the CLI reports what was approved. Known command requirements are checked before a request is sent.

Access tokens refresh automatically as needed. `flint auth logout --confirm` revokes the OAuth session before removing its local credentials. If the server does not confirm revocation, logout reports failure and retains the credentials for retry. Imported API-key logout still removes only the local key.

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

Environment credentials (`FLINT_API_KEY`, `FLINT_ACCESS_TOKEN`, `FLINT_WORKLOAD_IDENTITY_ID`, or `FLINT_CHECKOUT_SESSION_SECRET`) override configured browser-session contexts. An explicit `--context` cannot be combined with these overrides. Merchant and sandbox guards still apply.

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

A locked or unavailable OS keyring fails with guidance after a bounded wait. For SSH, containers, CI, and agents, use workload identity or the `FLINT_API_KEY` fallback; there is no file-based API-key or OAuth credential store. `flint auth import --profile NAME`, `flint login --profile NAME`, and `flint signup --profile NAME` create missing profiles without changing the default profile.

API-key auth-context metadata is cached in memory and in a separate 0600 file for five minutes, keyed by a SHA-256 hash of the key and API base URL. The cache contains no API key. A later HTTP 401 or 403 invalidates it; OAuth credential handling is unchanged.

Configuration selection uses `--profile`, `FLINT_PROFILE`, a project pin in `.flint/config.json`, then the default profile. With `FLINT_API_KEY`, `FLINT_ACCESS_TOKEN`, or `FLINT_WORKLOAD_IDENTITY_ID`, a missing profile is allowed and scopes resource history. Empty config files are accepted and unknown fields are ignored; `flint config validate` reports unknown fields as warnings. Use `flint config unset merchant` to clear the selected profile's saved merchant guard. Project and environment guards still apply, and mismatch errors identify their source.

## Use

```bash
flint login         # recommended: approve browser sign-in; credential saved in the OS keychain
flint doctor        # config, credential, connectivity, environment, merchant, scopes, version
flint payment-links create --name T-shirt --item-name T-shirt --amount 2500 --currency USD --open
flint listen --forward-to http://localhost:8080/webhooks/flint
```

`flint listen` signs forwarded events with a secret it saves and reuses across runs; print it with `flint listen --print-secret`, or pin your own with `--webhook-secret` or `FLINT_WEBHOOK_SECRET`. Without `--forward-to` it prints events instead. `--forward-to` accepts localhost, loopback IPs, and `host.docker.internal`. With `--max-events` or `--for`, it exits 1 if any delivery failed, 5 if it never connected, and 6 if fewer than `--max-events` events arrived before `--for` ended; the checkpoint's `replay_cursor` resumes from the first failure with `--cursor`. `--save-to -` writes a downloaded file to stdout.

`flint`, `flint --help`, and `flint help` list starting points, API resource groups, and examples for creating primitives. `flint schema commands customers --output json` returns the commands under one group as data, such as `customers` or `customers addresses`; without a group, `flint schema commands --output json` returns the whole catalog, which is large. Root help with `--output json` or `FLINT_OUTPUT=json` returns a compact index instead. `flint schema command customers create --output json` describes one command; dotted names such as `customers.create` work too. Command descriptions and offline help topics return a `{"data": ...}` envelope in JSON mode. `schema input` and `schema output` are exceptions: they return raw JSON Schema documents. Human schema output is indented. `flint help events` lists event types with short descriptions; `flint help events customer.created` shows one event's details. `flint help errors` describes the error envelope; `flint help exit-codes` lists exit codes.

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

The product example sets a $25.00 USD price; money amounts use the smallest currency unit. Before sending, the CLI checks the final JSON body, after merging convenience flags, against the same schema `flint schema input` prints. A missing required field, wrong type, invalid value, or unknown field fails locally with exit code 2 and `INVALID_REQUEST_BODY`. `error.details.problems` lists each problem and its JSON path, such as `$.default_variant.unit_price_money.currency`; likely misspellings include a “did you mean” suggestion. Problems caused by convenience flags identify the flag and set `error.param` to its name. Currency convenience flags accept lowercase values and send uppercase currency codes. An `_flint` key, which only MCP tool calls use, is rejected; pass those controls as flags. Invalid JSON reports its line and column. Use the raw `flint api` command to send undocumented fields without local schema validation.

With credentials configured, add `--dry-run=client` to run the same checks and inspect the prepared request without making an API request or creating a resource:

```bash
flint products create --input product.json --dry-run=client --output json
```

Client dry runs use the saved or environment credential locally; sign in first if no credential is configured. Workload identity dry runs validate source configuration without requesting or exchanging tokens. They do not check server permissions or server-side business rules, such as whether referenced IDs exist. Remove `--dry-run=client` to submit the create request.

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

The CLI includes `--title` and `--body` as URL parameters, preserving multiline text. Browser prefilling requires the Help web app to accept these parameters; its existing link schema needs a corresponding update.

Add `--ai-agent` to identify the reporter as an AI agent. It defaults to false and accepts `--ai-agent=false`. The CLI emits `ai_agent=true` in the link when enabled and a boolean `ai_agent` in JSON output. The Help web app must read this parameter too; it is self-reported metadata, not verified identity.

Live partner OAuth installs require `--mode live --live` and confirmation (`--confirm` for scripts), including when using `flint api get /v1/oauth/authorize`. Raw API pagination (`--all` or `--paginate`) accepts only read-only GET operations.

### Scripts and agents

Lists return one page. Check `next_page_token`; it is omitted when no more pages remain. Pass it with `--page-token TOKEN`, use `--all` for every page, or `--limit N` to collect up to N resources automatically. `--limit` requests `min(remaining, 100)` items on each page and preserves the exact continuation token. If a server returns more items than requested, the CLI truncates the result and omits the token to avoid skipping resources.

Use `--expand a,b` for documented relationships; command help lists valid expansions, and `flint schema command <name> --output json` includes an `expansions` array. Time filters accept RFC3339 timestamps, `YYYY-MM-DD` at UTC midnight, durations such as `2h`, `7d` or `2w`, and `yesterday`.

`@last` and `@last.<prefix>` (for example `@last.pi`) refer to the most recent matching resource this profile created or changed. Reads do not update history. History is shared by every process using the profile; use explicit IDs when running commands in parallel. `--select` and `--field` render missing optional fields as null; a field absent from every resource and the output schema is an error.

`FLINT_OUTPUT` accepts human, json, or ndjson (streams), case-insensitively. Its ndjson setting falls back to JSON for non-streaming commands; explicit `--output ndjson` still requires a stream. `FLINT_NO_INPUT` accepts `1`, `true`, `yes`, and `on`, case-insensitively. Boolean flags accept `--flag=true` and `--flag true` forms.

JSON errors use `{"error": {...}}` with `type`, `code`, `message`, optional `param`, object-shaped `details`, and `request_id` when available. HTTP responses add `http_status`. `retryable` is true when sending the same request again is safe and may succeed: no usable response arrived, or Flint returned `remediation.retry: "same_request"`. Responses without a Flint error envelope retry on HTTP 429, 500, 502, 503, or 504. Reads and keyed writes make at most 3 attempts within `--timeout`, honoring `Retry-After` and using jittered backoff otherwise. Writes that take no key are sent once. CLI remediation appears at `error.remediation` and also at `error.details.remediation` for compatibility. Unknown-command and unknown-flag details contain `input` and a `suggestions` array, which can be empty. Failed waits return `WAIT_CONDITION_UNMET` or `WAIT_CONDITION_IMPOSSIBLE` with `details.conditions`, `details.observed`, and `details.last`. Exit codes are 0 success, 1 API or webhook delivery error, 2 usage, 3 auth/config (including HTTP 403), 4 confirmation required, 5 network/timeout (including streams that never connect), 6 wait or expected event count unmet, and 70 internal error.

Every error from a keyed write includes `details.idempotency_key`; reuse it when retrying the same request. `Retry-After` is also reported as `retry_after_ms` on JSON errors. `--confirm` is accepted on every mutation, so agents can pass it defensively. Existing confirmation requirements still apply. Before merchant-route mutations, `FLINT_ACCESS_TOKEN` is introspected for environment and guard checks. If introspection cannot verify the environment, sensitive operations require `--confirm` with `UNVERIFIED_ENVIRONMENT_CONFIRMATION_REQUIRED`; network failures stop the command.


## MCP server

`flint mcp serve` exposes Flint commands as Model Context Protocol tools over stdio, so an AI agent can call the Flint API with your credential. Tools are named `<resource>_<action>`, such as `customers_create`; dotted names such as `customers.create` are still accepted in tool calls and `--tools`. The server uses the same credential as the CLI: a saved `flint login`, the profile named by `FLINT_PROFILE`, or `FLINT_API_KEY`.

Claude Code:

```bash
claude mcp add --transport stdio flint -- flint mcp serve
claude mcp add --transport stdio --env FLINT_PROFILE=staging flint-staging -- flint mcp serve
```

Cursor (`.cursor/mcp.json`) and other clients that read an `mcpServers` JSON file:

```json
{
  "mcpServers": {
    "flint": {
      "command": "npx",
      "args": ["-y", "@flintpay/cli", "mcp", "serve", "--tools", "customers,payment-intents,payment-links,help"],
      "env": { "FLINT_API_KEY": "flint_test_..." }
    }
  }
}
```

Use a sandbox key (`flint_test_...`) or `FLINT_PROFILE` rather than a live key in shared config files.

Server flags:

- `--tools GROUP[,GROUP...]` chooses which tools to expose, and can be repeated. Each entry is a tool group such as `customers` or a single tool such as `payment-intents_create`; dotted names also work. Without `--tools` (or with an empty value), the server exposes a curated set for typical integrations: customers, products, payment intents, refunds, webhook endpoints and events, and core checkout session and payment link tools (about 30 tools, about 150 KB of compact `tools/list` result JSON). `--tools all` exposes every tool (about 600 tools, about 1.8 MB), and `--tools default,invoices` adds groups to the default set. `help` and the `schema_*` tools are always exposed. Order tools and `auth_status` are opt-in with `--tools default,orders,auth_status`. An unknown entry stops the server at startup.
- `--allow-live` lets tools act on live (production) credentials and contexts. Without it, any call that would use a live credential or a selected live context fails, and agents cannot pass `_flint.live`. With it, agents must still acknowledge each live call with `_flint.live`.
- `--allow-destructive` lets agents confirm destructive operations, and sensitive operations in live mode, with `_flint.confirm`. Without it, the server asks you through MCP elicitation when the client supports it, and otherwise refuses with instructions for you.

Agents can always inspect a write first with `_flint.dry_run: "client"` or, for destructive operations, `_flint.preview`. Successful results that reached the API include a `Flint environment: sandbox|live` line, followed by a `Warnings:` block when the CLI printed warnings. The `help` tool returns a short index without a topic, a tool's arguments and required fields when given a tool name, and control details for `controls`. `schema_input` and `schema_output` return a tool's full schemas on demand, and `schema_events` lists webhook event types (pass `event_type` for payload schemas). Agents can add a `_flint.merchant` guard but cannot replace the profile, merchant guard, or context the server uses. `config set`, `config unset`, and `auth logout` are not available over MCP.

## Migrating from 0.9.0

The next release is 0.10.0 because writes with required idempotency keys now reject calls that omit the key, and retry behavior changes. See [the 0.10.0 migration notes](docs/migrating-0.10.md) for affected commands, script and MCP changes, retry rules, and error fields.

If upgrading from 0.8.0, also follow [the 0.9.0 migration notes](docs/migrating-0.9.md) for the public API refresh's command and MCP tool lists, parameter changes, and schema field inventory. In particular, use `flint gift-cards adjust-balance`, `flint gift-cards cash-out`, and `flint webhook-deliveries list --webhook-event-id whev_123`; create refunds with an explicit `--refund-type full|amount|targets`.

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
| `internal/spec` | The embedded public OpenAPI snapshot every command is built against. |
| `internal/contract` | Coverage report generation. |
| `e2e` | Tests that run the built binary against a local server. |

Sandbox and live requests both use `https://api.withflintpay.com`. Browser login binds the OAuth session to the approved environment; imported API keys retain their own environment. Imported API keys and legacy sessions require `--live` to acknowledge production access; multi-context sessions use the selected context. Set `FLINT_BASE_URL` before signing in to a development server, using a separate profile. Saved OAuth sessions stay bound to that server and reject a conflicting URL override. A trailing `/v1` in `FLINT_BASE_URL` is stripped with a warning on stderr unless `--quiet`.

`internal/spec/openapi.json` is the public API snapshot used to build the command catalog. When updating it, replace it with a reviewed public OpenAPI export, run `make coverage`, and run `make contract`. The coverage report checks that every public operation has a dedicated command or an explicit exclusion with a reason. Feedback commands are retired; use `flint support open` to start a thread on Flint Help.

Tests use local HTTP servers and synthetic credentials. They do not require an API key or write to a Flint account.

## Release

Follow [RELEASING.md](RELEASING.md) for one-time publishing setup, release PRs, tagging a reviewed commit on `main`, installation verification, and recovery from partial publication.

Push a `cli/vX.Y.Z` tag. [`.github/workflows/cli-release.yml`](.github/workflows/cli-release.yml) runs the tests and a sandbox listener check, builds every platform with GoReleaser, then publishes the GitHub release with build provenance, the npm platform packages plus the `@flintpay/cli` wrapper, the container image, and the Homebrew formula in `flint-pay/homebrew-tap`. A tag with a prerelease suffix publishes to the npm `next` tag and skips Homebrew.

GitHub and npm publication verify the contents of an existing release before accepting a rerun.

Before tagging a release, create `flint-pay/homebrew-tap` and configure `FLINT_TEST_API_KEY` in the `cli-release-staging-sandbox` GitHub environment, npm trusted publishing for the six packages, and `HOMEBREW_TAP_TOKEN` for the tap. Use a disposable staging sandbox key for the listener check against `https://api.staging.withflintpay.com`, which creates a customer; never use a live key. Configure the environment to require release approval as described in [RELEASING.md](RELEASING.md#one-time-setup).

The npm trusted publisher must name the `npm` GitHub environment and the `cli-release.yml` workflow. Release installation tests run before publication; public npm, manual, and stable Homebrew installations are verified afterward.

## License

Apache-2.0. See [LICENSE](LICENSE).
