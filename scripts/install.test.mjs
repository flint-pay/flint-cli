import assert from "node:assert/strict";
import { spawnSync, execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const installer = fileURLToPath(new URL("./install.sh", import.meta.url));
const release = (version, prerelease = false) => ({ tag_name: `cli/v${version}`, prerelease });

function runInstaller(releases, expectedVersion, requestedVersion = "latest", options = {}) {
  const temp = mkdtempSync(join(tmpdir(), "flint-install-selection-"));
  try {
    const mockBin = join(temp, "mock-bin");
    const contents = join(temp, "contents");
    const installDir = join(temp, "installed");
    mkdirSync(mockBin);
    mkdirSync(contents);
    writeFileSync(join(contents, "flint"), `#!/bin/sh
if [ "\${2:-}" = --field ]; then
  printf '%s\\n' '${options.binaryVersion ?? expectedVersion}'
  exit ${options.binaryExitCode ?? 0}
fi
printf 'installed fixture\\n'
`);
    chmodSync(join(contents, "flint"), 0o755);
    const archive = `flint_${expectedVersion}_linux_amd64.tar.gz`;
    const archivePath = join(temp, archive);
    execFileSync("tar", ["-czf", archivePath, "-C", contents, "flint"]);
    const checksum = createHash("sha256").update(readFileSync(archivePath)).digest("hex");
    writeFileSync(join(temp, "checksums.txt"), `${checksum}  ${archive}\n`);
    writeFileSync(join(temp, "releases.json"), options.releaseBody ?? JSON.stringify(releases, null, 2));
    const oldBinary = "#!/bin/sh\nprintf 'old working CLI\\n'\n";
    if (options.existingInstall) {
      mkdirSync(installDir);
      writeFileSync(join(installDir, "flint"), oldBinary);
      chmodSync(join(installDir, "flint"), 0o755);
    }
    writeFileSync(join(mockBin, "uname"), `#!/bin/sh
case "$1" in
  -s) printf 'Linux\\n' ;;
  -m) printf 'x86_64\\n' ;;
  *) exit 9 ;;
esac
`);
    writeFileSync(join(mockBin, "curl"), `#!/bin/sh
set -eu
url="$2"
printf '%s\\n' "$url" >> "$FLINT_TEST_REQUESTS"
case "$url" in
  'https://api.github.com/repos/flint-pay/flint-cli/releases?per_page=100')
    cp "$FLINT_TEST_RELEASES" "$4"
    exit "$FLINT_TEST_FEED_STATUS" ;;
  "$FLINT_TEST_BASE_URL/$FLINT_TEST_ARCHIVE")
    cp "$FLINT_TEST_ARCHIVE_PATH" "$4" ;;
  "$FLINT_TEST_BASE_URL/checksums.txt")
    cp "$FLINT_TEST_CHECKSUMS" "$4" ;;
  *) printf 'Unexpected download URL: %s\\n' "$url" >&2; exit 9 ;;
esac
`);
    chmodSync(join(mockBin, "uname"), 0o755);
    chmodSync(join(mockBin, "curl"), 0o755);
    if (options.copyFailure) {
      // Simulate a copy that has started writing and then runs out of space.
      writeFileSync(join(mockBin, "install"), `#!/bin/sh
printf 'partial executable' > "$4"
exit 9
`);
      chmodSync(join(mockBin, "install"), 0o755);
    }
    const result = spawnSync("sh", [installer], {
      encoding: "utf8",
      env: {
        ...process.env,
        PATH: `${mockBin}${delimiter}${process.env.PATH}`,
        FLINT_CLI_VERSION: requestedVersion,
        FLINT_INSTALL_DIR: installDir,
        FLINT_TEST_REQUESTS: join(temp, "requests"),
        FLINT_TEST_RELEASES: join(temp, "releases.json"),
        FLINT_TEST_BASE_URL: `https://github.com/flint-pay/flint-cli/releases/download/cli%2Fv${expectedVersion}`,
        FLINT_TEST_ARCHIVE: archive,
        FLINT_TEST_ARCHIVE_PATH: archivePath,
        FLINT_TEST_CHECKSUMS: join(temp, "checksums.txt"),
        FLINT_TEST_FEED_STATUS: String(options.feedStatus ?? 0),
      },
    });
    return {
      ...result,
      requests: readFileSync(join(temp, "requests"), "utf8").trim().split("\n"),
      installed: existsSync(join(installDir, "flint")),
      oldInstallPreserved: options.existingInstall && readFileSync(join(installDir, "flint"), "utf8") === oldBinary,
      installedOutput: existsSync(join(installDir, "flint"))
        ? spawnSync(join(installDir, "flint"), ["version"], { encoding: "utf8" }).stdout
        : "",
      leftoverStaging: existsSync(installDir) ? readdirSync(installDir).filter(name => name.startsWith(".flint-install.")) : [],
    };
  } finally {
    rmSync(temp, { recursive: true, force: true });
  }
}

const shellTest = process.platform === "win32" ? test.skip : test;

shellTest("latest installs the highest stable version even when a backport was published last", () => {
  for (const releases of [[release("1.0.1"), release("2.0.0")], [release("2.0.0"), release("1.0.1")]]) {
    const result = runInstaller(releases, "2.0.0");
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.installed, true);
    assert.match(result.stdout, /installed fixture/);
    assert.equal(result.requests.length, 3);
    assert.ok(result.requests[1].includes("cli%2Fv2.0.0/flint_2.0.0_"));
  }
});

shellTest("latest supports compact JSON and release fields in any order", () => {
  const releases = [release("2.0.0"), release("1.0.1")];
  for (const releaseBody of [
    JSON.stringify(releases),
    JSON.stringify(releases.map(({ tag_name, prerelease }) => ({ prerelease, draft: false, tag_name })), null, 2),
    String.raw`[{"prerelease":false,"\u0074ag_name":"cli\/v2.0.0"},{"prerelease":false,"tag_name":"cli/v1.0.1"}]`,
  ]) {
    const result = runInstaller(releases, "2.0.0", "latest", { releaseBody });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.installed, true);
    assert.ok(result.requests[1].includes("cli%2Fv2.0.0/flint_2.0.0_"));
  }
});

shellTest("latest compares major, minor, and patch components numerically", () => {
  for (const versions of [["9.0.0", "10.0.0"], ["1.9.0", "1.10.0"], ["1.0.9", "1.0.10"], ["1.0.9007199254740992", "1.0.9007199254740993"]]) {
    const result = runInstaller(versions.map(version => release(version)), versions[1]);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.installed, true);
  }
});

shellTest("latest ignores prereleases, other product tags, and invalid stable versions", () => {
  const result = runInstaller([
    release("99.0.0-beta.1", true),
    release("98.0.0", true),
    release("97.0.0-beta.1"),
    release("96.0.0+build"),
    release("095.0.0"),
    release("94.0"),
    { prerelease: false, tag_name: "cli/v93.0.0", draft: true },
    { tag_name: "server/v93.0.0", prerelease: false },
    release("2.0.0"),
    release("1.0.1"),
  ], "2.0.0");
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.installed, true);
});

shellTest("latest accepts nested GitHub release objects and escaped body text", () => {
  const result = runInstaller([
    {
      author: { login: "release-bot" },
      ...release("2.0.0"),
      assets: [{ name: "flint.tar.gz", uploader: { login: "release-bot", ...release("99.0.0") } }],
      body: 'Notes: [example] {object} "quoted" \\path\nSecond line with "tag_name": "cli/v99.0.0".',
    },
    release("1.0.1"),
  ], "2.0.0");
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.installed, true);
});

shellTest("latest fails before downloading or installing when no stable CLI release exists", () => {
  for (const releases of [[], [release("2.0.0-beta.1", true), { tag_name: "server/v2.0.0", prerelease: false }]]) {
    const result = runInstaller(releases, "unused");
    assert.equal(result.status, 5, result.stderr);
    assert.match(result.stderr, /Could not resolve the latest Flint CLI release/);
    assert.equal(result.requests.length, 1);
    assert.equal(result.installed, false);
  }
});

shellTest("explicit versions keep their requested target and skip latest selection", () => {
  for (const version of ["1.0.1", "1.0.1-beta.1"]) {
    const result = runInstaller([release("2.0.0")], version, `v${version}`);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.installed, true);
    assert.equal(result.requests.length, 2);
    assert.ok(result.requests[0].includes(`cli%2Fv${version}/flint_${version}_`));
  }
});

shellTest("latest rejects failed or truncated feeds before touching an existing installation", () => {
  const body = JSON.stringify([release("1.0.1"), release("2.0.0")], null, 2);
  for (const options of [
    { feedStatus: 18 },
    { releaseBody: body.slice(0, -1) },
    { releaseBody: body.slice(0, body.indexOf('"tag_name": "cli/v2.0.0"')) },
    { releaseBody: body.replace(/\]\s*$/, "}") },
    { releaseBody: body.replace("},", "}") },
    { releaseBody: `${body} false` },
  ]) {
    const result = runInstaller([release("1.0.1"), release("2.0.0")], "2.0.0", "latest", { ...options, existingInstall: true });
    assert.equal(result.status, 5, result.stderr);
    assert.equal(result.requests.length, 1);
    assert.equal(result.oldInstallPreserved, true);
    assert.equal(result.installedOutput, "old working CLI\n");
    assert.deepEqual(result.leftoverStaging, []);
  }
});

shellTest("wrong or broken downloaded executables preserve the existing working CLI", () => {
  for (const options of [{ binaryVersion: "1.0.1" }, { binaryExitCode: 9 }]) {
    const result = runInstaller([release("2.0.0")], "2.0.0", "latest", { ...options, existingInstall: true });
    assert.equal(result.status, 5, result.stderr);
    assert.match(result.stderr, /does not match requested version|failed version verification/);
    assert.equal(result.oldInstallPreserved, true);
    assert.equal(result.installedOutput, "old working CLI\n");
    assert.deepEqual(result.leftoverStaging, []);
  }
});

shellTest("a failed destination copy preserves the existing working CLI and removes partial staging", () => {
  const result = runInstaller([release("2.0.0")], "2.0.0", "latest", { existingInstall: true, copyFailure: true });
  assert.equal(result.status, 9, result.stderr);
  assert.equal(result.oldInstallPreserved, true);
  assert.equal(result.installedOutput, "old working CLI\n");
  assert.deepEqual(result.leftoverStaging, []);
});

shellTest("successful replacement verifies the staged version and removes staging", () => {
  const result = runInstaller([release("2.0.0")], "2.0.0", "latest", { existingInstall: true });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.oldInstallPreserved, false);
  assert.equal(result.installedOutput, "installed fixture\n");
  assert.deepEqual(result.leftoverStaging, []);
});
