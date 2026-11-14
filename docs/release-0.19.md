# Flint CLI 0.19.0

## Changes

- Refresh the complete public API snapshot, adding eight operations without new coverage exclusions.
- Breaking: move buyer credit-note list, get, and PDF commands from `me invoices credit-notes` to `me credit-notes`. Preserve invoice-scoped listing with `--invoice-id`; remove the invoice ID from get and PDF calls.
- Breaking: replace `expected_version` with the parent-specific version field on return line items, credit-note allocations, invoice manual payments, subscription line items, and return-policy revisions.
- Breaking: limit subscription multipliers to 1 and plan `quantity_options` to an omitted field, `[]`, or `[1]`.
- Add customer email-change requests, invoice late-fee lists, remembered-buyer management, and subscription line-item lists.
- Add buyer next-renewal swap previews and allow buyer delivery-option previews without a destination.

## Installation and migration

```sh
npm install -g @flintpay/cli@latest
# Or:
brew upgrade flint
flint version --output json
```

Follow the [0.19.0 migration notes](migrating-0.19.md) for all three command migrations, request field replacements, subscription constraints, and MCP changes. Upgrades from 0.17.x or earlier should also follow the [0.18.0 migration notes](migrating-0.18.md).
