#!/bin/sh
# tn installer.
#
# Usage:
#     curl -fsSL https://raw.githubusercontent.com/cklxx/tune/main/install.sh | sh
#
# Overrides (set as env vars before invoking sh):
#     VERSION=v0.2.0    pin a specific release tag (default: latest)
#     INSTALL_DIR=...   where to drop the binary (default: /usr/local/bin
#                       if writable, else $HOME/.local/bin)
#     FORCE_GO=1        skip the prebuilt-binary path and use `go install`
#
# This script is hand-rolled (no jq / no python / no bash-isms), so it works
# on minimal containers.

set -eu

REPO="cklxx/tune"
BINARY="tn"

# ---- detect platform ----
case "$(uname -s)" in
  Linux*)   os=linux ;;
  Darwin*)  os=darwin ;;
  MINGW*|MSYS*|CYGWIN*)
    echo "windows: please download tn_*.zip from https://github.com/$REPO/releases" >&2
    exit 1
    ;;
  *)
    echo "unsupported OS: $(uname -s)" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *)
    echo "unsupported arch: $(uname -m)" >&2
    exit 1
    ;;
esac

# ---- resolve install dir ----
if [ -z "${INSTALL_DIR:-}" ]; then
  if [ -w /usr/local/bin ] 2>/dev/null; then
    INSTALL_DIR=/usr/local/bin
  else
    INSTALL_DIR="$HOME/.local/bin"
  fi
fi
mkdir -p "$INSTALL_DIR"

# ---- go install fallback ----
go_install() {
  if ! command -v go >/dev/null 2>&1; then
    echo "go not found and no prebuilt release available — install Go from https://go.dev/dl/ or pin VERSION=" >&2
    exit 1
  fi
  ref="${VERSION:-latest}"
  echo "→ go install github.com/$REPO/cmd/$BINARY@$ref"
  GOBIN="$INSTALL_DIR" go install "github.com/$REPO/cmd/$BINARY@$ref"
  echo "✓ installed $INSTALL_DIR/$BINARY"
}

if [ "${FORCE_GO:-}" = "1" ]; then
  go_install
  exit 0
fi

# ---- look up latest release ----
if [ -z "${VERSION:-}" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
    | grep -m1 '"tag_name"' \
    | sed -E 's/.*"tag_name"[^"]*"([^"]+)".*/\1/' || true)
fi

if [ -z "${VERSION:-}" ]; then
  echo "no published release found yet — falling back to \`go install\`"
  go_install
  exit 0
fi

# strip leading v for filenames (goreleaser convention).
ver_no_v="${VERSION#v}"
url="https://github.com/$REPO/releases/download/${VERSION}/${BINARY}_${ver_no_v}_${os}_${arch}.tar.gz"

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t tn)
trap 'rm -rf "$tmp"' EXIT

echo "→ downloading $url"
if ! curl -fsSL "$url" -o "$tmp/pkg.tar.gz"; then
  echo "release archive not found at $url — falling back to \`go install\`"
  go_install
  exit 0
fi

# best-effort checksum verify (don't fail the install if checksums.txt is missing).
if curl -fsSL "https://github.com/$REPO/releases/download/${VERSION}/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
  archive_name="${BINARY}_${ver_no_v}_${os}_${arch}.tar.gz"
  expected=$(grep "  $archive_name\$" "$tmp/checksums.txt" | awk '{print $1}')
  if [ -n "$expected" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      actual=$(sha256sum "$tmp/pkg.tar.gz" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
      actual=$(shasum -a 256 "$tmp/pkg.tar.gz" | awk '{print $1}')
    else
      actual="$expected" # no checker available; trust HTTPS
    fi
    if [ "$expected" != "$actual" ]; then
      echo "checksum mismatch: expected $expected, got $actual" >&2
      exit 1
    fi
  fi
fi

tar -xzf "$tmp/pkg.tar.gz" -C "$tmp"
chmod +x "$tmp/$BINARY"
mv "$tmp/$BINARY" "$INSTALL_DIR/$BINARY"

echo "✓ installed $INSTALL_DIR/$BINARY ($VERSION)"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo
    echo "note: $INSTALL_DIR is not in your PATH. Add this to your shell rc:"
    echo "    export PATH=\"\$PATH:$INSTALL_DIR\""
    ;;
esac

echo
echo "Try:  tn --help"
