// server.json describes the MCP server for registries. Like npm/package.json,
// the checked-in copy carries a development version that packaging replaces
// with the release version: the manifest and every package it lists must name
// the npm version that is published.
export function stampServerManifest(manifest, version) {
  const stamped = structuredClone(manifest);
  stamped.version = version;
  for (const entry of stamped.packages ?? []) {
    entry.version = version;
  }
  return stamped;
}
