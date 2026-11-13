# API snapshot migration (unreleased)

This change refreshes the embedded API snapshot to the Flint commit pinned in `internal/spec/provenance.json`. It has not been released. The next CLI release needs a minor version bump because generated commands and request schemas change.

## API keys and login

Mint sandbox keys with the single key route:

```sh
flint api-keys create --sandbox-id test_01KEZF7C00JVWYYN2SBWFFSA36 --name sandbox-dev --access restricted --scopes customers.read --live
```

Use a credential authorized to mint keys for that sandbox. `--live` acknowledges a live caller credential; the new key is still a test key because `sandbox_id` selects its sandbox. When the active credential already belongs to the target sandbox, omit `--live`.

`flint sandboxes api-keys create` and `flint onboarding api-key create` are removed. MCP callers use `api-keys.create` through `flint_write`, with `sandbox_id` in the input. The typed tool is `api_keys_create`. The retired onboarding and sandbox mint tools are no longer listed.

Login keeps its client-owned default scopes and sends the public `mode` field (`test` or `live`) for device authorization and reauthorization. Context lists prefer `mode` and still accept the older `environment` field. Onboarding-session and partner-app auth contexts may omit scopes.

## Other removed commands

The refreshed server snapshot also removes the commands below. Inspect current operations with `flint schema commands --output json` and their input with `flint schema input COMMAND` before updating scripts. MCP operation IDs use the same dotted names.

- `flint checkout-sessions delivery-quotes create`
- `flint checkout-sessions delivery-quotes get`
- `flint checkout-sessions delivery-selections create`
- `flint checkout-sessions delivery-selections current delete`
- `flint checkout-sessions delivery-selections current get`
- `flint checkout-sessions delivery-selections get`
- `flint checkout-sessions delivery-selections list`
- `flint fulfillments events create`
- `flint fulfillments shipments create`
- `flint inventory-counts transitions create`
- `flint inventory-reservations transitions create`
- `flint inventory-transfers transitions create`
- `flint invoices checkout-sessions create`
- `flint me invoices checkout-sessions create`
- `flint me return-resolutions checkout-sessions create`
- `flint orders delivery-selections current get`
- `flint orders fulfillments create`
- `flint organizations create`
- `flint organizations delete`
- `flint organizations get`
- `flint organizations list`
- `flint organizations memberships create`
- `flint organizations memberships delete`
- `flint organizations memberships list`
- `flint organizations update`
- `flint return-resolutions checkout-sessions create`
- `flint shipments packages create`

Use the current checkout session delivery commands for delivery choices. Organization administration has moved out of the merchant credential API.

The refreshed snapshot also updates request and response schemas. Run `flint schema input COMMAND` for current required fields, union variants, and allowed values. `coverage.json` records every current public operation and its CLI classification.

## Scope diagnostics

Local terminal scope refusals now use `INSUFFICIENT_SCOPE`, matching the API. The local error's `details` is an array. Each local scope detail has `reason: "scope_not_granted"`, `scope_requirement: {mode, scopes}`, and `missing_scopes`. Replace checks for `MISSING_REQUIRED_SCOPES`, `scope_mode`, and `required_scopes` with this representation. Client permission previews also publish `scope_requirement`.

`flint schema command COMMAND --output json` publishes the same `scope_requirement` before a call. `flint auth status` and `flint doctor` include `scope_access`, with scoped-command counts, blocked commands, and blocked resource groups. Each group includes its total scoped commands and the blocked subset, so a partially accessible group stays visible. These counts cover scoped commands accepted by the credential class. Credentials that do not expose scopes report `status: "unknown"`.

Scope remediation names the active credential override and preserves the selected profile. Browser sessions use context reauthorization when available; a fresh login requests the complete requirement. Workload tokens use workload grants and `FLINT_OIDC_SCOPE` narrowing. MCP continues to pass API errors through unchanged.
