#!/bin/sh
set -eu

repository="flint-pay/flint-cli"
version="${FLINT_CLI_VERSION:-latest}"
install_dir="${FLINT_INSTALL_DIR:-/usr/local/bin}"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "Unsupported operating system: $(uname -s)" >&2; exit 2 ;;
esac

case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 2 ;;
esac

if [ "$version" = latest ]; then
  release_url="https://api.github.com/repos/${repository}/releases?per_page=100"
  tag="$(curl -fsSL "$release_url" | awk '
    /"tag_name":[[:space:]]*"cli\/v/ {
      line=$0
      sub(/^.*"tag_name":[[:space:]]*"cli\/v/, "", line)
      sub(/".*$/, "", line)
      candidate=line
    }
    /"prerelease":[[:space:]]*false/ && candidate != "" { print candidate; exit }
    /"prerelease":[[:space:]]*true/ { candidate="" }
  ')"
  if [ -z "$tag" ]; then echo "Could not resolve the latest Flint CLI release." >&2; exit 5; fi
else
  tag="${version#v}"
fi

if ! printf '%s\n' "$tag" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "Invalid Flint CLI version: ${tag}" >&2
  exit 2
fi

archive="flint_${tag}_${os}_${arch}.tar.gz"
base_url="https://github.com/${repository}/releases/download/cli%2Fv${tag}"
temp_dir="$(mktemp -d)"
trap 'rm -rf "$temp_dir"' EXIT INT TERM

curl -fsSL "${base_url}/${archive}" -o "${temp_dir}/${archive}"
curl -fsSL "${base_url}/checksums.txt" -o "${temp_dir}/checksums.txt"

expected="$(awk -v archive="$archive" '$2 == archive {print $1}' "${temp_dir}/checksums.txt")"
if [ -z "$expected" ]; then echo "Release checksum is missing for ${archive}." >&2; exit 5; fi
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "${temp_dir}/${archive}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "${temp_dir}/${archive}" | awk '{print $1}')"
else
	echo "A SHA-256 checksum tool is required (sha256sum or shasum)." >&2
	exit 5
fi
if [ "$actual" != "$expected" ]; then echo "Checksum verification failed for ${archive}." >&2; exit 5; fi

tar -xzf "${temp_dir}/${archive}" -C "$temp_dir"
mkdir -p "$install_dir"
install -m 0755 "${temp_dir}/flint" "${install_dir}/flint"
"${install_dir}/flint" version
