import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { createReadStream } from "node:fs";
import { appendFile, lstat, readFile, readdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { releaseVersion } from "./release-version.mjs";

export const containerImage = "ghcr.io/flint-pay/flint-cli";
const targets = ["darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64", "windows_amd64"];
export const npmNames = ["darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64", "windows-amd64", "wrapper"];
const sha256 = bytes => createHash("sha256").update(bytes).digest("hex");

export function releaseFiles(tag) {
  const version = releaseVersion(tag);
  return {
    archives: [...targets.map(target => `flint_${version}_${target}.${target.startsWith("windows") ? "zip" : "tar.gz"}`), "checksums.txt"],
    npm: npmNames.map(target => `npm-packages/flintpay-cli${target === "wrapper" ? "" : `-${target}`}-${version}.tgz`),
    homebrew: ["flint.rb"],
    container: ["container.oci.tar", "container-digest.txt"],
  };
}

async function hashes(path) {
  assert.ok((await lstat(path)).isFile(), `Expected regular artifact: ${path}`);
  const sha = createHash("sha256"), integrity = createHash("sha512");
  for await (const bytes of createReadStream(path)) { sha.update(bytes); integrity.update(bytes); }
  return { sha256: sha.digest("hex"), integrity: `sha512-${integrity.digest("base64")}` };
}

async function verifyInventory(directory, group, expected) {
  if (group === "archives") {
    const archives = (await readdir(directory)).filter(file => /\.(?:tar\.gz|zip)$/.test(file));
    assert.deepEqual(archives.sort(), expected.filter(file => file !== "checksums.txt").sort(), "Unexpected publication archives");
  } else if (group === "npm") {
    const packages = (await readdir(join(directory, "npm-packages"))).filter(file => file.endsWith(".tgz"));
    assert.deepEqual(packages.sort(), expected.map(file => file.slice("npm-packages/".length)).sort(), "Unexpected publication npm tarballs");
  }
}

export async function createManifest(directory, tag, commit, buildDate, schemaHash) {
  assert.match(commit, /^[a-f0-9]{40}$/);
  assert.match(schemaHash, /^[a-f0-9]{64}$/);
  assert.ok(Number.isFinite(Date.parse(buildDate)), "Expected commit build date");
  const groups = releaseFiles(tag), files = {}, npm = [];
  for (const [group, paths] of Object.entries(groups)) await verifyInventory(directory, group, paths);
  for (const file of Object.values(groups).flat()) {
    const digest = await hashes(join(directory, file));
    files[file] = digest.sha256;
    if (groups.npm.includes(file)) {
      const target = npmNames[groups.npm.indexOf(file)];
      npm.push({ name: target === "wrapper" ? "@flintpay/cli" : `@flintpay/cli-${target}`, filename: file, integrity: digest.integrity });
    }
  }
  const digest = (await readFile(join(directory, "container-digest.txt"), "utf8")).trim();
  assert.match(digest, /^sha256:[a-f0-9]{64}$/);
  return { format: 1, tag, commit, build_date: buildDate, schema_hash: schemaHash, files, npm, container: { image: containerImage, digest } };
}

export function reviewedManifest(bytes, expectedHash, tag, commit) {
  assert.match(expectedHash ?? "", /^[a-f0-9]{64}$/, "A frozen review manifest SHA-256 is required");
  assert.equal(sha256(bytes), expectedHash, "Frozen review manifest hash differs");
  const manifest = JSON.parse(bytes);
  assert.equal(manifest.format, 1);
  assert.equal(manifest.tag, tag, "Review applies to another tag");
  assert.equal(manifest.commit, commit, "Review applies to another source commit");
  assert.equal(manifest.container.image, containerImage);
  assert.match(manifest.container.digest, /^sha256:[a-f0-9]{64}$/);
  assert.deepEqual(Object.keys(manifest.files).sort(), Object.values(releaseFiles(tag)).flat().sort(), "Review must cover every publication artifact");
  for (const digest of Object.values(manifest.files)) assert.match(digest, /^[a-f0-9]{64}$/);
  return manifest;
}

export async function compareManifest(actual, bytes, expectedHash) {
  const reviewed = reviewedManifest(bytes, expectedHash, actual.tag, actual.commit);
  assert.deepEqual(actual, reviewed, "Hosted artifacts differ from the frozen review; stop publication");
}

export async function verifyFiles(directory, group, bytes, expectedHash, tag, commit) {
  const reviewed = reviewedManifest(bytes, expectedHash, tag, commit);
  const files = releaseFiles(tag)[group];
  assert.ok(files, `Unknown publication group: ${group}`);
  await verifyInventory(directory, group, files);
  for (const file of files) {
    const actual = await hashes(join(directory, file));
    assert.equal(actual.sha256, reviewed.files[file], `Reviewed artifact differs: ${file}`);
    if (group === "npm") assert.equal(actual.integrity, reviewed.npm.find(item => item.filename === file)?.integrity, `Reviewed npm integrity differs: ${file}`);
  }
  return reviewed;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [mode, directory, ...args] = process.argv.slice(2);
  if (mode === "create") {
    process.stdout.write(`${JSON.stringify(await createManifest(directory, ...args), null, 2)}\n`);
  } else if (mode === "compare") {
    const actual = JSON.parse(await readFile(join(directory, "candidate-release.json"), "utf8"));
    const bytes = process.env.CLI_RELEASE_REVIEW_MANIFEST ?? "";
    const expectedHash = process.env.CLI_RELEASE_REVIEW_SHA256;
    await compareManifest(actual, bytes, expectedHash);
    // Carry the exact externally anchored bytes to publishers. Never read mutable
    // repository variables again after comparison or repack reviewed payloads.
    await writeFile(join(directory, "reviewed-release.json"), bytes);
    if (process.env.GITHUB_OUTPUT) await appendFile(process.env.GITHUB_OUTPUT, `review_sha256=${expectedHash}\n`);
    if (process.env.GITHUB_STEP_SUMMARY) await appendFile(process.env.GITHUB_STEP_SUMMARY, `Frozen review matched ${actual.tag} at ${actual.commit}.\n\nManifest SHA-256: ${expectedHash}\n\nContainer digest: ${actual.container.digest}\n`);
  } else if (mode === "verify") {
    const [group, expectedHash, tag, commit] = args;
    await verifyFiles(directory, group, await readFile(join(directory, "reviewed-release.json")), expectedHash, tag, commit);
  } else {
    throw new Error("Expected create, compare, or verify");
  }
}
