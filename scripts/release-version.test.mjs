import assert from "node:assert/strict";
import test from "node:test";
import { releaseVersion } from "./release-version.mjs";

test("release tags map to the same version across channels", () => {
  for (const version of ["0.1.0", "1.2.3", "0.1.0-beta.1", "1.0.0-rc.0", "1.0.0-beta-1"]) {
    assert.equal(releaseVersion(`cli/v${version}`), version);
  }
});

test("reject ambiguous or invalid versions before publishing", () => {
  for (const tag of [undefined, "v1.2.3", "cli/v01.2.3", "cli/v1.2", "cli/v1.2.3-", "cli/v1.2.3-beta..1", "cli/v1.2.3-01", "cli/v1.2.3+build", "cli/v1.2.3\n"]) {
    assert.throws(() => releaseVersion(tag));
  }
});
