#!/usr/bin/env bash
#
# Install openroutines:
#   curl -fsSL https://get.openroutines.dev/install.sh | bash
#
# OPENROUTINES_VERSION=vX.Y.Z pins a version (default: the latest release).
# OPENROUTINES_INSTALL_DIR=/path overrides the install location (~/.local/bin).
set -euo pipefail

BASE_URL="https://get.openroutines.dev"

fail() {
  printf '%s\n' "$@" >&2
  exit 1
}

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  darwin | linux) ;;
  *) fail "Unsupported OS: $OS" ;;
esac

ARCH=$(uname -m)
case "$ARCH" in
  x86_64 | amd64) ARCH="amd64" ;;
  aarch64 | arm64) ARCH="arm64" ;;
  *) fail "Unsupported architecture: $ARCH" ;;
esac

VERSION="${OPENROUTINES_VERSION:-}"
if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "$BASE_URL/version.txt") || fail "Could not fetch $BASE_URL/version.txt."
fi

# A quoted ~ in OPENROUTINES_INSTALL_DIR reaches us unexpanded; expand it.
INSTALL_DIR="${OPENROUTINES_INSTALL_DIR:-$HOME/.local/bin}"
INSTALL_DIR="${INSTALL_DIR/#\~/$HOME}"

BINARY="openroutines_${VERSION}_${OS}_${ARCH}"
RELEASE_URL="https://github.com/steadyspacecorp/openroutines/releases/download/$VERSION"

TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT
cd "$TMP_DIR"

echo "Downloading openroutines $VERSION ($OS/$ARCH)..."
curl -fsSLO "$RELEASE_URL/$BINARY" || fail "Could not download $RELEASE_URL/$BINARY."
curl -fsSLO "$RELEASE_URL/checksums.txt" || fail "Could not download $RELEASE_URL/checksums.txt."

EXPECTED=$(awk -v f="$BINARY" '$2 == f { print $1; exit }' checksums.txt)
[ -n "$EXPECTED" ] || fail "No checksum for $BINARY in checksums.txt."
ACTUAL=$( (sha256sum "$BINARY" 2>/dev/null || shasum -a 256 "$BINARY") | awk '{ print $1 }')
[ "$EXPECTED" = "$ACTUAL" ] || fail "Checksum mismatch!" "  Expected: $EXPECTED" "  Actual:   $ACTUAL"

mkdir -p "$INSTALL_DIR"
chmod +x "$BINARY"
# Stage beside the target and rename into place: atomic, and safe while an
# openroutines process is running -- cp over a live binary fails with ETXTBSY
# on Linux and can corrupt the running process on macOS.
cp "$BINARY" "$INSTALL_DIR/.openroutines.new"
mv -f "$INSTALL_DIR/.openroutines.new" "$INSTALL_DIR/openroutines"

echo "Installed openroutines $VERSION to $INSTALL_DIR"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "Note: $INSTALL_DIR is not on your PATH." ;;
esac
