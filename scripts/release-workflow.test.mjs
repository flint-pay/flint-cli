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
