#!/bin/sh
# Install drivekey: downloads the release binary for this machine and checks its checksum.
# Usage: curl -fsSL https://raw.githubusercontent.com/jerryfane/drivekey/main/install.sh | sh
# Env: DRIVEKEY_VERSION (default: latest), DRIVEKEY_INSTALL_DIR (default: ~/.local/bin).
set -eu

repo="jerryfane/drivekey"
version="${DRIVEKEY_VERSION:-latest}"
dir="${DRIVEKEY_INSTALL_DIR:-$HOME/.local/bin}"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "drivekey install.sh supports Linux and macOS; on Windows download the .exe from https://github.com/$repo/releases" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$version" = latest ]; then
  base="https://github.com/$repo/releases/latest/download"
else
  base="https://github.com/$repo/releases/download/$version"
fi
asset="drivekey_${os}_${arch}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
want=$(awk -v a="$asset" '$2 == a {print $1}' "$tmp/checksums.txt")
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else
  got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo "checksum mismatch for $asset (want '$want', got '$got')" >&2
  exit 1
fi

mkdir -p "$dir"
install -m 0755 "$tmp/$asset" "$dir/drivekey"
echo "installed $dir/drivekey"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "note: $dir is not on PATH; add it or run $dir/drivekey" ;;
esac

if ! command -v gcloud >/dev/null 2>&1; then
  echo
  echo "drivekey needs Google's gcloud CLI for the login step, and it is not installed."
  echo "Install it from https://cloud.google.com/sdk/docs/install"
  echo "(Linux/macOS: curl https://sdk.cloud.google.com | bash)"
fi
