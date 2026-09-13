# Flint CLI

`flint` is the command-line client for Flint's public API. Every command maps to a documented `/v1` route, so anything you can do over HTTP you can do from a shell, a script, or a CI job.

- Guide: https://developers.withflintpay.com/docs/guides/cli
- Command reference: https://developers.withflintpay.com/docs/cli
- npm package source: [`npm/`](npm/)

## Install

```bash
npm install -g @flintpay/cli      # resolves a prebuilt binary for your platform
brew install flint-pay/tap/flint   # macOS and Linux
```

Prebuilt archives for macOS, Linux, and Windows are attached to each `cli/v*` [release](https://github.com/flint-pay/flint-cli/releases) with a `checksums.txt`. [`scripts/install.sh`](scripts/install.sh) downloads and verifies the right one. The release workflow also publishes a container image to `ghcr.io/flint-pay/flint-cli`.

## Use

```bash
flint auth import   # paste a sandbox key; stored in the OS keychain, never a config file
flint doctor        # config, credential, connectivity, environment, merchant, scopes, version
flint checkout create --quick-pay-name T-shirt --amount 2500 --currency USD --open
flint listen --forward-to http://localhost:8080/webhooks/flint
```

`flint help` lists the starting points and `flint <command> --help` documents any single command. `flint schema commands --output json` returns the whole catalog as data.

## Develop

```bash
make build      # bin/flint
make test       # go test ./...
make contract   # tests, then diff the API coverage report against coverage.json
make coverage   # regenerate coverage.json after intentionally changing the surface
```

Layout:

| Path | What it holds |
| --- | --- |
| `cmd/flint` | Entry point; version, commit, build date, and schema hash are injected at link time. |
| `cmd/flint-coverage` | Emits `coverage.json`: every public route and whether a first-class command covers it. |
| `internal/cli` | Command registry, parsing, transport, output, and the local commands. |
| `internal/spec` | The embedded public OpenAPI snapshot every command is built against. |
| `internal/contract` | Coverage report generation. |
| `e2e` | Tests that run the built binary against a local server. |

Test and live requests both use `https://api.withflintpay.com`. Your API key selects the environment. Set `FLINT_BASE_URL` explicitly when testing against a different server.

`internal/spec/openapi.json` is the public API snapshot used to build the command catalog. When updating it, replace it with a reviewed public OpenAPI export, run `make coverage`, and run `make contract`. The coverage report checks that every public operation has a dedicated command.

Tests use local HTTP servers and synthetic credentials. They do not require an API key or write to a Flint account.

## Release

Push a `cli/vX.Y.Z` tag. [`.github/workflows/cli-release.yml`](.github/workflows/cli-release.yml) runs the tests and a sandbox listener check, builds every platform with GoReleaser, then publishes the GitHub release with build provenance, the npm platform packages plus the `@flintpay/cli` wrapper, the container image, and the Homebrew formula in `flint-pay/homebrew-tap`. A tag with a prerelease suffix publishes to the npm `next` tag and skips Homebrew.

GitHub and npm publication verify the contents of an existing release before accepting a rerun.

Before tagging a release, create `flint-pay/homebrew-tap` and configure `FLINT_TEST_API_KEY` in the `cli-release-sandbox` GitHub environment, npm trusted publishing for the six packages, and `HOMEBREW_TAP_TOKEN` for the tap. Use a disposable sandbox for the listener check, which creates a customer. Configure the environment to require release approval.

## License

Apache-2.0. See [LICENSE](LICENSE).
