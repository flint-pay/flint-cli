import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { npmNames, verifyFiles } from "./release-manifest.mjs";
import { releaseVersion } from "./release-version.mjs";

export async function publishNpm({ directory, reviewHash, tag, commit }, run = spawnSync) {
  const reviewed = await verifyFiles(directory, "npm", await readFile(join(directory, "reviewed-release.json")), reviewHash, tag, commit);
  const version = releaseVersion(tag);
  for (const target of npmNames) {
    const name = target === "wrapper" ? "@flintpay/cli" : `@flintpay/cli-${target}`;
    const artifact = reviewed.npm.find(item => item.name === name);
    assert.ok(artifact, `Missing reviewed package ${name}`);
    const result = run("npm", ["view", `${name}@${version}`, "dist.integrity", "--json"], { encoding: "utf8" });
    if (result.error) throw result.error;
    let metadata;
    try { metadata = JSON.parse(result.stdout); } catch { throw new Error(`Cannot establish npm version state for ${name}`); }
    if (result.status === 0) {
      assert.equal(metadata, artifact.integrity, `Published ${name}@${version} differs from review`);
    } else {
      assert.ok(result.status !== null && metadata?.error?.code === "E404", `Cannot establish npm version state for ${name}: ${metadata?.error?.code ?? result.error?.message}`);
      const published = run("npm", ["publish", join(directory, artifact.filename), "--ignore-scripts", "--access", "public", "--provenance", "--tag", version.includes("-") ? "next" : "latest"], { stdio: "inherit" });
      if (published.error) throw published.error;
      assert.equal(published.status, 0, `npm publication failed for ${name}`);
    }
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [directory, reviewHash, tag, commit] = process.argv.slice(2);
  await publishNpm({ directory, reviewHash, tag, commit });
}
