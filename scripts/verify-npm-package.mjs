import { pathToFileURL } from "node:url";
import { singlePackResult } from "./npm-pack-result.mjs";

export function verifyPlatformPack(json, name, version, binary) {
  const result = singlePackResult(json);
  if (result.name !== name || result.version !== version) {
    throw new Error(`Expected public npm archive ${name}@${version}`);
  }
  const executable = result.files?.find(file => file.path === binary);
  if (!Number.isSafeInteger(executable?.size) || executable.size <= 0) {
    throw new Error(`Missing nonempty ${binary} in ${name}@${version}`);
  }
  return result;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  let json = "";
  for await (const chunk of process.stdin) json += chunk;
  verifyPlatformPack(json, ...process.argv.slice(2));
}
