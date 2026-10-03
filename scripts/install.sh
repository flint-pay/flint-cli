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

temp_dir="$(mktemp -d)"
staging_dir=""
cleanup() {
  rm -rf "$temp_dir"
  if [ -n "$staging_dir" ]; then rm -rf "$staging_dir"; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [ "$version" = latest ]; then
  release_url="https://api.github.com/repos/${repository}/releases?per_page=100"
  if ! curl -fsSL "$release_url" -o "${temp_dir}/releases.json"; then
    echo "Could not download the Flint CLI release feed." >&2
    exit 5
  fi
  if ! tag="$(awk '
    # Decode ASCII JSON escapes for the field names and release tags we use.
    function decoded(value, result, i, character, escaped, j, code) {
      for (i=2; i<length(value); i++) {
        character=substr(value, i, 1)
        if (character == "\\") {
          escaped=substr(value, ++i, 1)
          if (escaped == "u") {
            code=0
            for (j=1; j<=4; j++) code=code*16+index("0123456789abcdef", tolower(substr(value, ++i, 1)))-1
            character=(code < 128 ? sprintf("%c", code) : "?")
          } else if (escaped ~ /^[bfnrt]$/) character="?"
          else character=escaped
        }
        result=result character
      }
      return result
    }
    # A release feed can list a backport before a newer version. Compare
    # decimal components as strings to avoid rounding.
    function newer(candidate, current, left, right, i) {
      split(candidate, left, ".")
      split(current, right, ".")
      for (i=1; i<=3; i++) {
        if (length(left[i]) != length(right[i])) return length(left[i]) > length(right[i])
        if ("v" left[i] != "v" right[i]) return "v" left[i] > "v" right[i]
      }
      return 0
    }
    function capture(type, value) {
      if (depth != 2 || container[depth] != "{") return
      if (key[depth] == "tag_name") release_tag=(type == "string" ? decoded(value) : "")
      else if (key[depth] == "prerelease") release_prerelease=type
      else if (key[depth] == "draft") release_draft=type
    }
    function close_container(candidate) {
      if (depth == 2 && container[depth] == "{" && release_prerelease == "false" && (release_draft == "" || release_draft == "false")) {
        candidate=release_tag
        if (candidate ~ /^cli\/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/) {
          sub(/^cli\/v/, "", candidate)
          if (latest == "" || newer(candidate, latest)) latest=candidate
        }
      }
      delete container[depth]
      delete state[depth]
      delete key[depth--]
    }
    # Track JSON containers and syntax, capturing only root release fields.
    # Field order and whitespace may vary; nested metadata is ignored.
    function consume(type, value, closing) {
      if (depth == 0) {
        if (started || type != "[") { invalid=1; return }
        started=1
      } else {
        closing=(container[depth] == "{" ? "}" : "]")
        if (type == closing && (state[depth] == "end" || state[depth] == "key-or-end" || state[depth] == "value-or-end")) {
          close_container()
          return
        }
        if (state[depth] == "end") {
          if (type != ",") { invalid=1; return }
          state[depth]=(container[depth] == "{" ? "key" : "value")
          return
        }
        if (state[depth] == "key" || state[depth] == "key-or-end") {
          if (type != "string") { invalid=1; return }
          key[depth]=decoded(value)
          state[depth]="colon"
          return
        }
        if (state[depth] == "colon") {
          if (type != ":") { invalid=1; return }
          state[depth]="value"
          return
        }
        if (type != "{" && type != "[" && type != "string" && type != "number" && type != "true" && type != "false" && type != "null") {
          invalid=1
          return
        }
        capture(type, value)
        state[depth]="end"
      }
      if (type == "{" || type == "[") {
        container[++depth]=type
        state[depth]=(type == "{" ? "key-or-end" : "value-or-end")
        if (depth == 2 && type == "{") release_tag=release_prerelease=release_draft=""
      }
    }
    {
      line=$0
      while (!invalid && length(line)) {
        sub(/^[[:space:]]+/, "", line)
        if (!length(line)) break
        if (match(line, /^"([^"\\[:cntrl:]]|\\(["\\\/bfnrt]|u[0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]))*"/)) type="string"
        else if (match(line, /^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/)) type="number"
        else if (match(line, /^(true|false|null)/)) type=substr(line, 1, RLENGTH)
        else { type=substr(line, 1, 1); RLENGTH=1 }
        value=substr(line, 1, RLENGTH)
        line=substr(line, RLENGTH+1)
        consume(type, value)
      }
    }
    END {
      if (invalid || !started || depth != 0) exit 5
      if (latest != "") print latest
    }
  ' "${temp_dir}/releases.json")"; then
    echo "The Flint CLI release feed is incomplete or invalid." >&2
    exit 5
  fi
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
staging_dir="$(mktemp -d "${install_dir}/.flint-install.XXXXXX")"
install -m 0755 "${temp_dir}/flint" "${staging_dir}/flint"
if ! installed_version="$("${staging_dir}/flint" version --field data.cli_version --color never)"; then
  echo "Downloaded Flint CLI executable failed version verification." >&2
  exit 5
fi
if [ "$installed_version" != "$tag" ]; then
  echo "Downloaded Flint CLI version ${installed_version} does not match requested version ${tag}." >&2
  exit 5
fi
mv -f "${staging_dir}/flint" "${install_dir}/flint"
"${install_dir}/flint" version
