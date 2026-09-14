import assert from "node:assert/strict";
import test from "node:test";
import { singlePackResult } from "./npm-pack-result.mjs";

test("read npm 11 array and npm 12 package-keyed pack results", () => {
  const result = { filename: "flintpay-cli-0.1.0.tgz", integrity: "sha512-example" };
  assert.deepEqual(singlePackResult(JSON.stringify([result])), result);
  assert.deepEqual(singlePackResult(JSON.stringify({ "@flintpay/cli": result })), result);
  for (const invalid of [[], {}, [{ filename: "incomplete" }], [result, result]]) {
    assert.throws(() => singlePackResult(JSON.stringify(invalid)));
  }
});
