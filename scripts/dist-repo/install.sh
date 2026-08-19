#!/usr/bin/env bash
# Kram installer.
#
#   curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
#
# Downloads the right prebuilt binary for this machine from GitHub
# Releases, verifies its SHA-256 checksum against the release's own
# SHA256SUMS file, and installs it to $HOME/.local/bin (override with
# KRAM_INSTALL_DIR). No sudo, no shell-rc edits, no dependency beyond
# curl/tar and a sha256 tool most systems already have. Windows isn't
# handled here — see the repo README for the manual .zip download.
set -euo pipefail

REPO="${KRAM_RELEASES_REPO:-codexmark/kram-releases}"
INSTALL_DIR="${KRAM_INSTALL_DIR:-$HOME/.local/bin}"
REQUESTED_VERSION="${KRAM_VERSION:-}"

echo "Kram installer"
echo

# ---- 1. platform detection ----------------------------------------------

os_raw="$(uname -s)"
case "$os_raw" in
  Linux) os="linux" ;;
  Darwin) os="darwin" ;;
  *)
    echo "Kram: unsupported OS: $os_raw" >&2
    echo "Windows users: download kram-windows-amd64.zip from" >&2
    echo "  https://github.com/${REPO}/releases/latest" >&2
    exit 1
    ;;
esac

arch_raw="$(uname -m)"
case "$arch_raw" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *)
    echo "Kram: unsupported architecture: $arch_raw" >&2
    exit 1
    ;;
esac

echo "Platform: ${os}/${arch}"
echo "Version:  ${REQUESTED_VERSION:-latest}"
echo

# ---- 2. build download URLs ----------------------------------------------

asset="kram-${os}-${arch}.tar.gz"
if [ -n "${KRAM_BASE_URL:-}" ]; then
  # Internal escape hatch for testing this script against a local or
  # mirrored release layout — never needed for a normal install.
  base_url="$KRAM_BASE_URL"
elif [ -n "$REQUESTED_VERSION" ]; then
  base_url="https://github.com/${REPO}/releases/download/${REQUESTED_VERSION}"
else
  base_url="https://github.com/${REPO}/releases/latest/download"
fi
asset_url="${base_url}/${asset}"
sums_url="${base_url}/SHA256SUMS"

# ---- 3. temp workspace, cleaned up on exit no matter what ---------------

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

# ---- 4. download ----------------------------------------------------------

echo "Downloading..."
if ! curl -fsSL "$asset_url" -o "$tmp_dir/$asset"; then
  echo "Kram: failed to download ${asset_url}" >&2
  echo "  (check that this OS/arch combination has a release, and that KRAM_VERSION is valid if set)" >&2
  exit 1
fi

if ! curl -fsSL "$sums_url" -o "$tmp_dir/SHA256SUMS"; then
  echo "Kram: failed to download SHA256SUMS from ${sums_url}" >&2
  exit 1
fi

# ---- 5. checksum verification ---------------------------------------------

echo "Verifying SHA-256..."
expected="$(grep " ${asset}\$" "$tmp_dir/SHA256SUMS" | awk '{print $1}')"
if [ -z "$expected" ]; then
  echo "Kram: no checksum entry for ${asset} in SHA256SUMS — refusing to install an unverifiable binary" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp_dir/$asset" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp_dir/$asset" | awk '{print $1}')"
else
  echo "Kram: no sha256sum or shasum found — cannot verify the download's integrity" >&2
  echo "  install one of those tools and try again" >&2
  exit 1
fi

if [ "$expected" != "$actual" ]; then
  echo "Kram: checksum mismatch for ${asset}" >&2
  echo "  expected: $expected" >&2
  echo "  actual:   $actual" >&2
  echo "  refusing to install a corrupted or tampered download" >&2
  exit 1
fi

# ---- 6. extract -------------------------------------------------------

tar -xzf "$tmp_dir/$asset" -C "$tmp_dir"
if [ ! -f "$tmp_dir/kram" ]; then
  echo "Kram: extracted archive did not contain a 'kram' binary — this looks like a packaging bug, not a network issue" >&2
  exit 1
fi

# ---- 7. install (only after everything above has succeeded) -----------

mkdir -p "$INSTALL_DIR"
echo "Installing to ${INSTALL_DIR}/kram..."
if command -v install >/dev/null 2>&1; then
  install -m 755 "$tmp_dir/kram" "$INSTALL_DIR/kram"
else
  cp "$tmp_dir/kram" "$INSTALL_DIR/kram"
  chmod 755 "$INSTALL_DIR/kram"
fi

# ---- 8. post-install validation ----------------------------------------

if ! installed_version="$("$INSTALL_DIR/kram" -version 2>&1)"; then
  echo "Kram: installed binary at ${INSTALL_DIR}/kram failed to run" >&2
  echo "  $installed_version" >&2
  exit 1
fi

echo
echo "${installed_version} installed successfully."
echo

# ---- 9. PATH guidance ----------------------------------------------------

if command -v kram >/dev/null 2>&1; then
  echo "Run:"
  echo
  echo "  kram"
else
  echo "Kram was installed to:"
  echo
  echo "  ${INSTALL_DIR}/kram"
  echo
  echo "But ${INSTALL_DIR} is not on your PATH. Add this to your shell's"
  echo "config (~/.bashrc, ~/.zshrc, etc.) and open a new shell:"
  echo
  echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
fi
