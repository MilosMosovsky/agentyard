#!/bin/sh
# agentyard installer:
#   curl -fsSL https://raw.githubusercontent.com/MilosMosovsky/agentyard/main/scripts/install.sh | sh
# Env: AGENTYARD_VERSION (e.g. v0.1.0, default latest), AGENTYARD_BIN_DIR (default ~/.local/bin).
# It only installs the binary. Starting it in the background is `agentyard install`, which asks first.
set -eu

REPO="MilosMosovsky/agentyard"
BIN_DIR="${AGENTYARD_BIN_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

have curl || die "curl is required"
have tar || die "tar is required"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "unsupported OS: $(uname -s) (macOS and Linux only)" ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

version="${AGENTYARD_VERSION:-}"
if [ -z "$version" ]; then
  version="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
  [ -n "$version" ] || die "could not determine the latest release; set AGENTYARD_VERSION"
fi
case "$version" in v*) ;; *) version="v$version" ;; esac
plain="${version#v}"

archive="agentyard_${plain}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading agentyard $version ($os/$arch)"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "download failed: $base/checksums.txt"

expected="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$expected" ] || die "$archive not listed in checksums.txt"
if have shasum; then
  actual="$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')"
elif have sha256sum; then
  actual="$(sha256sum "$tmp/$archive" | awk '{ print $1 }')"
else
  die "need shasum or sha256sum to verify the download"
fi
[ "$expected" = "$actual" ] || die "checksum mismatch for $archive"
say "Checksum OK"

tar -xzf "$tmp/$archive" -C "$tmp" agentyard
mkdir -p "$BIN_DIR"
mv "$tmp/agentyard" "$BIN_DIR/agentyard"
chmod 755 "$BIN_DIR/agentyard"
# Unsigned binary: let macOS run it without the Gatekeeper prompt.
[ "$os" = darwin ] && xattr -d com.apple.quarantine "$BIN_DIR/agentyard" 2>/dev/null || true

say "Installed $BIN_DIR/agentyard"
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "Note: $BIN_DIR is not on your PATH. Add this to your shell profile:"
     say "  export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac

say ""
say "Next:"
say "  agentyard demo       try it on sample data"
say "  agentyard install    run it in the background at login (asks before changing anything)"
