import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import test from "node:test";

test("retry successful npm installs with missing binaries, and stop after six failures", { skip: process.platform === "win32" }, () => {
  const temp = mkdtempSync(join(tmpdir(), "flint-npm-retry-"));
  try {
    const npm = join(temp, "npm");
    writeFileSync(npm, `#!/usr/bin/env node
const fs = require('node:fs');
const path = require('node:path');
const log = process.env.FLINT_TEST_LOG;
if (process.argv[2] === 'view') { console.log('0.1.0'); process.exit(0); }
const prefix = process.argv[process.argv.indexOf('--prefix') + 1];
fs.appendFileSync(log, prefix + '\\n');
const attempt = fs.readFileSync(log, 'utf8').trim().split('\\n').length;
if (attempt === 1 || process.env.FLINT_TEST_ALWAYS_FAIL) process.exit(0);
fs.mkdirSync(path.join(prefix, 'node_modules/.bin'), {recursive: true});
fs.writeFileSync(path.join(prefix, 'node_modules/.bin/flint'), ${JSON.stringify('#!/usr/bin/env node\nconsole.log(JSON.stringify({data:{cli_version:"0.1.0"}}));\n')}, {mode: 0o755});
`);
    chmodSync(npm, 0o755);
    for (const fail of [false, true]) {
      const log = join(temp, `attempts-${fail}`);
      const result = spawnSync("bash", ["scripts/verify-npm-install.sh"], {
        encoding: "utf8",
        env: { ...process.env, PATH: `${temp}${delimiter}${process.env.PATH}`, CLI_VERSION: "0.1.0", NPM_TAG: "latest", FLINT_NPM_RETRY_DELAY: "0", FLINT_TEST_LOG: log, FLINT_TEST_ALWAYS_FAIL: fail ? "1" : "" },
      });
      assert.equal(result.status, fail ? 1 : 0, result.stderr);
      const prefixes = readFileSync(log, "utf8").trim().split("\n");
      assert.equal(prefixes.length, fail ? 6 : 2);
      assert.equal(new Set(prefixes).size, prefixes.length, "Each retry must use a clean prefix");
    }
  } finally {
    rmSync(temp, { recursive: true, force: true });
  }
});
