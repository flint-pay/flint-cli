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

After installation, `flint upgrade` (or its alias `flint update`) checks for the latest stable release. On macOS and Linux it uses npm or Homebrew when they own the installation, or securely replaces a standalone binary after verifying the release checksum. On Windows it prints the exact npm or manual update step to run after closing the CLI. Progress appears on stderr, with an elapsed-time update every 10 seconds during longer steps. Use `--quiet` to hide it, or `--output json --progress json` for structured results and progress on separate streams.

Prebuilt archives for macOS, Linux, and Windows are attached to each `cli/v*` [release](https://github.com/flint-pay/flint-cli/releases) with a `checksums.txt`. [`scripts/install.sh`](scripts/install.sh) downloads and verifies the right one. The release workflow also publishes a container image to `ghcr.io/flint-pay/flint-cli`.

For manual installation on macOS or Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/flint-pay/flint-cli/main/scripts/install.sh -o /tmp/flint-install.sh
FLINT_INSTALL_DIR="$HOME/.local/bin" sh /tmp/flint-install.sh
"$HOME/.local/bin/flint" version
```

Add `$HOME/.local/bin` to your `PATH` if needed. Set `FLINT_CLI_VERSION=X.Y.Z` to install a specific version. On Windows, download the ZIP and `checksums.txt` from the same release, verify the ZIP with `Get-FileHash -Algorithm SHA256`, and extract `flint.exe` into a directory on `PATH`.

## Sign in (recommended)

For local development, start with `flint auth login`. It opens the Flint website, displays a confirmation code, and saves OAuth access and refresh tokens in your OS keychain. Use `--no-open` to open the printed link yourself, `--profile NAME` to use an existing profile, or `--live` to explicitly request production access. Login waits up to 10 minutes by default; `--timeout 5m` overrides that limit.

If browser login is unavailable, use `flint auth import` instead. Browser approval requires a person; automation should use an API key through `FLINT_API_KEY` or `flint auth import --stdin`.

Access tokens refresh automatically as needed. `flint auth logout --confirm` revokes the OAuth session before removing its local credentials. If the server does not confirm revocation, logout reports failure and retains the credentials for retry. Imported API-key logout still removes only the local key.

`flint login` is a shortcut for `flint auth login`. Related shortcuts: `flint logout` for `flint auth logout`, and `flint whoami` for `flint auth status`.

### Sessions and contexts

Approve access to selected merchants, sandboxes, and live contexts once in the browser, then select where commands run:

```sh
flint login
flint context list
flint context switch                        # interactive selector
flint context switch ctx_development        # select an authorized sandbox
flint context switch ctx_production --live   # explicitly select a live default
flint auth status --context ctx_development  # one command, without changing the default
flint reauth                                # change browser-approved access
```

`flint login` reuses a valid saved OAuth session and starts browser approval again when the server confirms that the session expired or was revoked. Temporary connection failures or loss of access to one context do not automatically replace the session. Use `flint login --new-session` to replace it after browser approval; the CLI then attempts to revoke the previous session. `flint logout --confirm` revokes the entire session across its contexts.

Manage and revoke sessions on the [CLI sessions page](https://app.withflintpay.com/developers/cli). Browser authorization sends your computer's hostname and operating system to help identify the session.

Context selection uses `--context ID`, then a project's `.flint/config.json` `context` field, then the profile default. For example, `{"context":"ctx_development"}` pins a project. Switching changes the profile default, not a project pin. Each running command retains its starting context. Access tokens remain restricted to one context and refresh automatically. LIVE contexts are labeled in the selector and command diagnostics; selected multi-context sessions do not need `--live` on every command, while sensitive/destructive operations still require confirmation.

Environment credentials (`FLINT_API_KEY`, `FLINT_ACCESS_TOKEN`, or `FLINT_CHECKOUT_SESSION_SECRET`) override configured browser-session contexts. An explicit `--context` cannot be combined with these overrides. Merchant and sandbox guards still apply.

If `flint context list` reports that contexts are unavailable, your existing login remains usable and keeps its live-mode safeguards. Run `flint reauth` later to authorize multiple contexts.

### API keys and automation

For manual setup, create a key in the [Flint dashboard](https://app.withflintpay.com/developers/api-keys) and run `flint auth import`. For CI, supply `FLINT_API_KEY` through your secret store and set `FLINT_NO_INPUT=1`. `flint signup` remains available for creating an account from the terminal.

## Use

```bash
flint auth login    # recommended: approve browser sign-in; credential saved in the OS keychain
flint doctor        # config, credential, connectivity, environment, merchant, scopes, version
flint checkout create --quick-pay-name T-shirt --amount 2500 --currency USD --open
flint listen --forward-to http://localhost:8080/webhooks/flint
```

`flint help` lists the starting points and `flint <command> --help` documents any single command. `flint schema commands --output json` returns the whole catalog as data.

`flint support open` opens the Flint Help composer; you review and post in the browser. Use `--private` for a private thread or `--no-open` to print the link only.

```bash
flint support open --title "Webhook delivery failed" --body "My endpoint returned 500." --area webhooks --private
```

The CLI includes `--title` and `--body` as URL parameters, preserving multiline text. Browser prefilling requires the Help web app to accept these parameters; its existing link schema needs a corresponding update.

Add `--ai-agent` to identify the reporter as an AI agent. It defaults to false and accepts `--ai-agent=false`. The CLI emits `ai_agent=true` in the link when enabled and a boolean `ai_agent` in JSON output. The Help web app must read this parameter too; it is self-reported metadata, not verified identity.

Live partner OAuth installs require `--mode live --live` and confirmation (`--confirm` for scripts), including when using `flint api get /v1/oauth/authorize`. Raw API pagination (`--all` or `--paginate`) accepts only read-only GET operations.

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

Sandbox and live requests both use `https://api.withflintpay.com`. Browser login binds the OAuth session to the approved environment; imported API keys retain their own environment. Imported API keys and legacy sessions require `--live` to acknowledge production access; multi-context sessions use the selected context. Set `FLINT_BASE_URL` before signing in to a development server, using a separate profile. Saved OAuth sessions stay bound to that server and reject a conflicting URL override.

`internal/spec/openapi.json` is the public API snapshot used to build the command catalog. When updating it, replace it with a reviewed public OpenAPI export, run `make coverage`, and run `make contract`. The coverage report checks that every public operation has a dedicated command or an explicit exclusion with a reason. Feedback commands are retired; use `flint support open` to start a thread on Flint Help.

Tests use local HTTP servers and synthetic credentials. They do not require an API key or write to a Flint account.

## Release

Follow [RELEASING.md](RELEASING.md) for one-time publishing setup, release PRs, tagging a reviewed commit on `main`, installation verification, and recovery from partial publication.

Push a `cli/vX.Y.Z` tag. [`.github/workflows/cli-release.yml`](.github/workflows/cli-release.yml) runs the tests and a sandbox listener check, builds every platform with GoReleaser, then publishes the GitHub release with build provenance, the npm platform packages plus the `@flintpay/cli` wrapper, the container image, and the Homebrew formula in `flint-pay/homebrew-tap`. A tag with a prerelease suffix publishes to the npm `next` tag and skips Homebrew.

GitHub and npm publication verify the contents of an existing release before accepting a rerun.

Before tagging a release, create `flint-pay/homebrew-tap` and configure `FLINT_TEST_API_KEY` in the `cli-release-sandbox` GitHub environment, npm trusted publishing for the six packages, and `HOMEBREW_TAP_TOKEN` for the tap. Use a disposable sandbox for the listener check, which creates a customer. Configure the environment to require release approval.

The npm trusted publisher must name the `npm` GitHub environment and the `cli-release.yml` workflow. Release installation tests run before publication; public npm, manual, and stable Homebrew installations are verified afterward.

## License

Apache-2.0. See [LICENSE](LICENSE).
