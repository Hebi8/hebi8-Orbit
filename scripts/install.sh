#!/usr/bin/env bash
# Install hebi8/orbit binaries (`h8o`, plus optional `h8o-hub` & `h8o-runner`) from GitHub Releases.
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Hebi8/hebi8-Orbit/main/scripts/install.sh | bash
#   curl -fsSL ... | bash -s -- --all --version v0.2.0
set -euo pipefail

REPO="${ORBIT_REPO:-Hebi8/hebi8-Orbit}"
INSTALL_DIR="${ORBIT_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${ORBIT_VERSION:-}"
INSTALL_ALL=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --all) INSTALL_ALL=1; shift ;;
    --version) VERSION="$2"; shift 2 ;;
    --dir) INSTALL_DIR="$2"; shift 2 ;;
    *) echo "Unknown flag: $1" >&2; exit 2 ;;
  esac
done

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

if [[ -z "$VERSION" ]]; then
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
  if [[ -z "$VERSION" ]]; then
    echo "Could not determine latest release tag from ${REPO}; pass --version vX.Y.Z" >&2
    exit 1
  fi
fi

ARCHIVE="hebi8-orbit-${VERSION}-${OS}-${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Downloading ${URL}..."
curl -fsSL "$URL" -o "${TMP}/${ARCHIVE}"
tar -xzf "${TMP}/${ARCHIVE}" -C "$TMP"

STAGE="${TMP}/h8o-${VERSION}-${OS}-${ARCH}"
mkdir -p "$INSTALL_DIR"

install_bin() {
  local name="$1"
  install -m 755 "${STAGE}/${name}" "${INSTALL_DIR}/${name}.new"
  mv -f "${INSTALL_DIR}/${name}.new" "${INSTALL_DIR}/${name}"
  echo "Installed ${INSTALL_DIR}/${name}"
}

install_bin h8o
if [[ "$INSTALL_ALL" -eq 1 ]]; then
  install_bin h8o-hub
  install_bin h8o-runner
fi

echo "Done! Run 'h8o version' and 'h8o doctor' to verify."
