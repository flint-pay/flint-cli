#!/usr/bin/env node

const { spawnSync } = require("node:child_process");
const path = require("node:path");

const platform = process.platform;
const arch = process.arch === "x64" ? "amd64" : process.arch;
const supported = new Set([
  "darwin-arm64",
  "darwin-amd64",
  "linux-arm64",
  "linux-amd64",
  "win32-amd64",
]);
const target = `${platform}-${arch}`;

if (!supported.has(target)) {
  process.stderr.write(`Flint CLI does not publish a binary for ${platform}/${process.arch}.\n`);
  process.exit(2);
}

const packagePlatform = platform === "win32" ? "windows" : platform;
const packageName = `@flintpay/cli-${packagePlatform}-${arch}`;
let packagePath;
try {
  packagePath = path.dirname(require.resolve(`${packageName}/package.json`));
} catch {
  process.stderr.write(`The optional platform package ${packageName} is missing. Reinstall @flintpay/cli without omitting optional dependencies.\n`);
  process.exit(3);
}

const binary = path.join(packagePath, "bin", platform === "win32" ? "flint.exe" : "flint");
const result = spawnSync(binary, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  process.stderr.write(`${result.error.message}\n`);
  process.exit(5);
}
process.exit(result.status === null ? 5 : result.status);
