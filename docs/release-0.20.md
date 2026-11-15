# Flint CLI 0.20.0

## Changes

- Breaking: remove subscription `quantity` multipliers and plan `quantity_options`, including their flags, request fields, and response fields. Checkout subscription terms and payment-link previews also omit the multiplier fields. This implements the accepted `subscription-seat-line-quantity` API break.
- Add repeatable `--line-item sli_ID=QUANTITY` flags to merchant and buyer `subscriptions change-quantity`. Requests require 1 to 50 distinct existing subscription lines. Each quantity is a whole number from 1 to 9,999; omitted lines keep their quantities.
- Breaking: remove direct subscription line-item `--quantity` updates. Use `subscriptions change-quantity` to change existing line quantities with an explicit billing timing.
- Add per-line signup quantities to subscription creation, checkout creation, and payment-link resolution through JSON input.
- Breaking: payment confirmation defaults `client_action_type` to `url`; request `stripe` explicitly when your integration needs a Stripe client secret.
- Refresh the complete public API snapshot pinned in `internal/spec/provenance.json`, including buyer email-change request list/get commands, payment return recovery and checkout confirmation support, past-due billing and payout-hold facts, and customer access-link delivery and expiry options.
- Breaking: remove merchant Return `buyer_actions` and buyer Return, line-item, and resolution `supported_actions`. Use the current buyer task and lifecycle fields.

## Installation and migration

```sh
npm install -g @flintpay/cli@latest
# Or:
brew upgrade flint
flint version --output json
```

Follow the [0.20.0 migration notes](migrating-0.20.md) before updating scripts and MCP calls.
