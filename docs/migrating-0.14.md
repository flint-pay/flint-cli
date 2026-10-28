# Migrating to Flint CLI 0.14.1

This release adds API key access flags. Existing commands and `--input` bodies keep working. 0.14.0 was not released: its npm wrapper package never published, so install 0.14.1.

## API key access flags

Flint now requires `access` whenever a request sends `scopes`, so every CLI key command sends `access`.

- `api-keys create`, `api-keys update`, `sandboxes api-keys create`, and `sandboxes create` accept `--access full|read_only|restricted` and repeatable `--scope` flags. `--scope` without `--access` selects `restricted`, as the Dashboard does; `--scope` with `full` or `read_only` is rejected.
- `api-keys create` no longer requires `--access` when you pass `--scope`. Creating a key needs an access choice: `--access`, or `--scope` for restricted.
- `sandboxes api-keys create` also takes `--name`. `sandboxes create` issues a test key when you pass `--access`, `--scope`, or `--test-key-name`, sending `issue_test_key: true`.
- `api-keys update` is a curated command with `--name` and `--expected-version`. Changing `access` or `scopes` requires `expected_version`.
- `--input` bodies keep their own fields: a body with `scopes` must also set `access`, and the CLI rejects it locally otherwise. MCP tools follow the same request schema.

```sh
flint api-keys create --name local-dev --scope payments.payment_intents.read
flint api-keys update key_123 --scope customers.read --expected-version 3
flint sandboxes test-key test_123 --name qa --access read_only --live
flint sandboxes create --name qa-scenarios --scope customers.read --live
```
