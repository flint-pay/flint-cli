#!/usr/bin/env bash
set -euo pipefail

: "${FLINT_CLI_VERSION:?Set FLINT_CLI_VERSION to the prepared release version}"
root="$(cd "$(dirname "$0")/.." && pwd)"
tap="flint-ci/release-$$"
formula="$tap/flint"
export HOMEBREW_NO_AUTO_UPDATE=1
export HOMEBREW_NO_INSTALL_CLEANUP=1
export HOMEBREW_DEVELOPER=1

# Use an isolated tap and refuse to replace a developer's installed CLI.
if brew list --formula flint >/dev/null 2>&1; then
  echo "Run the Homebrew smoke test on a machine without flint installed." >&2
  exit 1
fi
brew tap-new "$tap"
cleanup() {
  brew uninstall --force "$formula" >/dev/null 2>&1 || true
  brew untap "$tap" >/dev/null 2>&1 || true
}
trap cleanup EXIT
tap_dir="$(brew --repository "$tap")"
node "$root/scripts/render-homebrew-formula.mjs" \
  "$FLINT_CLI_VERSION" "cli/v$FLINT_CLI_VERSION" "$root/dist/checksums.txt" > "$tap_dir/Formula/flint.rb"
# Point the generated formula at the exact local artifacts before publication.
# Its platform selection, checksums, installation and test block stay intact.
node --input-type=module - "$tap_dir/Formula/flint.rb" "$root/dist" <<'JS'
import { readFileSync, writeFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
const [formula, dist] = process.argv.slice(2);
const contents = readFileSync(formula, 'utf8').replace(
  /https:\/\/github\.com\/flint-pay\/flint-cli\/releases\/download\/[^/]+\//g,
  pathToFileURL(dist + '/').href,
);
writeFileSync(formula, contents);
JS
brew install --formula "$formula"
brew test "$formula"
"$(brew --prefix "$formula")/bin/flint" version --output json | \
  node --input-type=module -e 'import assert from "node:assert/strict"; let input=""; for await (const chunk of process.stdin) input+=chunk; assert.equal(JSON.parse(input).data.cli_version, process.env.FLINT_CLI_VERSION);'
