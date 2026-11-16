# Flint CLI 0.20.3

0.20.3 fixes default login permissions, reauthorization, and the merchant profile created during signup. It keeps the 0.20 API snapshot and command contract. These compatible fixes use a patch release under the [release versioning rules](../RELEASING.md#prepare-a-release-pr).

## Changes

- `flint login` requests permissions for shop operations and financial reads, including catalog, inventory, promotions, gift cards, settings, and reports. More sensitive operations still require explicit scopes and identity verification where required. Review the requested permissions in the browser before approving.
- `flint reauth` without `--scope` retains prior permissions for each existing merchant, environment, and sandbox context. You can deliberately remove permissions in the browser. Adding a context does not copy another context's prior permissions.
- Servers without permission-preserving reauthorization return `REAUTH_PRESERVE_UNAVAILABLE`. Update the server or use `flint reauth --scope SCOPE` to add explicit permissions. The CLI does not fall back to replacing your prior grants.
- `flint signup --business-name "Ada Labs"` sets the name buyers see at checkout, on receipts, and in gift card emails for a new merchant. Supplied website and support flags also set its profile. Existing merchants keep their profile, and the result reports the actual details with a warning when a requested value was not applied. Explicit profile fields require the matching server API.
- Interrupted signup retains the verified merchant details. Changed create-time profile flags are rejected when resuming verification.
- Live-mode MCP guidance explains live login and context selection.

## Installation

```sh
npm install -g @flintpay/cli@0.20.3
# Or:
brew upgrade flint
flint version --output json
```

For a standalone installation, select `FLINT_CLI_VERSION=0.20.3` with the [manual installer](../README.md#install). Existing 0.14.x installations can use `flint upgrade`; the legacy version check has been supported since 0.20.1.

Scripts using the 0.20.0 contract need no migration. When upgrading from an earlier minor version, follow the [0.20.0 migration notes](migrating-0.20.md).
