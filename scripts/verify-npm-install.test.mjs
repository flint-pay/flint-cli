import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import test from "node:test";

const platforms = ["darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64", "windows-amd64"];

for (const [scenario, attempts] of [
  ["missing-binary-once", 2],
  ["missing-platform-once", 2],
  ["wrong-tag-once", 2],
  ["missing-platform", 6],
  ["missing-binary", 6],
  ["prerelease", 1],
]) {
  test(`public npm readiness: ${scenario}`, { skip: process.platform === "win32" }, () => {
    const temp = mkdtempSync(join(tmpdir(), "flint-npm-retry-"));
    try {
      const npm = join(temp, "npm");
      writeFileSync(npm, `#!/usr/bin/env node
const fs = require('node:fs');
const path = require('node:path');
const args = process.argv.slice(2);
fs.appendFileSync(process.env.FLINT_TEST_LOG, JSON.stringify(args) + '\\n');
const cache = args[args.indexOf('--cache') + 1];
const attempt = Number(cache.match(/attempt-(\\d+)/)[1]);
const scenario = process.env.FLINT_TEST_SCENARIO;
const version = process.env.CLI_VERSION;
if (args[0] === 'pack') {
  const spec = args[1];
  const name = spec.slice(0, spec.lastIndexOf('@'));
  if (name.endsWith('windows-amd64') && (scenario === 'missing-platform' || scenario === 'missing-platform-once' && attempt === 1)) process.exit(1);
  const binary = name.endsWith('windows-amd64') ? 'bin/flint.exe' : 'bin/flint';
  console.log(JSON.stringify([{name,version,filename:'platform.tgz',integrity:'sha512-example',files:[{path:binary,size:123}]}]));
} else if (args[0] === 'view') {
  console.log(scenario === 'wrong-tag-once' && attempt === 1 ? '0.0.9' : version);
} else if (args[0] === 'install') {
  if (scenario === 'missing-binary' || scenario === 'missing-binary-once' && attempt === 1) process.exit(0);
  const prefix = args[args.indexOf('--prefix') + 1];
  fs.mkdirSync(path.join(prefix, 'node_modules/.bin'), {recursive: true});
  fs.writeFileSync(path.join(prefix, 'node_modules/.bin/flint'), '#!/usr/bin/env node\\nconsole.log(JSON.stringify({data:{cli_version:' + JSON.stringify(version) + '}}));\\n', {mode: 0o755});
} else process.exit(2);
`);
      chmodSync(npm, 0o755);
      const log = join(temp, "calls");
      const version = scenario === "prerelease" ? "0.1.0-beta.1" : "0.1.0";
      const tag = scenario === "prerelease" ? "next" : "latest";
      const fails = scenario === "missing-platform" || scenario === "missing-binary";
      const result = spawnSync("bash", ["scripts/verify-npm-install.sh"], {
        encoding: "utf8",
        env: { ...process.env, PATH: `${temp}${delimiter}${process.env.PATH}`, CLI_VERSION: version, NPM_TAG: tag, FLINT_NPM_RETRY_DELAY: "0", FLINT_TEST_LOG: log, FLINT_TEST_SCENARIO: scenario },
      });
      assert.equal(result.status, fails ? 1 : 0, result.stderr);
      const calls = readFileSync(log, "utf8").trim().split("\n").map(line => JSON.parse(line));
      const cacheFor = args => args[args.indexOf("--cache") + 1];
      const caches = new Set(calls.map(cacheFor));
      assert.equal(caches.size, attempts, "Each retry must use a clean cache");
      for (const args of calls) assert.ok(args.includes("--prefer-online"));
      for (const cache of caches) {
        const packs = calls.filter(args => args[0] === "pack" && cacheFor(args) === cache);
        assert.deepEqual(packs.map(args => args[1]), platforms.map(platform => `@flintpay/cli-${platform}@${version}`));
        for (const args of packs) assert.ok(args.includes("--ignore-scripts"));
      }
      for (const args of calls.filter(args => args[0] === "view")) {
        assert.equal(args[1], `@flintpay/cli@${tag}`);
      }
      if (scenario === "missing-platform") {
        assert.ok(!calls.some(args => args[0] === "install"), "Missing foreign-platform artifacts must block readiness");
      }
    } finally {
      rmSync(temp, { recursive: true, force: true });
    }
  });
}
