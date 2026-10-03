import assert from "node:assert/strict";
import test from "node:test";
import { verifyPlatformPack } from "./verify-npm-package.mjs";

test("public platform archives must contain the expected version and executable", () => {
  const name = "@flintpay/cli-windows-amd64";
  const version = "0.1.0-beta.1";
  const binary = "bin/flint.exe";
  const valid = { name, version, filename: "platform.tgz", integrity: "sha512-example", files: [{ path: binary, size: 123 }] };
  for (const json of [JSON.stringify([valid]), JSON.stringify({ [name]: valid })]) {
    assert.deepEqual(verifyPlatformPack(json, name, version, binary), valid);
  }
  for (const change of [
    { name: "@flintpay/cli-linux-arm64" },
    { version: "0.0.9" },
    { files: [] },
    { files: [{ path: "bin/flint", size: 123 }] },
    { files: [{ path: binary, size: 0 }] },
  ]) {
    assert.throws(() => verifyPlatformPack(JSON.stringify([{ ...valid, ...change }]), name, version, binary));
  }
});
