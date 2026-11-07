# Flint CLI 0.17.0

## Changes

- Refresh the embedded API snapshot from Flint `c416007c02`. It adds 49 commands: customer tax exemptions, order edits and previews, order receipt PDFs, price lists and their prices, subscription quantity and plan changes, tickets, and one command per fulfillment and package action.
- Replace `fulfillments transitions create` and `packages transitions create` with one command per action, such as `flint fulfillments mark-ready FUL` and `flint packages mark-shipped PKG`.
- Replace `credit-notes refunds create` and `credit-notes refunds list` with `flint refunds create --refund-type credit_note --credit-note-id CN` and `flint refunds list --credit-note-id CN`.
- Rename `flint orders inventory-exception resolve` to `flint orders resolve-inventory-exception`.
- `flint refunds create` calls that omit `--refund-type` and pass only shared target flags, such as `--payment-intent-id`, now report `AMBIGUOUS_VARIANT` with the candidate types. Set `--refund-type` explicitly.
- New destructive commands ask for confirmation by environment and carry the MCP `destructiveHint`: `customers tax-exemptions delete`, `fulfillments accept`, `fulfillments cancel`, `me subscriptions change-plan`, `orders edits create`, `orders edits retry`, `price-lists delete`, `price-lists prices delete`, and `subscriptions change-quantity`.
- The typed MCP tool list grows to 591 tools. Removed and renamed commands no longer resolve as MCP operations.

## Installation and migration

This release removes four commands and renames one, so scripts and MCP clients that call them must switch. Follow the [0.17.0 migration notes](migrating-0.17.md) for each replacement.

```sh
npm install -g @flintpay/cli@latest
# Or:
brew upgrade flint
flint version --output json
```

Upgrades from 0.15.x or earlier should also follow the [0.15.0 migration notes](migrating-0.15.md). The embedded API snapshot is larger because Flint now commits its export pretty-printed, so each binary grows by about 12 MB.
