#!/usr/bin/env node

const { spawn } = require("node:child_process");
const path = require("node:path");

// Exit codes 1-6 report Flint CLI outcomes such as usage, authentication, and
// network errors. A broken installation is none of these, so the wrapper uses
// the CLI's internal software error code instead.
const EXIT_SOFTWARE = 70;

const platform = process.platform;
const arch = process.arch === "x64" ? "amd64" : process.arch;
const supported = new Set([
  "darwin-arm64",
  "darwin-amd64",
  "linux-arm64",
  "linux-amd64",
  "win32-amd64",
]);
const target = `${platform}-${arch}`;

if (!supported.has(target)) {
  process.stderr.write(`Flint CLI does not publish a binary for ${platform}/${process.arch}. Supported platforms: macOS (arm64, x64), Linux (arm64, x64), and Windows (x64).\n`);
  process.exit(EXIT_SOFTWARE);
}

const packagePlatform = platform === "win32" ? "windows" : platform;
const packageName = `@flintpay/cli-${packagePlatform}-${arch}`;
let packagePath;
try {
  packagePath = path.dirname(require.resolve(`${packageName}/package.json`));
} catch {
  process.stderr.write(`The optional platform package ${packageName} is missing. Reinstall @flintpay/cli without omitting optional dependencies.\n`);
  process.exit(EXIT_SOFTWARE);
}

const binary = path.join(packagePath, "bin", platform === "win32" ? "flint.exe" : "flint");
const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });
// Forward signals sent to this process alone, such as an MCP client stopping
// its server. A terminal Ctrl-C reaches the CLI directly as well, because it
// shares this process group; the CLI ignores such a duplicate for a moment.
const signalHandlers = new Map();
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  const handler = () => child.kill(signal);
  signalHandlers.set(signal, handler);
  process.on(signal, handler);
}
function removeSignalHandlers() {
  for (const [signal, handler] of signalHandlers) {
    process.removeListener(signal, handler);
  }
}
child.on("error", (error) => {
  removeSignalHandlers();
  process.stderr.write(`Could not start the Flint CLI executable from ${packageName}: ${error.message}. Reinstall @flintpay/cli.\n`);
  process.exitCode = EXIT_SOFTWARE;
});
child.on("exit", (code, signal) => {
  removeSignalHandlers();
  if (signal) {
    // Preserve signal termination for callers as well as ordinary exit codes.
    process.kill(process.pid, signal);
  } else {
    process.exitCode = code;
  }
});
