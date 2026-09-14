import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";

function within(promise, milliseconds = 5000) {
  let timer;
  return Promise.race([
    promise,
    new Promise((_, reject) => { timer = setTimeout(() => reject(new Error("Wrapper process timed out")), milliseconds); }),
  ]).finally(() => clearTimeout(timer));
}

function wrapperFixture(t, script, { missingBinary = false } = {}) {
  const temp = mkdtempSync(join(tmpdir(), "flint-wrapper-"));
  const wrapper = join(temp, "node_modules", "@flintpay", "cli", "bin", "flint.js");
  mkdirSync(dirname(wrapper), { recursive: true });
  copyFileSync(new URL("../npm/bin/flint.js", import.meta.url), wrapper);
  const platform = process.platform === "win32" ? "windows" : process.platform;
  const arch = process.arch === "x64" ? "amd64" : process.arch;
  const pkg = join(temp, "node_modules", "@flintpay", `cli-${platform}-${arch}`);
  mkdirSync(join(pkg, "bin"), { recursive: true });
  writeFileSync(join(pkg, "package.json"), JSON.stringify({ name: `@flintpay/cli-${platform}-${arch}` }));
  const binary = join(pkg, "bin", platform === "windows" ? "flint.exe" : "flint");
  if (!missingBinary) {
    if (platform === "windows") copyFileSync(process.execPath, binary);
    else symlinkSync(process.execPath, binary);
  }
  // A Node helper stands in for the Go executable, exercising the real wrapper.
  const child = spawn(process.execPath, [wrapper, "-e", script, "--", "argument with spaces"], { stdio: "pipe" });
  let stdout = "", stderr = "", helperPID;
  child.stdout.on("data", chunk => { stdout += chunk; });
  child.stderr.on("data", chunk => { stderr += chunk; });
  const closed = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, signal) => resolve({ code, signal }));
  });
  const lines = createInterface({ input: child.stdout });
  const ready = new Promise(resolve => {
    lines.once("line", line => { helperPID = Number(line); resolve(helperPID); });
  });
  t.after(async () => {
    lines.close();
    if (helperPID) {
      try { process.kill(helperPID, "SIGKILL"); } catch { /* Already exited. */ }
    }
    child.kill("SIGKILL");
    await within(closed).catch(() => {});
    rmSync(temp, { recursive: true, force: true });
  });
  return { child, closed, ready, stdout: () => stdout, stderr: () => stderr };
}

test("npm wrapper preserves arguments, stdio, and exit status", async t => {
  const run = wrapperFixture(t, `
    process.stdin.setEncoding('utf8');
    process.stdin.on('data', text => process.stdout.write(process.argv[1] + ':' + text));
    process.stdin.on('end', () => { process.stderr.write('diagnostic'); process.exitCode = 7; });
  `);
  run.child.stdin.end("input");
  assert.deepEqual(await within(run.closed), { code: 7, signal: null });
  assert.equal(run.stdout(), "argument with spaces:input");
  assert.equal(run.stderr(), "diagnostic");
});

test("npm wrapper reports a missing platform executable", async t => {
  const run = wrapperFixture(t, "", { missingBinary: true });
  assert.deepEqual(await within(run.closed), { code: 5, signal: null });
  assert.match(run.stderr(), /ENOENT/);
});

for (const signal of ["SIGTERM", "SIGINT", "SIGHUP"]) {
  test(`npm wrapper forwards direct ${signal} and waits for its child`, { skip: process.platform === "win32" }, async t => {
    const run = wrapperFixture(t, `
      process.on('${signal}', () => { process.stdout.write('received ${signal}'); process.exit(23); });
      process.stdin.resume();
      console.log(process.pid);
    `);
    const pid = await within(run.ready);
    assert.ok(pid > 0);
    run.child.kill(signal);
    assert.deepEqual(await within(run.closed), { code: 23, signal: null });
    assert.match(run.stdout(), new RegExp(`received ${signal}`));
    assert.throws(() => process.kill(pid, 0), { code: "ESRCH" });
  });
}

test("npm wrapper preserves child signal termination", { skip: process.platform === "win32" }, async t => {
  const run = wrapperFixture(t, "process.kill(process.pid, 'SIGTERM')");
  assert.deepEqual(await within(run.closed), { code: null, signal: "SIGTERM" });
});

test("npm wrapper lets EOF shut down the child", async t => {
  const run = wrapperFixture(t, "process.stdin.resume(); console.log(process.pid)");
  await within(run.ready);
  run.child.stdin.end();
  assert.deepEqual(await within(run.closed), { code: 0, signal: null });
});
