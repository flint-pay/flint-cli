import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import { resolve } from "node:path";
import { singlePackResult } from "./npm-pack-result.mjs";
import { verifyPlatformPack } from "./verify-npm-package.mjs";
import { releaseVersion } from "./release-version.mjs";

const version = releaseVersion(`cli/v${process.env.FLINT_CLI_VERSION}`);
const destination = resolve("dist/npm-packages");
mkdirSync(destination, { recursive: true });
for (const target of ["darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64", "windows-amd64", "wrapper"]) {
  const json = execFileSync("npm", ["pack", resolve("dist/npm", target), "--ignore-scripts", "--json", "--pack-destination", destination], { encoding: "utf8" });
  const name = target === "wrapper" ? "@flintpay/cli" : `@flintpay/cli-${target}`;
  const result = target === "wrapper" ? singlePackResult(json)
    : verifyPlatformPack(json, name, version, target === "windows-amd64" ? "bin/flint.exe" : "bin/flint");
  assert.equal(result.name, name);
  assert.equal(result.version, version);
  assert.equal(result.filename, `${name.slice(1).replace("/", "-")}-${version}.tgz`);
}
