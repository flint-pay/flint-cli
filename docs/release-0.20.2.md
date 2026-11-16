# Flint CLI 0.20.2

0.20.2 fixes webhook forwarding, calendar filters, downloads, order summaries, and promotion attribute discovery. It keeps the 0.20 API snapshot and command contract. These compatible fixes use a patch release under the [release versioning rules](../RELEASING.md#prepare-a-release-pr).

## Changes

- Run `flint listen --forward-to http://localhost:3000/webhooks/flint` with a sandbox credential to create or reuse the CLI's JWT endpoint. Use the printed endpoint ID as `FLINT_WEBHOOK_ENDPOINT_ID` in your receiver. Later runs reuse the same ID.
- Pass `--webhook-endpoint-id` to test an existing endpoint's subscriptions. The listener forwards only its enabled event types. Individual signing refusals produce `skipped` records and do not count toward `--max-events`.
- Calendar filters use the merchant's `timezone` from `GET /v1/merchant`.
- Piped or redirected downloads write raw file bytes in human output. Delivery-method validation reports the selected request variant.
- Order summaries include the receipt number, payment status, total, and buyer, including buyer order lists.
- Promotion attribute discovery includes the complete attribute, type, and operator matrix. CLI and MCP dry runs reject unknown attributes while preserving metadata keys.

## Installation

```sh
npm install -g @flintpay/cli@0.20.2
# Or:
brew upgrade flint
flint version --output json
```

For a standalone installation, select `FLINT_CLI_VERSION=0.20.2` with the [manual installer](../README.md#install). Existing 0.14.x installations can use `flint upgrade`; the legacy version check has been supported since 0.20.1.

Scripts using the 0.20.0 contract need no migration. When upgrading from an earlier minor version, follow the [0.20.0 migration notes](migrating-0.20.md).
