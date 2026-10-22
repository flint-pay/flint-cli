import assert from "node:assert/strict";
import { spawnSync, execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const installer = fileURLToPath(new URL("./install.sh", import.meta.url));
const release = (version, prerelease = false) => ({ tag_name: `cli/v${version}`, prerelease });

function runInstaller(releases, expectedVersion, requestedVersion = "latest", options = {}) {
  const temp = mkdtempSync(join(tmpdir(), "flint-install-selection-"));
  // Never let a test reach the real /usr/local/bin, even when it omits FLINT_INSTALL_DIR.
  const systemDir = join(temp, "system", "bin");
  try {
    const mockBin = join(temp, "mock-bin");
    const contents = join(temp, "contents");
    const home = join(temp, "home");
    const installDir = join(temp, "installed");
    const homeInstallDir = join(home, ".local", "bin");
    mkdirSync(mockBin);
    mkdirSync(contents);
    mkdirSync(home);
    mkdirSync(dirname(systemDir));
    if (options.systemDir !== "missing") mkdirSync(systemDir);
    if (options.systemDir === "read-only") chmodSync(systemDir, 0o555);
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
printf '%s\\n' "$*" >> "$FLINT_TEST_ARGUMENTS"
url="" output="" write_out="" config="" headers=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output="$2"; shift 2 ;;
    -D) headers="$2"; shift 2 ;;
    -w) write_out="$2"; shift 2 ;;
    -K) config="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s\\n' "$url" >> "$FLINT_TEST_REQUESTS"
if [ "$config" = - ]; then cat >> "$FLINT_TEST_CURL_CONFIG"; fi
case "$url" in
  'https://github.com/flint-pay/flint-cli/releases/latest')
    printf 'HTTP/2 %s\\r\\nlocation: %s\\r\\n\\r\\n' "$FLINT_TEST_REDIRECT_HTTP_STATUS" "$FLINT_TEST_REDIRECT_LOCATION" > "$headers"
    printf '%s' "$FLINT_TEST_REDIRECT_HTTP_STATUS"
    exit "$FLINT_TEST_REDIRECT_STATUS" ;;
  'https://api.github.com/repos/flint-pay/flint-cli/releases?per_page=100')
    cp "$FLINT_TEST_RELEASES" "$output"
    if [ "$write_out" = '%{http_code}' ]; then printf '%s' "$FLINT_TEST_FEED_HTTP_STATUS"; fi
    exit "$FLINT_TEST_FEED_STATUS" ;;
  "$FLINT_TEST_BASE_URL/$FLINT_TEST_ARCHIVE")
    cp "$FLINT_TEST_ARCHIVE_PATH" "$output" ;;
  "$FLINT_TEST_BASE_URL/checksums.txt")
    cp "$FLINT_TEST_CHECKSUMS" "$output" ;;
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
    const env = {
      ...process.env,
      PATH: [mockBin, ...(options.onPath ?? []).map(name => join(temp, name)), process.env.PATH].join(delimiter),
      HOME: home,
      FLINT_API_KEY: "flint_test_xxx",
      FLINT_CLI_VERSION: requestedVersion,
      FLINT_INSTALL_DIR: installDir,
      FLINT_TEST_SYSTEM_INSTALL_DIR: systemDir,
      FLINT_TEST_ARGUMENTS: join(temp, "arguments"),
      FLINT_TEST_REQUESTS: join(temp, "requests"),
      FLINT_TEST_CURL_CONFIG: join(temp, "curl-config"),
      FLINT_TEST_RELEASES: join(temp, "releases.json"),
      FLINT_TEST_BASE_URL: `https://github.com/flint-pay/flint-cli/releases/download/cli%2Fv${expectedVersion}`,
      FLINT_TEST_ARCHIVE: archive,
      FLINT_TEST_ARCHIVE_PATH: archivePath,
      FLINT_TEST_CHECKSUMS: join(temp, "checksums.txt"),
      FLINT_TEST_REDIRECT_LOCATION: options.redirectLocation ?? "",
      FLINT_TEST_REDIRECT_HTTP_STATUS: String(options.redirectHTTPStatus ?? 302),
      FLINT_TEST_REDIRECT_STATUS: String(options.redirectStatus ?? 0),
      FLINT_TEST_FEED_STATUS: String(options.feedStatus ?? 0),
      FLINT_TEST_FEED_HTTP_STATUS: String(options.feedHTTPStatus ?? 200),
    };
    delete env.GITHUB_TOKEN;
    if (options.githubToken) env.GITHUB_TOKEN = options.githubToken;
    if (options.defaultInstallDir) delete env.FLINT_INSTALL_DIR;
    const result = spawnSync("sh", [installer], { encoding: "utf8", env });
    const read = name => existsSync(join(temp, name)) ? readFileSync(join(temp, name), "utf8") : "";
    return {
      ...result,
      requests: read("requests").trim().split("\n"),
      curlArguments: read("arguments"),
      curlConfig: read("curl-config"),
      installed: existsSync(join(installDir, "flint")),
      installedInHome: existsSync(join(homeInstallDir, "flint")),
      installedInSystem: existsSync(join(systemDir, "flint")),
      homeInstallDir,
      systemDir,
      oldInstallPreserved: options.existingInstall && readFileSync(join(installDir, "flint"), "utf8") === oldBinary,
      installedOutput: existsSync(join(installDir, "flint"))
        ? spawnSync(join(installDir, "flint"), ["version"], { encoding: "utf8" }).stdout
        : "",
      leftoverStaging: existsSync(installDir) ? readdirSync(installDir).filter(name => name.startsWith(".flint-install.")) : [],
    };
  } finally {
    if (existsSync(systemDir)) chmodSync(systemDir, 0o755);
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
    assert.equal(result.requests.length, 4);
    assert.ok(result.requests[2].includes("cli%2Fv2.0.0/flint_2.0.0_"));
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
    assert.ok(result.requests[2].includes("cli%2Fv2.0.0/flint_2.0.0_"));
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
    assert.equal(result.requests.length, 2);
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
    assert.equal(result.requests.length, 2);
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

const runsAsRoot = process.getuid?.() === 0;

shellTest("an omitted install directory falls back to ~/.local/bin when the system directory is not writable", { skip: runsAsRoot && "root can write to read-only directories" }, () => {
  const result = runInstaller([release("2.0.0")], "2.0.0", "latest", { defaultInstallDir: true, systemDir: "read-only" });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.installedInHome, true);
  assert.equal(result.installedInSystem, false);
  assert.match(result.stdout, /installed fixture/);
  assert.ok(result.stderr.includes(`${result.systemDir} is not writable, so Flint CLI will be installed in ${result.homeInstallDir}.`), result.stderr);
  assert.ok(result.stderr.includes(`${result.homeInstallDir} is not on your PATH.`), result.stderr);
  assert.ok(result.stderr.includes(`export PATH="${result.homeInstallDir}:$PATH"`), result.stderr);
});

shellTest("an omitted install directory uses a writable or creatable system directory", () => {
  for (const systemDir of ["writable", "missing"]) {
    const result = runInstaller([release("2.0.0")], "2.0.0", "latest", { defaultInstallDir: true, systemDir, onPath: ["system/bin"] });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.installedInSystem, true);
    assert.equal(result.installedInHome, false);
    assert.equal(result.stderr, "");
  }
});

shellTest("the PATH hint appears only when the install directory is not on PATH", () => {
  const onPath = runInstaller([release("2.0.0")], "2.0.0", "latest", { onPath: ["installed"] });
  assert.equal(onPath.status, 0, onPath.stderr);
  assert.equal(onPath.stderr, "");
  const offPath = runInstaller([release("2.0.0")], "2.0.0");
  assert.equal(offPath.status, 0, offPath.stderr);
  assert.match(offPath.stderr, /installed is not on your PATH\. Add it to your shell profile/);
});

shellTest("latest authenticates the release lookup with GITHUB_TOKEN without exposing it", () => {
  const token = "installer-test-token";
  const result = runInstaller([release("2.0.0")], "2.0.0", "latest", { githubToken: token });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.installed, true);
  assert.equal(result.curlConfig, `header = "Authorization: Bearer ${token}"\n`);
  assert.equal(result.curlArguments.includes(token), false, "the token must not appear in curl's arguments");
  assert.equal(`${result.stdout}${result.stderr}`.includes(token), false, "the token must not be printed");
  // Only the API lookup reads the credential; release downloads never receive it.
  assert.equal(result.curlArguments.trim().split("\n").filter(line => line.includes("-K -")).length, 1);
});

shellTest("latest sends no credential when GITHUB_TOKEN is unset", () => {
  const result = runInstaller([release("2.0.0")], "2.0.0");
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.curlConfig, "");
  assert.doesNotMatch(result.curlArguments, /Authorization|-K/);
});

shellTest("latest explains GitHub rate limiting before touching an existing installation", () => {
  const token = "installer-test-token";
  for (const [feedHTTPStatus, githubToken, message] of [
    [403, undefined, /HTTP 403\), usually because its API rate limit for anonymous requests was reached\. Set GITHUB_TOKEN to a GitHub token to raise the limit, or set FLINT_CLI_VERSION/],
    [429, undefined, /HTTP 429\), usually because its API rate limit for anonymous requests was reached\. Set GITHUB_TOKEN/],
    [403, token, /HTTP 403\), usually because the API rate limit for GITHUB_TOKEN was reached\. Wait for the limit to reset, or set FLINT_CLI_VERSION/],
    [401, token, /GitHub rejected GITHUB_TOKEN \(HTTP 401\)/],
    [500, undefined, /Could not download the Flint CLI release feed \(HTTP 500\)\./],
  ]) {
    const result = runInstaller([release("2.0.0")], "2.0.0", "latest", { feedHTTPStatus, githubToken, existingInstall: true });
    assert.equal(result.status, 5, result.stderr);
    assert.match(result.stderr, message);
    assert.equal(result.requests.length, 2);
    assert.equal(result.oldInstallPreserved, true);
    assert.equal(result.stderr.includes(token), false);
  }
});

const latestTagURL = "https://github.com/flint-pay/flint-cli/releases/tag/cli/v";

shellTest("latest uses the web redirect without the API or a token", () => {
  const token = "installer-test-token";
  for (const redirectLocation of [`${latestTagURL}2.0.0`, "/flint-pay/flint-cli/releases/tag/cli/v2.0.0"]) {
    const result = runInstaller([], "2.0.0", "latest", { redirectLocation, githubToken: token, feedHTTPStatus: 429 });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.installed, true);
    assert.equal(result.requests.length, 3);
    assert.equal(result.requests.some(url => url.includes("api.github.com")), false);
    assert.equal(result.curlConfig, "");
    assert.equal(`${result.stdout}${result.stderr}${result.curlArguments}`.includes(token), false);
    const redirectArguments = result.curlArguments.trim().split("\n")[0];
    assert.doesNotMatch(redirectArguments, /-\w*L|--location|-K/);
  }
});

shellTest("latest falls back for invalid redirects, non-redirects, and request failures", () => {
  for (const options of [
    { redirectLocation: `${latestTagURL}2.0.0-beta.1` },
    { redirectLocation: `${latestTagURL}2.0.0+build` },
    { redirectLocation: `${latestTagURL}02.0.0` },
    { redirectLocation: `${latestTagURL}2.0.0?query=1` },
    { redirectLocation: `${latestTagURL}2.0.0#fragment` },
    { redirectLocation: "https://example.com/flint-pay/flint-cli/releases/tag/cli/v2.0.0" },
    { redirectLocation: "https://github.com/other/repo/releases/tag/cli/v2.0.0" },
    { redirectLocation: `${latestTagURL}2.0.0`, redirectHTTPStatus: 200 },
    { redirectLocation: `${latestTagURL}2.0.0`, redirectStatus: 7 },
    {},
  ]) {
    const result = runInstaller([release("2.0.0")], "2.0.0", "latest", options);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.installed, true);
    assert.equal(result.requests.length, 4);
    assert.match(result.requests[1], /api\.github\.com/);
  }
});
