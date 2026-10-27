# Migrating to Flint CLI 0.12.0

This is a breaking change from CLI 0.11.0: commands for routes the public API no longer serves are removed. Before 1.0, breaking changes bump the minor version.

## Commands and MCP tools

Command names below use dots; invoke `payment-intents.capture` as `flint payment-intents capture`. MCP tool names replace every dot with an underscore, such as `payment_intents_capture`.

| 0.11.0 | 0.12.0 |
| --- | --- |
| `orders.payment-intents.capture`, `orders.payment-intents.cancel` | `payment-intents.capture`, `payment-intents.cancel`, which take only the payment intent ID |
