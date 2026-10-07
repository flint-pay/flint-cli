import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { containerImage, verifyFiles } from "./release-manifest.mjs";
import { releaseVersion } from "./release-version.mjs";

// Only explicit registry not-found responses establish absence. Authentication,
// transport, and other registry failures must never permit a version write.
export function registryDigest(result) {
  if (result.error) throw new Error(`Cannot establish container version state: ${result.error.message}`);
  if (result.status === 0) {
    JSON.parse(result.stdout.toString());
    return `sha256:${createHash("sha256").update(result.stdout).digest("hex")}`;
  }
  if (result.status !== null && /\b(?:manifest unknown|name unknown)\b/i.test(result.stderr?.toString() ?? "")) return null;
  throw new Error(`Cannot establish container version state: ${result.error?.message ?? result.stderr?.toString()}`);
}

export async function publishContainer({ directory, reviewHash, tag, commit, authfile }, run = spawnSync) {
  const reviewed = await verifyFiles(directory, "container", await readFile(join(directory, "reviewed-release.json")), reviewHash, tag, commit);
  const digest = reviewed.container.digest;
  const version = releaseVersion(tag);
  const execute = args => run("skopeo", args, { encoding: null });
  const inspect = reference => execute(["inspect", "--raw", "--authfile", authfile, reference]);
  const copy = (source, destination) => {
    const result = execute(["copy", "--all", "--preserve-digests", "--authfile", authfile, source, destination]);
    if (result.error) throw result.error;
    assert.equal(result.status, 0, result.error?.message ?? result.stderr?.toString());
  };
  const source = `oci-archive:${join(directory, "container.oci.tar")}`;
  assert.equal(registryDigest(inspect(source)), digest, "Prepared OCI manifest differs from review");
  const destination = `docker://${containerImage}:v${version}`;
  const existing = registryDigest(inspect(destination));
  if (existing !== null) {
    assert.equal(existing, digest, "Immutable container version already has a different digest; use a new version");
  } else {
    // The workflow serializes publishers. Tag protections and exclusive registry
    // write access must also prevent independent writers racing this check.
    copy(source, destination);
  }
  assert.equal(registryDigest(inspect(destination)), digest, "Published container digest differs from review");
  if (!version.includes("-")) {
    // Stable releases retain latest routing; copy the verified immutable digest.
    copy(`docker://${containerImage}@${digest}`, `docker://${containerImage}:latest`);
    assert.equal(registryDigest(inspect(`docker://${containerImage}:latest`)), digest);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [directory, reviewHash, tag, commit, authfile] = process.argv.slice(2);
  await publishContainer({ directory, reviewHash, tag, commit, authfile });
}
