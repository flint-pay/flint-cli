import { readFileSync } from "node:fs";

const [version, tag, checksumPath] = process.argv.slice(2);
if (!version || !/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(version)) {
  throw new Error("A semantic CLI version is required");
}
if (!tag || tag !== `cli/v${version}`) {
  throw new Error(`Expected release tag cli/v${version}`);
}
if (!checksumPath) {
  throw new Error("A checksums file is required");
}

const checksums = new Map(
  readFileSync(checksumPath, "utf8")
    .trim()
    .split("\n")
    .map((line) => {
      const match = line.match(/^([a-f0-9]{64})\s+(.+)$/);
      if (!match) throw new Error(`Invalid checksum line: ${line}`);
      return [match[2], match[1]];
    }),
);

const releaseBase = `https://github.com/flint-pay/flint-cli/releases/download/${encodeURIComponent(tag)}`;
const artifact = (os, arch) => {
  const name = `flint_${version}_${os}_${arch}.tar.gz`;
  const checksum = checksums.get(name);
  if (!checksum) throw new Error(`Missing checksum for ${name}`);
  return { url: `${releaseBase}/${name}`, checksum };
};

const darwinArm64 = artifact("darwin", "arm64");
const darwinAmd64 = artifact("darwin", "amd64");
const linuxArm64 = artifact("linux", "arm64");
const linuxAmd64 = artifact("linux", "amd64");

process.stdout.write(`class Flint < Formula
  desc "Flint Pay command-line interface"
  homepage "https://withflintpay.com"
  version "${version}"
  license "Apache-2.0"

  on_macos do
    if Hardware::CPU.arm?
      url "${darwinArm64.url}"
      sha256 "${darwinArm64.checksum}"
    else
      url "${darwinAmd64.url}"
      sha256 "${darwinAmd64.checksum}"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "${linuxArm64.url}"
      sha256 "${linuxArm64.checksum}"
    else
      url "${linuxAmd64.url}"
      sha256 "${linuxAmd64.checksum}"
    end
  end

  def install
    bin.install "flint"
  end

  test do
    system "#{bin}/flint", "version", "--output", "json"
  end
end
`);
