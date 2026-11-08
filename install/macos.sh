#!/usr/bin/env bash
# gd installer (macOS / Linux), one line:
#   curl -fsSL https://raw.githubusercontent.com/ibbmeintersos-sudo/OnlyDrive/main/install/macos.sh | bash
# Env overrides: GD_REPO=user/repo  GD_VERSION=vX.Y.Z  GD_DEST=/path/to/gd
set -euo pipefail

REPO="${GD_REPO:-ibbmeintersos-sudo/OnlyDrive}"
VER="${GD_VERSION:-latest}"
BASE="https://github.com/$REPO/releases"
API="https://api.github.com/repos/$REPO"

fail() {
  echo
  echo "ERROR: $1" >&2
  echo
  echo "Fallback: build from source (needs Go 1.26+):" >&2
  echo "  git clone https://github.com/$REPO.git && cd OnlyDrive && go build -o /usr/local/bin/gd ./cmd/gd" >&2
  exit 1
}

# Resolve version via the JSON API
if [ "$VER" = "latest" ]; then
  VER=$(curl -fsSL "$API/releases/latest" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p') \
    || fail "cannot reach GitHub releases for '$REPO'. Repo not published yet? Build from source (see below)."
  [ -n "$VER" ] || fail "no releases found for '$REPO'. Build from source (see below)."
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')   # darwin | linux
ARCH=$(uname -m)
[ "$ARCH" = "x86_64" ] && ARCH=amd64
[ "$ARCH" = "aarch64" ] && ARCH=arm64
ASSET="OnlyDrive-$OS-$ARCH"
URL="$BASE/download/$VER/$ASSET"

if [ -n "${GD_DEST:-}" ]; then
  DEST="$GD_DEST"
elif [ -w /usr/local/bin ]; then
  DEST=/usr/local/bin/gd
else
  DEST="$HOME/.local/bin/gd"
  mkdir -p "$(dirname "$DEST")"
fi

TMP=$(mktemp "${TMPDIR:-/tmp}/gd.XXXXXX")
SUMS=$(mktemp "${TMPDIR:-/tmp}/gd-sums.XXXXXX")
trap 'rm -f "$TMP" "$SUMS"' EXIT

echo "downloading gd $VER ($OS/$ARCH) -> $DEST"
curl -fsSL "$URL" -o "$TMP" || fail "download failed ($URL). No asset for this platform yet? Build from source (see below)."
curl -fsSL "$BASE/download/$VER/checksums.txt" -o "$SUMS" \
  || fail "cannot download checksums.txt for $VER; refusing to install an unverified binary."

EXPECTED=$(awk -v a="$ASSET" '$2 == a || $2 == "*" a { print $1; exit }' "$SUMS")
[ -n "$EXPECTED" ] || fail "checksums.txt has no entry for $ASSET"
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL=$(sha256sum "$TMP" | awk '{print $1}')
else
  ACTUAL=$(shasum -a 256 "$TMP" | awk '{print $1}')
fi
if [ "$ACTUAL" != "$EXPECTED" ]; then
  fail "SHA-256 mismatch for $ASSET (expected $EXPECTED, got $ACTUAL)"
fi

chmod +x "$TMP"
mv -f "$TMP" "$DEST"

echo
echo "gd installed. Starting the guided wizard…"
if [ "$DEST" = "$HOME/.local/bin/gd" ]; then
  echo "note: add $HOME/.local/bin to PATH to run 'gd' directly"
fi
"$DEST"
