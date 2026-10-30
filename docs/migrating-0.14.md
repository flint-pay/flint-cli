# Migrating to Flint CLI 0.14.3

This release adds API key access flags. Existing commands and `--input` bodies keep working. 0.14.0 was not released: its npm wrapper package never published, so install 0.14.3.

## 0.14.3 changes

### Breaking error-code renames for scripts

Update scripts that match CLI-local error codes:

| Previous code | 0.14.3 code |
| --- | --- |
| `REQUEST_TIMEOUT` | `CLIENT_TIMEOUT` |
| `REQUEST_CANCELED` | `CLIENT_CANCELED` |
| Local `INVALID_CREDENTIAL` | `CREDENTIAL_FORMAT_INVALID` |

API errors named `INVALID_CREDENTIAL` still pass through unchanged. These local renames are breaking changes for scripts in 0.14.3.

### Timeouts and recovery

- `--timeout` and MCP `_flint.timeout` now default to a total 150-second budget for every API operation. Each attempt gets the remaining budget; there is no separate ten-second attempt cutoff. Explicit timeout overrides still apply.
- When a request may have reached Flint but no definite result arrives, errors retain attempt facts, request IDs, the write's idempotency key, and operation-specific recovery reads. Keyed writes also report an unknown outcome after exhausted 5xx or in-progress/recovery responses. `details.outcome: "unknown"`, `details.retry: "none"`, and `retryable: false` mean to read the resource before deciding what to do next. An empty recovery list does not prove that a write had no effect.
- Retry advice follows the API's `remediation.retry`: `same_request` allows the identical request with the same key; `after_refresh` requires reading and rebuilding before using a new key; `none` supplies no retry instruction. For known operations that accept idempotency keys, reusing the same key is safe and returns the original result.
- MCP preserves the complete successful write result with a warning when `select` cannot find a path, instead of replacing a committed result with an error.

### Credentials and scopes

- Choose `--credential-store keyring|file` or `FLINT_CREDENTIAL_STORE` for authentication. The OS keyring remains the default. The explicit file backend stores credentials separately from config with owner-only permissions; it is not encrypted by the OS keyring. The selected backend persists per profile, including pending signup and login continuation. There is no automatic plaintext fallback.
- Fresh profiles skip keyring lookup, so first-run setup no longer waits on the keyring. Doctor continues public diagnostics when credentials are missing or unavailable, and identifies file storage and environment credentials as alternatives.
- Cached scope denials refresh once before refusing a command. `auth status` and `whoami` fetch current auth-context metadata. Sandbox scope guidance identifies live-only requirements instead of suggesting scopes a sandbox key cannot obtain.

### Commands and output

- The refreshed embedded API snapshot adds `test-clocks` and `disputes simulate`.
- Terminal `flint api` accepts unknown canonical relative `/v1/` routes with a snapshot warning and existing credential, environment, and confirmation guards. Unknown writes run once, forward a supplied idempotency key without generating one, and have no local replay guarantee. MCP raw tools remain limited to the snapshot.
- OpenAPI downloads accept documents up to 64 MiB decompressed and preserve error request IDs. Doctor checks the configured server's unpinned API-version header without downloading the document, and continues after discovery warnings.
- `schema errors` lists the sorted union of CLI and embedded API error types. `schema events` lists compact event types by default and accepts a type or prefix to inspect payload schemas.
- `support open --area disputes` is accepted. The README now correctly states that Flint Help prefills the title and body from support links.
- `--dry-run=client` works offline without credentials, using a sandbox context unless `--live` is set. It reports skipped scope and merchant/sandbox guard checks. Configured credential-store errors remain errors, and actual execution still requires credentials.
- Paginated lists accept `--output ndjson`, emitting one resource per line as pages arrive. One page is fetched by default; use `--all` or `--limit` to fetch more. `--select` projects each page; `--jq`, `--field`, and `--wait-for` are rejected in this mode. If a later page fails, earlier lines stay on stdout and the JSON error goes to stderr with a nonzero exit code.

## 0.14.2 fixes

0.14.2 needs no changes to commands or scripts.

- `flint listen` forwards and prints events from current Flint servers again. They send each event as the delivery envelope (`webhook_event_id`, `event_type`, `payload_version`, `data`, and so on) without a `payload` field, so 0.13.0 and 0.14.1 withheld every event, even for a full-access key. The listener now forwards that envelope, the same body a webhook delivery carries, and still reads `payload` from older servers.
- Human output shows future timestamps as `in 8d` instead of `8d ago`, for example a dispute's evidence deadline.

## API key access flags

Flint now requires `access` whenever a request sends `scopes`, so every CLI key command sends `access`.

- `api-keys create`, `api-keys update`, `sandboxes api-keys create`, and `sandboxes create` accept `--access full|read_only|restricted` and repeatable `--scope` flags. `--scope` without `--access` selects `restricted`, as the Dashboard does; `--scope` with `full` or `read_only` is rejected.
- `api-keys create` no longer requires `--access` when you pass `--scope`. Creating a key needs an access choice: `--access`, or `--scope` for restricted.
- `sandboxes api-keys create` also takes `--name`. `sandboxes create` issues a test key when you pass `--access`, `--scope`, or `--test-key-name`, sending `issue_test_key: true`.
- `api-keys update` is a curated command with `--name` and `--expected-version`. Changing `access` or `scopes` requires `expected_version`.
- `--input` bodies keep their own fields: a body with `scopes` must also set `access`, and the CLI rejects it locally otherwise. MCP tools follow the same request schema.

```sh
flint api-keys create --name local-dev --scope payments.payment_intents.read
flint api-keys update key_123 --scope customers.read --expected-version 3
flint sandboxes test-key test_123 --name qa --access read_only --live
flint sandboxes create --name qa-scenarios --scope customers.read --live
```
