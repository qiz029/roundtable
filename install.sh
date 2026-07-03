#!/usr/bin/env sh
set -eu

repo="qiz029/roundtable"
binary_name="roundtable-agent"
version="${ROUNDTABLE_AGENT_VERSION:-latest}"
install_dir="${ROUNDTABLE_INSTALL_DIR:-$HOME/.local/bin}"

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

detect_os() {
  case "$(uname -s)" in
    Darwin) printf '%s\n' "Darwin" ;;
    Linux) printf '%s\n' "Linux" ;;
    *)
      echo "unsupported operating system: $(uname -s)" >&2
      exit 1
      ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    arm64 | aarch64) printf '%s\n' "arm64" ;;
    x86_64 | amd64) printf '%s\n' "x86_64" ;;
    *)
      echo "unsupported architecture: $(uname -m)" >&2
      exit 1
      ;;
  esac
}

release_base_url() {
  if [ -n "${ROUNDTABLE_DOWNLOAD_BASE_URL:-}" ]; then
    printf '%s\n' "$ROUNDTABLE_DOWNLOAD_BASE_URL"
    return
  fi

  if [ "$version" = "latest" ]; then
    printf 'https://github.com/%s/releases/latest/download\n' "$repo"
    return
  fi

  case "$version" in
    v*) tag="$version" ;;
    *) tag="v$version" ;;
  esac
  printf 'https://github.com/%s/releases/download/%s\n' "$repo" "$tag"
}

checksum_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
    return
  fi
  echo "missing required command: sha256sum or shasum" >&2
  exit 1
}

install_binary() {
  src="$1"
  dst="$install_dir/$binary_name"

  if [ -d "$install_dir" ] && [ -w "$install_dir" ]; then
    install -m 0755 "$src" "$dst"
    return
  fi

  parent_dir="$(dirname "$install_dir")"
  if [ -d "$parent_dir" ] && [ -w "$parent_dir" ]; then
    install -d "$install_dir"
    install -m 0755 "$src" "$dst"
    return
  fi

  if command -v sudo >/dev/null 2>&1; then
    sudo install -d "$install_dir"
    sudo install -m 0755 "$src" "$dst"
    return
  fi

  echo "install directory is not writable: $install_dir" >&2
  echo "set ROUNDTABLE_INSTALL_DIR to a writable directory" >&2
  exit 1
}

need_cmd curl
need_cmd tar
need_cmd awk
need_cmd grep
need_cmd install

os="$(detect_os)"
arch="$(detect_arch)"
asset="${binary_name}_${os}_${arch}.tar.gz"
base_url="$(release_base_url)"
tmpdir="$(mktemp -d)"

cleanup() {
  rm -rf "$tmpdir"
}
trap cleanup EXIT INT TERM

archive="$tmpdir/$asset"
checksums="$tmpdir/checksums.txt"

echo "Downloading $asset from $base_url"
curl -fsSL "$base_url/$asset" -o "$archive"
curl -fsSL "$base_url/checksums.txt" -o "$checksums"

expected="$(grep "  $asset\$" "$checksums" | awk '{print $1}')"
if [ -z "$expected" ]; then
  echo "checksum for $asset not found" >&2
  exit 1
fi

actual="$(checksum_file "$archive")"
if [ "$actual" != "$expected" ]; then
  echo "checksum mismatch for $asset" >&2
  echo "expected: $expected" >&2
  echo "actual:   $actual" >&2
  exit 1
fi

tar -xzf "$archive" -C "$tmpdir" "$binary_name"
install_binary "$tmpdir/$binary_name"

echo "$binary_name installed to $install_dir/$binary_name"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "Add $install_dir to PATH to run $binary_name from any shell." ;;
esac
"$install_dir/$binary_name" version
