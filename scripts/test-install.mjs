import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { releaseVersion } from "./release-version.mjs";
import { singlePackResult } from "./npm-pack-result.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const dist = join(root, "dist");
const version = releaseVersion(`cli/v${process.env.FLINT_CLI_VERSION}`);
const os = process.platform === "win32" ? "windows" : process.platform;
const arch = process.arch === "x64" ? "amd64" : process.arch;
const target = `${os}-${arch}`;
const binary = os === "windows" ? "flint.exe" : "flint";
const archive = `flint_${version}_${os}_${arch}.${os === "windows" ? "zip" : "tar.gz"}`;
const temp = mkdtempSync(join(tmpdir(), "flint-install-"));
const run = (command, args, options = {}) => execFileSync(command, args, { encoding: "utf8", ...options });
// Invoke npm's JS entrypoint directly so paths with spaces work on Windows.
const npmCLI = process.env.npm_execpath || (process.platform === "win32"
  ? join(resolve(process.execPath, ".."), "node_modules/npm/bin/npm-cli.js")
  : run("which", ["npm"]).trim());
const npm = (args, options = {}) => run(process.execPath, [npmCLI, ...args], options);
const checkVersion = (command, args = []) => {
  const result = JSON.parse(run(command, [...args, "version", "--output", "json"]));
  assert.equal(result.data.cli_version, version);
};

try {
  const checksums = readFileSync(join(dist, "checksums.txt"), "utf8");
  const expected = checksums.split("\n").map(line => line.trim().split(/\s+/)).find(parts => parts[1] === archive)?.[0];
  assert.ok(expected, `Missing checksum for ${archive}`);
  assert.equal(createHash("sha256").update(readFileSync(join(dist, archive))).digest("hex"), expected);
  const extracted = join(temp, "archive");
  mkdirSync(extracted);
  run("tar", ["-xf", join(dist, archive), "-C", extracted]);
  checkVersion(join(extracted, binary));

  // Install real tarballs in a clean consumer, without registry access or scripts.
  run(process.execPath, [join(root, "scripts/package-npm.mjs")]);
  const packs = join(temp, "packs");
  mkdirSync(packs);
  const tarballs = [target, "wrapper"].map(name => {
    const packed = singlePackResult(npm(["pack", join(dist, "npm", name), "--pack-destination", packs, "--json"]));
    return join(packs, packed.filename);
  });
  const consumer = join(temp, "consumer");
  mkdirSync(consumer);
  writeFileSync(join(consumer, "package.json"), '{"private":true}');
  npm(["install", "--offline", "--ignore-scripts", "--no-audit", "--no-fund", ...tarballs], { cwd: consumer });
  checkVersion(process.execPath, [join(consumer, "node_modules/@flintpay/cli/bin/flint.js")]);
  // Check that npm created the user-facing command as well.
  const shim = join(consumer, "node_modules/.bin", os === "windows" ? "flint.cmd" : "flint");
  if (os === "windows") {
    const result = spawnSync(`"${shim}" version --output json`, { shell: true, encoding: "utf8" });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(JSON.parse(result.stdout).data.cli_version, version);
  } else {
    checkVersion(shim);
  }

  if (os !== "windows") {
    // Intercept only the transport: the real installer still selects its target,
    // verifies SHA-256, extracts the archive, and installs the actual binary.
    const mockBin = join(temp, "mock-bin");
    mkdirSync(mockBin);
    const curl = join(mockBin, "curl");
    writeFileSync(curl, `#!/usr/bin/env node
const fs = require('node:fs');
const path = require('node:path');
const args = process.argv.slice(2);
const url = args.find(arg => arg.startsWith('https://'));
if (url === 'https://api.github.com/repos/flint-pay/flint-cli/releases?per_page=100') {
  process.stdout.write(JSON.stringify([{tag_name:'cli/v99.0.0-beta.1',prerelease:true},{tag_name:'cli/v${version}',prerelease:false}], null, 2));
} else {
  const prefix = 'https://github.com/flint-pay/flint-cli/releases/download/cli%2Fv${version}/';
  if (!url?.startsWith(prefix)) throw new Error('Unexpected download URL: ' + url);
  const name = url.slice(prefix.length);
  if (!['${archive}', 'checksums.txt'].includes(name)) throw new Error('Unexpected asset: ' + name);
  const destination = args[args.indexOf('-o') + 1];
  fs.copyFileSync(path.join(process.env.FLINT_TEST_DIST, name), destination);
  if (name === 'checksums.txt' && process.env.FLINT_TEST_BAD_CHECKSUM) {
    fs.writeFileSync(destination, '0'.repeat(64) + '  ${archive}\\n');
  }
}
`);
    chmodSync(curl, 0o755);
    const env = { ...process.env, PATH: `${mockBin}${delimiter}${process.env.PATH}`, FLINT_TEST_DIST: dist };
    for (const selected of [version, ...(version.includes("-") ? [] : ["latest"])]) {
      const installDir = join(temp, `installed-${selected}`);
      run("sh", [join(root, "scripts/install.sh")], { env: { ...env, FLINT_CLI_VERSION: selected, FLINT_INSTALL_DIR: installDir } });
      checkVersion(join(installDir, binary));
    }
    const rejected = spawnSync("sh", [join(root, "scripts/install.sh")], {
      encoding: "utf8",
      env: { ...env, FLINT_CLI_VERSION: version, FLINT_INSTALL_DIR: join(temp, "rejected"), FLINT_TEST_BAD_CHECKSUM: "1" },
    });
    assert.equal(rejected.status, 5, rejected.stderr);
    assert.match(rejected.stderr, /Checksum verification failed/);
  }
  console.log(`Archive, npm, and supported installer checks passed for ${target} ${version}`);
} finally {
  rmSync(temp, { recursive: true, force: true });
}
