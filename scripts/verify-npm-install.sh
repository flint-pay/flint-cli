#!/usr/bin/env bash
set -euo pipefail

: "${CLI_VERSION:?Set CLI_VERSION to the published version}"
: "${NPM_TAG:?Set NPM_TAG to latest or next}"
verification_dir="$(mktemp -d)"
trap 'rm -rf "$verification_dir"' EXIT

# npm may succeed while omitting an optional platform package that has not
# propagated yet. Retry the actual CLI execution and channel pointer as well.
for attempt in 1 2 3 4 5 6; do
  prefix="$verification_dir/attempt-$attempt"
  if npm install --prefix "$prefix" --no-audit --no-fund "@flintpay/cli@$CLI_VERSION" &&
    "$prefix/node_modules/.bin/flint" version --output json |
      jq -e --arg version "$CLI_VERSION" '.data.cli_version == $version' &&
    test "$(npm view "@flintpay/cli@$NPM_TAG" version)" = "$CLI_VERSION"; then
    exit 0
  fi
  if [ "$attempt" -lt 6 ]; then
    sleep "${FLINT_NPM_RETRY_DELAY:-20}"
  fi
done
echo "Published npm installation could not be verified after six attempts." >&2
exit 1
