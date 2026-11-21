# Changelog

## 0.22.0 (2026-11-21)

- Add the card fingerprint, email, and IP address from a failed or blocked payment attempt to the default block lists with `flint payment-intents attempts block-signals <payment_intent_id> <payment_attempt_id>`. Existing entries are kept, and MCP routes the operation through `flint_write`. The embedded API snapshot now matches Flint `cfe039b718`.
- Payment intent attempt arguments expect `pia_` IDs, and order payment attempt arguments expect `opat_` IDs. MCP keeps `checkout_session_id` in selected results from `checkout-sessions.create` and `me.checkout-sessions.create`.
- `flint payout-destinations list` shows each destination's bank name, last four digits, currency, and whether it is the default for that currency.
- `flint support open` opens the staging Help site when the CLI uses the staging API, including through a saved staging login.
- On Linux, `flint signup` saves a new profile's credential in the private credential file when the default keyring is unavailable and prints the file's path. An explicit `--credential-store keyring` still requires a working keyring.
- The README shows how to run the published container image.

## 0.21.0 (2026-11-19)

- Manage license keys and pools from the CLI or MCP. Import keys from text or CSV with `flint license-key-pools add-keys`. MCP routes license lookup and validation through `flint_read`.
- MCP redacts credentials from tool results and discards credentials echoed in errors. Credentials issued by successful calls and eligible remediation links go to a private file on the machine running Flint. Open the file there and delete it after use.
- Set the business time zone on the first signup call with `flint signup --timezone` or the MCP `timezone` field. Use an IANA name such as `America/Chicago`.
- `flint listen` retries webhook signing after throttling and transient failures and retains pending events across transport failures. Help search prioritizes listener and signing commands for webhook verification queries.
- The CLI and MCP validate positive payment amounts locally, including payment link amounts, payment intent amounts, and explicit capture amounts.
- Buyer subscription previews use `flint_write` in MCP. The CLI advertises idempotency only for API operations that support it.
- Help search finds commands for common merchant tasks. Unscheduled invoice drafts avoid the confirmation required to issue or schedule an invoice, and revoking the active API key warns that calls with that key will stop.
- `flint settings get` and `flint settings update` include the `receipts` section. Set `receipts.footer_text` to change the receipt note. Use `flint orders edits cancel` to cancel an order edit.
- Customer phone flags explain the required E.164 format and that the CLI trims surrounding whitespace.

## 0.20.5 (2026-11-17)

- MCP servers pin their startup context and credential. Changing the saved selection or replacing the credential returns `MCP_CONTEXT_CHANGED` and requires a server restart. OAuth refresh and live-access safeguards remain in effect.

## 0.20.3 (2026-11-16)

- Default browser login requests permissions for shop operations and financial reads. More sensitive operations still require explicit scopes.
- `flint reauth` without `--scope` retains prior permissions for each existing context unless you remove them in the browser. Servers without this support return `REAUTH_PRESERVE_UNAVAILABLE`.
- `flint signup` accepts `--business-name` and applies the supplied website and support details to a new merchant. Interrupted signup retains the verified merchant details; existing merchants keep their profile.
- Live-mode MCP guidance explains how to authorize and select a live login context.

## 0.20.2 (2026-11-16)

- `flint listen --forward-to URL` now creates or reuses a sandbox JWT endpoint and prints its stable ID for receiver verification. Existing endpoints passed with `--webhook-endpoint-id` keep their configuration and subscriptions. Event filters follow those subscriptions, and individual signing refusals produce `skipped` records without ending the session or counting toward `--max-events`.
- Calendar filters use the merchant's `timezone` from `GET /v1/merchant`.
- Piped or redirected downloads write raw file bytes in human output. Delivery-method validation reports the selected request variant.
- Order summaries include the receipt number, payment status, total, and buyer, including buyer order lists.
- Promotion attribute discovery includes the complete attribute, type, and operator matrix. CLI and MCP dry runs reject unknown attributes while preserving metadata keys.
