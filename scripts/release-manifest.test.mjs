import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";
import { compareManifest, createManifest, releaseFiles, reviewedManifest, verifyFiles } from "./release-manifest.mjs";
import { publishContainer, registryDigest } from "./publish-container.mjs";
import { publishNpm } from "./publish-npm.mjs";

const commit = "a".repeat(40), schema = "b".repeat(64), date = "2026-10-06T12:00:00Z";
// Synthetic bytes exercise the gate and publisher decisions. They are not OCI
// images, npm payloads, or evidence of a Docker build or registry publication.
const raw = Buffer.from('{"schemaVersion":2,"manifests":[]}');
const digest = `sha256:${createHash("sha256").update(raw).digest("hex")}`;
const hash = bytes => createHash("sha256").update(bytes).digest("hex");

async function fixture(t, tag = "cli/v0.0.0-test.1") {
  const directory = await mkdtemp(join(tmpdir(), "flint-publication-gate-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  for (const file of Object.values(releaseFiles(tag)).flat()) {
    await mkdir(dirname(join(directory, file)), { recursive: true });
    await writeFile(join(directory, file), file === "container-digest.txt" ? `${digest}\n` : `synthetic ${file}\n`);
  }
  const manifest = await createManifest(directory, tag, commit, date, schema);
  const bytes = `${JSON.stringify(manifest, null, 2)}\n`;
  await writeFile(join(directory, "reviewed-release.json"), bytes);
  return { directory, tag, commit, reviewHash: hash(bytes), authfile: "/synthetic/auth.json", manifest, bytes };
}

test("frozen comparison requires an external anchor and exact source identity", async t => {
  const f = await fixture(t);
  await compareManifest(f.manifest, f.bytes, f.reviewHash);
  for (const anchor of [undefined, "", "0".repeat(64)]) {
    await assert.rejects(compareManifest(f.manifest, f.bytes, anchor));
  }
  for (const change of [{ tag: "cli/v0.0.0-test.2" }, { commit: "c".repeat(40) }, { build_date: "2026-10-07T12:00:00Z" }, { schema_hash: "c".repeat(64) }, { container: { ...f.manifest.container, digest: `sha256:${"c".repeat(64)}` } }]) {
    await assert.rejects(compareManifest({ ...f.manifest, ...change }, f.bytes, f.reviewHash));
  }
  const omitted = structuredClone(f.manifest);
  delete omitted.files["container.oci.tar"];
  const bytes = JSON.stringify(omitted);
  assert.throws(() => reviewedManifest(bytes, hash(bytes), f.tag, commit), /every publication artifact/);
});

test("every archive, checksum, npm tarball, formula, and OCI byte is gated", async t => {
  const f = await fixture(t);
  for (const [group, files] of Object.entries(releaseFiles(f.tag))) {
    for (const file of files) {
      const path = join(f.directory, file), original = await readFile(path);
      await writeFile(path, Buffer.concat([original, Buffer.from("changed")]));
      await assert.rejects(verifyFiles(f.directory, group, f.bytes, f.reviewHash, f.tag, commit), /artifact differs/);
      if (file !== "container-digest.txt") {
        const changed = await createManifest(f.directory, f.tag, commit, date, schema);
        await assert.rejects(compareManifest(changed, f.bytes, f.reviewHash), /differ from the frozen review/);
      }
      await writeFile(path, original);
    }
    await verifyFiles(f.directory, group, f.bytes, f.reviewHash, f.tag, commit);
  }
  const wrongIntegrity = structuredClone(f.manifest);
  wrongIntegrity.npm[0].integrity = "sha512-wrong";
  const bytes = JSON.stringify(wrongIntegrity);
  await assert.rejects(verifyFiles(f.directory, "npm", bytes, hash(bytes), f.tag, commit), /npm integrity differs/);
});

test("unreviewed archives, missing artifacts, and symlink payloads stop comparison", async t => {
  const f = await fixture(t);
  const extra = join(f.directory, "unreviewed.tar.gz");
  await writeFile(extra, "extra");
  await assert.rejects(createManifest(f.directory, f.tag, commit, date, schema), /Unexpected publication archives/);
  await assert.rejects(verifyFiles(f.directory, "archives", f.bytes, f.reviewHash, f.tag, commit), /Unexpected publication archives/);
  await rm(extra);
  const path = join(f.directory, "container.oci.tar");
  await rm(path);
  await assert.rejects(createManifest(f.directory, f.tag, commit, date, schema), { code: "ENOENT" });
  await symlink(join(f.directory, "flint.rb"), path);
  await assert.rejects(createManifest(f.directory, f.tag, commit, date, schema), /regular artifact/);
});

test("comparison CLI fails closed and carries the exact reviewed manifest to publishers", async t => {
  const f = await fixture(t);
  await writeFile(join(f.directory, "candidate-release.json"), f.bytes);
  await rm(join(f.directory, "reviewed-release.json"));
  const run = env => spawnSync(process.execPath, [new URL("./release-manifest.mjs", import.meta.url).pathname, "compare", f.directory], {
    encoding: "utf8", env: { ...process.env, GITHUB_OUTPUT: join(f.directory, "outputs"), GITHUB_STEP_SUMMARY: join(f.directory, "summary"), CLI_RELEASE_REVIEW_MANIFEST: "", CLI_RELEASE_REVIEW_SHA256: "", ...env },
  });
  assert.notEqual(run({}).status, 0);
  await assert.rejects(readFile(join(f.directory, "reviewed-release.json")), { code: "ENOENT" });
  const result = run({ CLI_RELEASE_REVIEW_MANIFEST: f.bytes, CLI_RELEASE_REVIEW_SHA256: f.reviewHash });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(await readFile(join(f.directory, "reviewed-release.json"), "utf8"), f.bytes);
  assert.equal(await readFile(join(f.directory, "outputs"), "utf8"), `review_sha256=${f.reviewHash}\n`);
});

test("container registry failures establish absence only with explicit not-found errors", () => {
  assert.equal(registryDigest({ status: 0, stdout: raw }), digest);
  assert.equal(registryDigest({ status: 1, stderr: Buffer.from("reading manifest: manifest unknown") }), null);
  for (const error of ["unauthorized", "denied", "TLS handshake timeout", "connection refused", "503 Service Unavailable"]) {
    assert.throws(() => registryDigest({ status: 1, stderr: Buffer.from(error) }), /Cannot establish/);
  }
  assert.throws(() => registryDigest({ status: null, error: new Error("skopeo unavailable") }), /Cannot establish/);
  assert.throws(() => registryDigest({ status: 0, stdout: raw, error: new Error("EPERM") }), /Cannot establish/);
  assert.throws(() => registryDigest({ status: 0, stdout: Buffer.from("invalid") }));
});

test("container retries verify existing version digests without overwriting versions", async t => {
  const f = await fixture(t);
  for (const state of ["same", "different", "missing", "unauthorized", "timeout"]) {
    const copies = [];
    let inspected = false;
    const run = (command, args) => {
      assert.equal(command, "skopeo");
      const reference = args.at(-1);
      if (args[0] === "copy") {
        copies.push(args);
        return { status: 0 };
      }
      if (reference.startsWith("oci-archive:")) return { status: 0, stdout: raw };
      if (!inspected) {
        inspected = true;
        if (state === "different") return { status: 0, stdout: Buffer.from('{"different":true}') };
        if (state === "missing") return { status: 1, stderr: Buffer.from("manifest unknown") };
        if (state === "unauthorized" || state === "timeout") return { status: 1, stderr: Buffer.from(state) };
      }
      return { status: 0, stdout: raw };
    };
    if (["different", "unauthorized", "timeout"].includes(state)) await assert.rejects(publishContainer(f, run));
    else await publishContainer(f, run);
    assert.equal(copies.length, state === "missing" ? 1 : 0, state);
    if (copies.length) {
      assert.ok(copies[0].includes("--all"));
      assert.ok(copies[0].includes("--preserve-digests"));
      assert.ok(copies[0].at(-2).startsWith("oci-archive:"));
      assert.match(copies[0].at(-1), /:v0\.0\.0-test\.1$/);
    }
  }
});

test("container publication verifies its exported and final digest and preserves stable latest routing", async t => {
  const f = await fixture(t, "cli/v0.0.0");
  const copies = [];
  await publishContainer(f, (command, args) => {
    if (args[0] === "copy") { copies.push(args); return { status: 0 }; }
    return { status: 0, stdout: raw };
  });
  assert.equal(copies.length, 1, "An existing version is immutable; only stable latest is copied");
  assert.ok(copies[0].at(-2).endsWith(`@${digest}`));
  assert.ok(copies[0].at(-1).endsWith(":latest"));
  await assert.rejects(publishContainer(f, () => ({ status: 0, stdout: Buffer.from('{"wrong":true}') })), /Prepared OCI manifest/);
  let versionChecks = 0;
  await assert.rejects(publishContainer(f, (command, args) => {
    if (args[0] === "copy") return { status: 0 };
    if (args.at(-1).startsWith("oci-archive:")) return { status: 0, stdout: raw };
    if (++versionChecks === 1) return { status: 1, stderr: Buffer.from("manifest unknown") };
    return { status: 0, stdout: Buffer.from('{"wrong":true}') };
  }), /Published container digest/);
});

test("npm publishes frozen tarballs in platform-before-wrapper order and verifies retries", async t => {
  for (const tag of ["cli/v0.0.0-test.1", "cli/v0.0.0"]) {
    const f = await fixture(t, tag), publications = [];
    await publishNpm(f, (command, args) => {
      assert.equal(command, "npm");
      if (args[0] === "view") return { status: 1, stdout: '{"error":{"code":"E404"}}' };
      publications.push(args);
      return { status: 0 };
    });
    assert.deepEqual(publications.map(args => args[1]), f.manifest.npm.map(item => join(f.directory, item.filename)));
    assert.ok(publications.every(args => args.at(-1) === (tag.includes("-") ? "next" : "latest")));
    await publishNpm(f, (command, args) => {
      assert.equal(args[0], "view", "Retries must not write existing npm versions");
      return { status: 0, stdout: JSON.stringify(f.manifest.npm.find(item => args[1].startsWith(`${item.name}@`)).integrity) };
    });
    for (const result of [{ status: 0, stdout: '"sha512-wrong"' }, { status: 1, stdout: '{"error":{"code":"E401"}}' }, { status: 1, stdout: "network error" }]) {
      await assert.rejects(publishNpm(f, () => result));
    }
  }
});

test("publisher byte checks reject changes before any registry read or write", async t => {
  const f = await fixture(t);
  const run = () => assert.fail("A changed reviewed payload cannot reach a registry");
  await writeFile(join(f.directory, "container.oci.tar"), "changed");
  await assert.rejects(publishContainer(f, run), /artifact differs/);
  await writeFile(join(f.directory, f.manifest.npm[0].filename), "changed");
  await assert.rejects(publishNpm(f, run), /artifact differs/);
});
