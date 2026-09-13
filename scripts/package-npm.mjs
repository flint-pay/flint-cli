import { cpSync, chmodSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const cliDir = resolve(scriptDir, "..");
const distDir = resolve(cliDir, "dist");
const outputDir = resolve(distDir, "npm");
const version = process.env.FLINT_CLI_VERSION?.replace(/^v/, "");
if (!version) throw new Error("FLINT_CLI_VERSION is required");

const targets = [
  ["darwin", "arm64", "tar.gz"],
  ["darwin", "amd64", "tar.gz"],
  ["linux", "arm64", "tar.gz"],
  ["linux", "amd64", "tar.gz"],
  ["windows", "amd64", "zip"],
];

mkdirSync(outputDir, { recursive: true });
const wrapperDir = join(outputDir, "wrapper");
cpSync(resolve(cliDir, "npm"), wrapperDir, { recursive: true });
cpSync(resolve(cliDir, "LICENSE"), join(wrapperDir, "LICENSE"));
const wrapperManifestPath = join(wrapperDir, "package.json");
const wrapperManifest = JSON.parse(readFileSync(wrapperManifestPath, "utf8"));
wrapperManifest.version = version;
for (const dependency of Object.keys(wrapperManifest.optionalDependencies)) {
  wrapperManifest.optionalDependencies[dependency] = version;
}
writeFileSync(wrapperManifestPath, `${JSON.stringify(wrapperManifest, null, 2)}\n`);

for (const [os, arch, format] of targets) {
  const packageDir = join(outputDir, `${os}-${arch}`);
  const binDir = join(packageDir, "bin");
  mkdirSync(binDir, { recursive: true });
  const archive = join(distDir, `flint_${version}_${os}_${arch}.${format}`);
  if (format === "zip") {
    execFileSync("unzip", ["-q", archive, "-d", binDir]);
  } else {
    execFileSync("tar", ["-xzf", archive, "-C", binDir]);
  }
  cpSync(resolve(cliDir, "LICENSE"), join(packageDir, "LICENSE"));
  const binary = join(binDir, os === "windows" ? "flint.exe" : "flint");
  if (os !== "windows") chmodSync(binary, 0o755);
  const manifest = {
    name: `@flintpay/cli-${os}-${arch}`,
    version,
    description: `Flint CLI binary for ${os}-${arch}`,
    license: "Apache-2.0",
    repository: { type: "git", url: "https://github.com/flint-pay/flint-cli.git" },
    os: [os === "windows" ? "win32" : os],
    cpu: [arch === "amd64" ? "x64" : arch],
    files: [os === "windows" ? "bin/flint.exe" : "bin/flint"],
  };
  writeFileSync(join(packageDir, "package.json"), `${JSON.stringify(manifest, null, 2)}\n`);
}
