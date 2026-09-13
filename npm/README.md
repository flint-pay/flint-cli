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

## Authenticate

Create a sandbox key in the [Flint dashboard](https://app.withflintpay.com/developers/api-keys), then import it:

```bash
flint auth import
```

The key is validated against the API and stored in your OS keychain, not in a config file. If you do not have an account yet, `flint signup` completes machine-actionable onboarding steps and stores the initial sandbox key as soon as the API makes it available.

Confirm everything works:

```bash
flint doctor
```

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

For bounded commands, `--output json` prints one JSON envelope on stdout and disables every prompt. Streaming commands emit newline-delimited JSON instead. `flint listen --output json` writes one listener, webhook event, delivery result, or checkpoint object per line; use `--max-events` or `--for` to bound it. Diagnostics and progress go to stderr, so stdout stays a clean data stream.

Exit codes are stable: `0` success, `1` API error, `2` usage error, `3` auth or config, `4` confirmation required, `5` network, `6` `--wait-for` timed out, `70` internal error.

In CI, pass the key through `FLINT_API_KEY` instead of the keychain, and set `FLINT_OUTPUT=json` and `FLINT_NO_INPUT=1` to make every command non-interactive.

## Documentation

- Guide: https://developers.withflintpay.com/docs/guides/cli
- API reference: https://developers.withflintpay.com/docs/api
- `flint help` for starting points, `flint <command> --help` for any single command
- `flint schema commands --output json` for the complete machine-readable catalog

## License

Apache-2.0
