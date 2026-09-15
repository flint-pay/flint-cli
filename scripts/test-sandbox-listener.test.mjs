import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

test("sandbox failures expose error codes without leaking listener secrets", { skip: process.platform === "win32" }, () => {
  const dir = mkdtempSync(join(tmpdir(), "flint-sandbox-test-"));
  try {
    const binary = join(dir, "flint");
    writeFileSync(binary, `#!/usr/bin/env node
console.log(JSON.stringify({type:"listener",signing_secret:"never-print-this-secret"}));
console.log(JSON.stringify({error:{type:"authentication_error",code:"INVALID_API_KEY",message:"never-print-this-message",request_id:"req_test"}}));
process.exit(1);
`, {mode: 0o755});
    const result = spawnSync("bash", ["scripts/test-sandbox-listener.sh"], {
      encoding: "utf8", timeout: 10000,
      env: {...process.env, FLINT_API_KEY:"flint_test_fixture", FLINT_BIN:binary},
    });
    assert.equal(result.status, 1, result.stderr);
    assert.match(result.stderr, /INVALID_API_KEY/);
    assert.match(result.stderr, /req_test/);
    assert.doesNotMatch(result.stdout + result.stderr, /never-print/);
    const live = spawnSync("bash", ["scripts/test-sandbox-listener.sh"], {
      encoding: "utf8", env:{...process.env, FLINT_API_KEY:"flint_live_fixture", FLINT_BIN:binary},
    });
    assert.equal(live.status, 1);
    assert.match(live.stderr, /sandbox API key is required/);
  } finally { rmSync(dir, {recursive:true,force:true}); }
});
