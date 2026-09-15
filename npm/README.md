# `@flintpay/cli`

Official command-line interface for Flint's public API.

Every command maps to a documented `/v1` route, so anything you can do over HTTP you can do from a shell, a script, or a CI job.

## Install

```bash
npm install -g @flintpay/cli
```

This package resolves a prebuilt binary for your platform. Node `>=18` is required to run the installer. On macOS and Linux you can install with Homebrew instead:

```bash
brew install flint-pay/tap/flint
```

## Sign in (recommended)

For local development, sign in through the Flint website:

```bash
flint auth login
flint doctor
```

Confirm the code shown in your terminal and approve CLI access on the website. Flint validates the OAuth session and stores its access and refresh tokens in your OS keychain. Use `--no-open` to open the printed link yourself, `--profile NAME` for an existing profile, or `--live` to explicitly request production access.

Browser login has been verified against Flint staging. To use staging, set `FLINT_BASE_URL=https://api.staging.withflintpay.com` for login and subsequent commands. Production rollout is separate; use `flint auth import` on servers without browser login support.

Access tokens refresh automatically as needed. `flint auth logout --confirm` revokes the OAuth session before removing its local credentials. If the server does not confirm revocation, logout reports failure and retains the credentials for retry. Imported API-key logout still removes only the local key.

`flint login` is a shortcut for `flint auth login`. Related shortcuts: `flint logout` for `flint auth logout`, and `flint whoami` for `flint auth status`.

### API keys and automation

For manual setup, create a key in the [Flint dashboard](https://app.withflintpay.com/developers/api-keys) and run `flint auth import`. For CI, supply `FLINT_API_KEY` through your secret store and set `FLINT_NO_INPUT=1`. `flint signup` remains available for creating an account from the terminal.

## Take a payment

```bash
flint checkout create \
  --quick-pay-name T-shirt \
  --amount 2500 \
  --currency USD \
  --open
```

Amounts are integers in the currency's smallest unit, so `2500` is $25.00.

## Forward webhooks to localhost

```bash
flint listen --forward-to http://localhost:8080/webhooks/flint
```

This streams your sandbox's events to a local server with a real signature, so you do not need a tunnel while developing.

## Scripting

For bounded commands, `--output json` prints one JSON envelope on stdout and disables terminal input prompts. `flint auth login` still requires approval on the website; use `FLINT_NO_INPUT=1` to prohibit browser login in automation. Streaming commands emit newline-delimited JSON instead. `flint listen --output json` writes one listener, webhook event, delivery result, or checkpoint object per line; use `--max-events` or `--for` to bound it. Diagnostics and progress go to stderr, so stdout stays a clean data stream.

Exit codes are stable: `0` success, `1` API error, `2` usage error, `3` auth or config, `4` confirmation required, `5` network, `6` `--wait-for` timed out, `70` internal error.

In CI, pass the key through `FLINT_API_KEY` instead of the keychain, and set `FLINT_OUTPUT=json` and `FLINT_NO_INPUT=1` to make every command non-interactive.

## Documentation

- Guide: https://developers.withflintpay.com/docs/guides/cli
- API reference: https://developers.withflintpay.com/docs/api
- `flint help` for starting points, `flint <command> --help` for any single command
- `flint schema commands --output json` for the complete machine-readable catalog

## License

Apache-2.0
