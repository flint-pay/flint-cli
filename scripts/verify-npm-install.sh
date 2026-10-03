#!/usr/bin/env bash
set -euo pipefail

: "${CLI_VERSION:?Set CLI_VERSION to the published version}"
: "${NPM_TAG:?Set NPM_TAG to latest or next}"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
verification_dir="$(mktemp -d)"
trap 'rm -rf "$verification_dir"' EXIT

# Download every platform, including architectures these runners cannot execute.
# npm pack checks registry integrity and reports the actual archive contents.
verify_platform_packages() {
  local prefix="$1" target binary
  mkdir -p "$prefix/packs" || return 1
  for target in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64 windows-amd64; do
    binary=bin/flint
    if [ "$target" = windows-amd64 ]; then
      binary=bin/flint.exe
    fi
    if ! npm pack "@flintpay/cli-$target@$CLI_VERSION" \
      --ignore-scripts --json --pack-destination "$prefix/packs" \
      --cache "$prefix/cache" --prefer-online | \
      node "$script_dir/verify-npm-package.mjs" "@flintpay/cli-$target" "$CLI_VERSION" "$binary"; then
      return 1
    fi
  done
}

# npm may succeed while omitting an optional platform package that has not
# propagated yet. Retry archive downloads, execution, and the channel pointer
# with a clean npm cache so prior runs cannot mask unavailable public artifacts.
for attempt in 1 2 3 4 5 6; do
  prefix="$verification_dir/attempt-$attempt"
  if verify_platform_packages "$prefix" &&
    npm install --prefix "$prefix" --cache "$prefix/cache" --prefer-online \
      --include=optional --no-audit --no-fund "@flintpay/cli@$CLI_VERSION" &&
    "$prefix/node_modules/.bin/flint" version --output json |
      jq -e --arg version "$CLI_VERSION" '.data.cli_version == $version' &&
    test "$(npm view "@flintpay/cli@$NPM_TAG" version --cache "$prefix/cache" --prefer-online)" = "$CLI_VERSION"; then
    exit 0
  fi
  if [ "$attempt" -lt 6 ]; then
    sleep "${FLINT_NPM_RETRY_DELAY:-20}"
  fi
done
echo "Published npm installation could not be verified after six attempts." >&2
exit 1
