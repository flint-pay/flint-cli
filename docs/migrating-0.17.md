# Migrating to Flint CLI 0.17.0

0.17.0 refreshes the embedded API snapshot from Flint `c416007c024c6cb96035a5c3e8f9cf173dd085ee`. The API replaced the fulfillment and package `/transitions` routes with one route per action, folded credit note refunds into `refunds create`, and renamed the order inventory exception route. Four commands are removed and one is renamed. The snapshot also adds 49 commands. Commands that did not change keep their names and flags.

Command names are generated from routes, so MCP operation IDs change with them: an MCP operation ID is the command name joined with dots.

## Upgrading from 0.16.x

```sh
flint upgrade --check
flint upgrade
```

Or use the package manager that installed the CLI:

```sh
npm install -g @flintpay/cli@latest
brew upgrade flint
```

Check the result with `flint version --output json`.

## Removed commands

| 0.16.x command | Use in 0.17.0 |
| --- | --- |
| `flint fulfillments transitions create FUL --input '{"action":"mark_ready"}'` | `flint fulfillments mark-ready FUL` |
| `flint packages transitions create PKG --input '{"action":"mark_shipped",...}'` | `flint packages mark-shipped PKG ...` |
| `flint credit-notes refunds create CN ...` | `flint refunds create --refund-type credit_note --credit-note-id CN` |
| `flint credit-notes refunds list CN` | `flint refunds list --credit-note-id CN` |

Each former `action` value is now its own command, with the underscores written as hyphens. The fields that belonged to that action are flags on the new command, and `--input` still accepts the JSON body without the `action` field.

Fulfillment commands: `accept`, `cancel`, `complete`, `dispatch`, `fail`, `hold`, `mark-no-show`, `mark-packed`, `mark-picked`, `mark-preparing`, `mark-ready`, `release-hold`, `schedule`, and `start`. Run them as `flint fulfillments ACTION FUL`.

Package commands: `mark-delivered`, `mark-delivery-attempted`, `mark-exception`, `mark-in-transit`, `mark-out-for-delivery`, `mark-packed`, `mark-returned`, and `mark-shipped`. Run them as `flint packages ACTION PKG`.

`flint refunds create` has a new `credit_note` refund type. Pass `--credit-note-id`, and optionally `--expected-credit-note-version`. Because `--payment-intent-id`, `--order-id`, and the other target flags now belong to several refund types, a call that omits `--refund-type` and uses only those flags fails with `AMBIGUOUS_VARIANT` and lists the candidates. Set `--refund-type` explicitly. Refunds are irreversible, so `refunds create` still asks for confirmation.

## Renamed commands

| 0.16.x command | 0.17.0 command |
| --- | --- |
| `flint orders inventory-exception resolve ORD` | `flint orders resolve-inventory-exception ORD` |

The arguments and body are unchanged.

## Added commands

- Customer tax exemptions: `customers tax-exemptions create`, `delete`, `get`, `list`, and `update`.
- Order edits: `order-edit-previews create`, `orders edits create`, `orders edits get`, `orders edits list`, and `orders edits retry`.
- Receipts: `orders receipt-pdf get` and `me orders receipt-pdf get`.
- Price lists: `price-lists create`, `delete`, `get`, `list`, `update`, and `price-lists prices create`, `delete`, `get`, `list`, `update`.
- Subscriptions: `subscriptions change-quantity` and `me subscriptions change-plan`.
- Tickets: `tickets get`, `tickets list`, and `tickets lookup`.
- Fulfillment and package action commands listed above.

## Confirmation and MCP hints

Commands that follow `x-flint-destructive` are irreversible and ask for confirmation by environment. These new commands are marked destructive: `customers tax-exemptions delete`, `fulfillments accept`, `fulfillments cancel`, `me subscriptions change-plan`, `orders edits create`, `orders edits retry`, `price-lists delete`, `price-lists prices delete`, and `subscriptions change-quantity`. Their MCP tools carry `destructiveHint: true`. No existing command changed its destructive flag. `partner-apps update` is now marked sensitive.

## MCP

The MCP server still exposes its five discovery tools. Operation IDs for the removed and renamed commands above no longer resolve; use the replacement command names. The typed tool list is larger (591 tools).
