# Changelog

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
