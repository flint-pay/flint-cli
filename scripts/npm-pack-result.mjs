import { pathToFileURL } from "node:url";

export function singlePackResult(json) {
  const result = JSON.parse(json);
  // npm 11 returns an array; npm 12 keys results by package name.
  const entries = Array.isArray(result) ? result : Object.values(result);
  if (entries.length !== 1 || typeof entries[0]?.filename !== "string" || typeof entries[0]?.integrity !== "string") {
    throw new Error("Expected one npm pack result with filename and integrity");
  }
  return entries[0];
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  let json = "";
  for await (const chunk of process.stdin) json += chunk;
  process.stdout.write(singlePackResult(json).integrity);
}
