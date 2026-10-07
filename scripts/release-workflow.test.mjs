import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const workflow = readFileSync(new URL("../.github/workflows/cli-release.yml", import.meta.url), "utf8");
// Job-level blocks have two-space indentation; nested steps cannot match.
const jobs = new Map([...workflow.matchAll(/^  ([\w-]+):\n([\s\S]*?)(?=^  [\w-]+:|$(?![\s\S]))/gm)]
  .map(([, name, body]) => [name, body]));

test("GitHub update discovery waits for public npm installation readiness", () => {
  const github = jobs.get("publish-github");
  const needs = github.match(/^    needs: \[([^\]]+)\]/m)[1].split(",").map(value => value.trim());
  assert.ok(needs.includes("npm-readiness"), "GitHub publication must wait for registry propagation");

  const readiness = jobs.get("npm-readiness");
  assert.match(readiness, /needs: \[release, npm\]/,
    "An npm failure or pending approval must block readiness checks");
  assert.match(readiness, /os: \[ubuntu-latest, macos-latest\]/,
    "Readiness must cover runnable public installations on Linux and macOS");
  assert.match(readiness, /runs-on: \$\{\{ matrix\.os \}\}/);
  assert.match(readiness, /run: bash scripts\/verify-npm-install\.sh/);
  assert.match(readiness, /CLI_VERSION: \$\{\{ needs\.release\.outputs\.cli_version \}\}/);
  assert.match(readiness, /NPM_TAG: \$\{\{ needs\.release\.outputs\.npm_tag \}\}/);
});

const dependencies = name => {
  const value = jobs.get(name).match(/^    needs: (.+)$/m)?.[1];
  return value ? value.replace(/[\[\]]/g, "").split(",").map(item => item.trim()) : [];
};

test("every channel writer waits for exact frozen comparison and shared approval", () => {
  const publishers = ["npm", "docker", "publish-github", "homebrew"];
  for (const publisher of publishers) {
    assert.ok(dependencies(publisher).includes("frozen-compare"), `${publisher} must compare reviewed bytes`);
    assert.ok(dependencies(publisher).includes("publication-approval"), `${publisher} must wait for approval`);
    assert.doesNotMatch(jobs.get(publisher), /always\(\)|!cancelled\(\)/, "A failed gate cannot be bypassed by a publishing condition");
    assert.match(jobs.get(publisher), /flint-cli-reviewed-manifest/);
  }
  assert.deepEqual(dependencies("publication-approval"), ["frozen-compare"]);
  assert.match(jobs.get("publication-approval"), /^    environment: npm$/m);
  assert.deepEqual(dependencies("frozen-compare").sort(), ["release", "channel-preflight", "install-preflight", "container-build"].sort());
  assert.match(jobs.get("frozen-compare"), /CLI_RELEASE_REVIEW_MANIFEST: \$\{\{ vars\.CLI_RELEASE_REVIEW_MANIFEST \}\}/);
  assert.match(jobs.get("frozen-compare"), /CLI_RELEASE_REVIEW_SHA256: \$\{\{ vars\.CLI_RELEASE_REVIEW_SHA256 \}\}/);
  assert.match(jobs.get("frozen-compare"), /scripts\/release-manifest\.mjs compare dist/);
});

test("preapproval preparation cannot publish and publishers consume frozen files", () => {
  for (const preparer of ["release", "channel-preflight", "container-build", "frozen-compare", "publication-approval"]) {
    assert.doesNotMatch(jobs.get(preparer), /(?:contents|packages|attestations): write|push: true|npm publish|gh release create|git .*push/);
  }
  assert.match(jobs.get("container-build"), /push: false/);
  assert.match(jobs.get("container-build"), /outputs: type=oci,/);
  assert.match(jobs.get("container-build"), /provenance: mode=max/);
  assert.match(jobs.get("container-build"), /sbom: true/);
  assert.doesNotMatch(jobs.get("docker"), /build-push-action|push: true/);
  assert.match(jobs.get("docker"), /scripts\/publish-container\.mjs/);
  assert.doesNotMatch(jobs.get("npm"), /scripts\/package-npm\.mjs|npm pack/);
  assert.match(jobs.get("npm"), /scripts\/publish-npm\.mjs/);
  assert.match(jobs.get("publish-github"), /release-manifest\.mjs verify dist archives/);
  assert.match(jobs.get("homebrew"), /release-manifest\.mjs verify dist homebrew/);
  assert.match(jobs.get("homebrew"), /cp dist\/flint\.rb/);
  assert.match(jobs.get("homebrew"), /is_prerelease == 'false'/);
  assert.ok(dependencies("verify-published").includes("docker"));
  assert.match(jobs.get("verify-published"), /needs\.docker\.result == 'success'/);
});
