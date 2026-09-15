# Releasing the Flint CLI

The CLI follows the [Flint SDK release process](https://github.com/flint-pay/flint-sdks/blob/main/RELEASING.md): prepare a release PR, merge a reviewed commit to `main`, tag that commit, publish immutable versions, then verify installation from each public channel.

The CLI version is independent of the API version and SDK package versions. One `cli/vX.Y.Z` tag supplies the version for the binary, npm packages, Homebrew formula, and container. The development version in `npm/package.json` is replaced during packaging; do not bump it manually. Stable versions use npm `latest`; prereleases such as `cli/v0.1.0-beta.1` use `next` and do not update Homebrew or the container's `latest` tag. Release tags do not accept SemVer build metadata (`+build`).

## One-time setup

1. Keep `flint-pay/flint-cli` public. The manual installer and Homebrew download its GitHub Release assets anonymously; npm provenance also requires a public source repository. Merge `.github/workflows/cli-release.yml` before configuring publication.
2. Create the public `flint-pay/homebrew-tap` repository with an initial README commit. The release workflow writes `Formula/flint.rb` to its default branch. Add `HOMEBREW_TAP_TOKEN` as a secret in the CLI repository: use a fine-grained GitHub token restricted to the tap with Contents read/write permission, authorized for the organization. The CLI repository's default `GITHUB_TOKEN` cannot write to another repository.
3. Create the GitHub environment `cli-release-sandbox`, require a maintainer's approval, and allow the `cli/v*` release tags and the `main` branch. Add the environment secret `FLINT_TEST_API_KEY` for a disposable sandbox. The listener check creates a customer and validates the forwarded webhook signature; it must not use a live key.
4. Create the GitHub environment `npm`, using the same naming convention as the SDK repository. Restrict it to release tags and apply the desired maintainer approval rules. Configure a GitHub Actions trusted publisher on **each** of these npm packages:

   - `@flintpay/cli`
   - `@flintpay/cli-darwin-arm64`
   - `@flintpay/cli-darwin-amd64`
   - `@flintpay/cli-linux-arm64`
   - `@flintpay/cli-linux-amd64`
   - `@flintpay/cli-windows-amd64`

   Use organization `flint-pay`, repository `flint-cli`, workflow filename **`cli-release.yml`** (not its path), and environment **`npm`**. Allow direct `npm publish`. The npm scope is `flintpay`; the GitHub organization is `flint-pay`. The workflow uses GitHub-hosted runners and `id-token: write`, with a pinned npm version supporting OIDC. No permanent npm token is needed in GitHub.
5. If the npm packages do not exist yet, bootstrap them from a reviewed prerelease using an authenticated maintainer npm session, then configure trusted publishing in their package settings. Follow the preparation steps below, run `npm login`, publish the five prepared platform directories first, and publish the wrapper last with `npm publish <directory> --access public --tag next`. Do not publish empty placeholder packages. Local publication does not use GitHub OIDC; do not add `--provenance` locally. Reserve that prerelease version, keep its source tag and artifacts, and use a new version for the first full workflow release. Package settings may require interactive two-factor authentication.
6. Protect `main` with required CI/review and protect `cli/v*` tags against updates/deletion. The workflow checks main ancestry, but repository rules establish who may approve and tag a release. The existing container channel also requires GitHub Actions package write access; make the GHCR package public if it is intended for anonymous installation.

References: [npm trusted publishing](https://docs.npmjs.com/trusted-publishers/), [GitHub environments](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/manage-environments), and [Homebrew taps](https://docs.brew.sh/How-to-Create-and-Maintain-a-Tap).

## Prepare a release PR

1. Choose a new, unused version. Use patch releases for compatible fixes, minor releases for compatible additions, and the appropriate breaking-change bump. Before 1.0, review breaking changes explicitly and bump the minor version. Prerelease changes need a new prerelease counter.
2. Include the user-facing changes, installation documentation, and migration notes in the PR. Update `internal/spec/openapi.json` and regenerate `coverage.json` only if the API snapshot changed.
3. Run the checks:

   ```sh
   make contract
   go vet ./...
   node --test scripts/*.test.mjs
   ```

4. Prepare packages locally when changing distribution code. Use Go 1.25.14, GoReleaser 2.17.0, and Node/npm compatible with the release workflow. `--clean` replaces the ignored `dist/` directory; preserve an earlier preparation if needed for comparison.

   ```sh
   export CLI_VERSION=0.1.0-beta.1 # replace with the version under review
   export FLINT_CLI_VERSION="$CLI_VERSION"
   export BUILD_DATE="$(git show -s --format=%cI HEAD)"
   export FLINT_SCHEMA_HASH="$(shasum -a 256 internal/spec/openapi.json | awk '{print $1}')"
   goreleaser release --snapshot --clean
   node scripts/test-install.mjs
   # On a Mac without flint installed:
   bash scripts/test-homebrew.sh
   ```

   This builds but does not publish. The installation test checks the native archive checksum and version, installs actual npm tarballs into a clean consumer without registry access, and tests the Unix installer with local download fixtures, including rejection of a bad checksum. The Homebrew test creates a temporary tap and installs the generated formula using local archives, then removes the installation and tap.
PR and main-branch CI also build snapshot archives and run the same macOS, Linux, Windows, and Homebrew installation checks used by the release workflow; no sandbox key or publication is needed.

5. Review and merge after CI passes. Tag the exact reviewed commit on `main`, never the current feature branch by accident.

## Publish the reviewed commit

Run **CLI Sandbox Check** from `main` and approve the sandbox environment before tagging. This checks the configured credential and webhook forwarding without publishing. Failures report CLI error codes without exposing the signing secret.

Check that the version is unused on GitHub and all six npm packages. A registry authentication or network error does not establish that a version is available.

```sh
git fetch origin main --tags
git log -1 --oneline origin/main
# Replace this example with the approved, unused version.
git tag -a cli/v0.1.0-beta.1 origin/main -m 'Flint CLI 0.1.0-beta.1'
git push origin cli/v0.1.0-beta.1
gh run list --workflow cli-release.yml --limit 5
```

Approve the sandbox and npm environment deployments when prompted by GitHub. The workflow validates the tag and main ancestry, runs CI and the sandbox check, builds archives once, and runs installation tests on macOS, Linux, and Windows before any publication. Homebrew is tested on macOS using the generated formula. npm publishes the platform packages before the wrapper. Stable releases update the public tap after the other publishing jobs succeed.

GitHub Releases retain the binary archives and `checksums.txt`; the workflow also creates build provenance attestations. npm packages embed the same binaries. The container is built separately from the same commit and build metadata.

## Verify publication

The `verify-published` job installs the exact npm version and checks its `latest`/`next` tag, installs from the public GitHub downloads on macOS and Linux, and installs/tests the public Homebrew formula on macOS for stable releases. A publishing job succeeding is not sufficient: require the verification job to pass too.

For a manual check, replace `0.1.0-beta.1` below with the released version:

```sh
gh release view cli/v0.1.0-beta.1 --repo flint-pay/flint-cli
npm view @flintpay/cli@0.1.0-beta.1 version dist.integrity
npm view @flintpay/cli dist-tags --json
npm install -g @flintpay/cli@0.1.0-beta.1
flint version --output json
```

Stable installations use `npm install -g @flintpay/cli` or `brew install flint-pay/tap/flint`; beta installations use `npm install -g @flintpay/cli@next`. Homebrew upgrades use `brew update && brew upgrade flint-pay/tap/flint`.

For manual installation on macOS or Linux, download the installer from the release's source tag and select the same version:

```sh
curl -fsSL https://raw.githubusercontent.com/flint-pay/flint-cli/refs/tags/cli/v0.1.0-beta.1/scripts/install.sh -o /tmp/flint-install.sh
FLINT_CLI_VERSION=0.1.0-beta.1 FLINT_INSTALL_DIR="$HOME/.local/bin" sh /tmp/flint-install.sh
"$HOME/.local/bin/flint" version --output json
```

Add `$HOME/.local/bin` to your shell's `PATH` if needed. Omitting `FLINT_CLI_VERSION` selects the latest stable release. The installer defaults to `/usr/local/bin` if `FLINT_INSTALL_DIR` is omitted; choose a writable directory for installation without sudo.

For Windows, download `flint_VERSION_windows_amd64.zip` and `checksums.txt` from the same GitHub Release. Compare `Get-FileHash .\flint_VERSION_windows_amd64.zip -Algorithm SHA256` with its entry in `checksums.txt`, extract `flint.exe` into a directory on `PATH`, and run `flint version --output json`. macOS/Linux users can likewise verify and extract the relevant `.tar.gz` manually.

## Partial publication and retries

Channels publish independently. Check the actual GitHub release, npm versions/dist-tags, tap commit, and container tags before retrying. Never move a published source tag, overwrite release assets, or publish changed contents under an existing version. Fix code with a new version.

For a transient failure, rerun the failed jobs of the original workflow run. GitHub and npm publication verify existing content before accepting a rerun. The workflow pins build tools and derives build metadata from the source commit. If a rebuilt artifact differs, stop and investigate; do not bypass the integrity check. A partially uploaded GitHub Release may require carefully adding only the missing original artifacts before rerunning.

npm can take several minutes to expose accepted versions. Verification retries installation, CLI execution, and the npm dist-tag check in a fresh directory each time; if it still fails, wait and rerun only the failed verification job. Do not bump or republish solely to work around registry propagation. When retrying an older release after a newer release has shipped, inspect the channel pointers first: publishing an older stable version can move `latest` or the Homebrew formula backward. The automatic dist-tag verification deliberately fails if the expected pointer does not match.

For npm OIDC failures, check the exact repository/workflow/environment fields, direct publishing permission, the public repository URL in the package, and `id-token: write`. `npm whoami` does not test OIDC. For tap failures, check token expiry, organization authorization, Contents write access, and the tap's branch rules. Do not paste credentials into issues, PRs, or release notes.

## Explicit sandbox waiver

For an approved release during a known sandbox API outage, set repository variables `CLI_SANDBOX_WAIVER_TAG` to the exact release tag and `CLI_SANDBOX_WAIVER_REASON` to a nonempty public explanation. The sandbox environment still requires approval. Only that tag skips the live sandbox check; CI, security, installation, and public-channel verification remain required. The reason appears in the job summary and GitHub Release notes. Remove both variables after the release. The standalone CLI Sandbox Check never uses this waiver.
