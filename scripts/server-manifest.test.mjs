import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { stampServerManifest } from "./server-manifest.mjs";

const readJSON = path => JSON.parse(readFileSync(new URL(path, import.meta.url), "utf8"));
const server = readJSON("../server.json");
const wrapper = readJSON("../npm/package.json");

test("server.json and the npm wrapper share their name and development version", () => {
  assert.equal(server.name, wrapper.mcpName, "the MCP registry matches server.json to the npm package's mcpName");
  assert.equal(server.version, wrapper.version, "packaging stamps the release version; do not bump it manually");
  assert.equal(server.packages.length, 1);
  const [npmPackage] = server.packages;
  assert.equal(npmPackage.registryType, "npm");
  assert.equal(npmPackage.identifier, wrapper.name);
  assert.equal(npmPackage.version, wrapper.version);
  assert.equal(npmPackage.runtimeHint, "npx");
  assert.deepEqual(npmPackage.transport, { type: "stdio" });
});

test("packaging stamps the manifest and every package with the release version", () => {
  for (const version of ["0.1.0", "1.2.3-beta.1"]) {
    const stamped = stampServerManifest(server, version);
    assert.equal(stamped.version, version);
    assert.deepEqual(stamped.packages.map(entry => entry.version), [version]);
    // Only the versions change.
    assert.deepEqual(
      { ...stamped, version: server.version, packages: stamped.packages.map(entry => ({ ...entry, version: server.packages[0].version })) },
      server,
    );
  }
  assert.equal(server.version, "0.0.0-development", "stamping must not modify the checked-in manifest");
});

// These mirror the 2025-09-29 server.json schema and the registry's argument
// validation, so a manifest edit cannot drift into a shape registries reject.
test("server.json uses only fields and argument shapes defined by its schema", () => {
  assert.equal(server.$schema, "https://static.modelcontextprotocol.io/schemas/2025-09-29/server.schema.json");
  const allowed = (value, keys, where) => {
    for (const key of Object.keys(value)) assert.ok(keys.includes(key), `${where} has unsupported field ${key}`);
  };
  allowed(server, ["$schema", "name", "description", "version", "websiteUrl", "repository", "packages", "remotes", "_meta"], "server.json");
  assert.match(server.name, /^[a-zA-Z0-9.-]+\/[a-zA-Z0-9._-]+$/);
  assert.ok(server.description.length >= 1 && server.description.length <= 100, "description must have 1-100 characters");
  const inputKeys = ["description", "isRequired", "format", "value", "isSecret", "default", "choices", "variables"];
  const formats = ["string", "number", "boolean", "filepath"];
  for (const entry of server.packages) {
    allowed(entry, ["registryType", "registryBaseUrl", "identifier", "version", "fileSha256", "runtimeHint", "transport", "runtimeArguments", "packageArguments", "environmentVariables"], `package ${entry.identifier}`);
    for (const argument of entry.packageArguments) {
      if (argument.format !== undefined) assert.ok(formats.includes(argument.format), `unsupported format ${argument.format}`);
      if (argument.type === "positional") {
        allowed(argument, ["type", "valueHint", "isRepeated", ...inputKeys], "positional argument");
        assert.ok(argument.value || argument.valueHint, "positional arguments need value or valueHint");
        continue;
      }
      assert.equal(argument.type, "named");
      allowed(argument, ["type", "name", "isRepeated", ...inputKeys], `argument ${argument.name}`);
      assert.match(argument.name, /^--[a-z][a-z-]*$/);
      for (const field of ["value", "default"]) {
        if (argument[field] !== undefined) assert.ok(!argument[field].startsWith(argument.name), `${argument.name} ${field} must not start with its name`);
      }
      assert.equal(argument.isRequired, false, `${argument.name} is optional on the CLI`);
      assert.ok(argument.description, `${argument.name} needs a description`);
      if (argument.format === "boolean") {
        // Clients pass every named argument as --flag VALUE; the schema has
        // no value-less optional switch. A safety flag therefore defaults to
        // false and must never carry a fixed value that always enables it.
        assert.equal(argument.value, undefined, `${argument.name} must not be forced on`);
        assert.equal(argument.default, "false");
        assert.deepEqual(argument.choices, ["true", "false"]);
      }
    }
    for (const variable of entry.environmentVariables) {
      allowed(variable, ["name", ...inputKeys], `environment variable ${variable.name}`);
      assert.match(variable.name, /^FLINT_[A-Z_]+$/);
    }
  }
});

test("server.json describes the MCP server's tool selection and safety flags", () => {
  const named = Object.fromEntries(server.packages[0].packageArguments.filter(argument => argument.type === "named").map(argument => [argument.name, argument]));
  assert.deepEqual(Object.keys(named).sort(), ["--allow-destructive", "--allow-live", "--read-only", "--tools"]);
  const toolsDescription = named["--tools"].description;
  assert.match(toolsDescription, /Opt into typed tools/);
  assert.match(toolsDescription, /operation IDs, dotted resource prefixes, or typed names/);
  assert.match(toolsDescription, /all for every available operation/);
  assert.match(toolsDescription, /Search, describe, and help use the same scope/);
  assert.match(toolsDescription, /Omit for five discovery tools/);
  assert.doesNotMatch(toolsDescription, /curated|schema_|default,GROUP/);
  assert.match(named["--read-only"].description, /refuse every write/);
  assert.doesNotMatch(toolsDescription, /\d[\d,.]*\s*(?:(?:curated\s+)?tools\b|[KM]B\b)/i);
  assert.deepEqual(server.packages[0].packageArguments.filter(argument => argument.type === "positional").map(argument => argument.value), ["mcp", "serve"]);
});
