import { pathToFileURL } from "node:url";

// Release versions intentionally exclude build metadata: npm and all binary
// channels must share one unambiguous version and archive name.
export function releaseVersion(tag) {
  const match = /^cli\/v((0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?)$/.exec(tag);
  if (!match || match[0] !== tag || match[5]?.split(".").some(part => /^\d+$/.test(part) && /^0\d/.test(part))) {
    throw new Error(`Expected cli/vX.Y.Z with an optional SemVer prerelease suffix; got ${tag}`);
  }
  return match[1];
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.stdout.write(`${releaseVersion(process.argv[2])}\n`);
}
