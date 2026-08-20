#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
RELEASE="$TMP/release"
mkdir -p "$RELEASE"

make_asset() {
  local platform="$1"
  local work="$TMP/$platform"
  mkdir -p "$work"
  printf '#!/bin/sh\necho "kram test-%s"\n' "$platform" > "$work/kram"
  chmod 755 "$work/kram"
  tar -czf "$RELEASE/kram-${platform}.tar.gz" -C "$work" kram
}

make_asset linux-amd64
make_asset android-arm64
(
  cd "$RELEASE"
  sha256sum kram-*.tar.gz > SHA256SUMS
)

linux_install="$TMP/linux-install"
HOME="$TMP/home" KRAM_BASE_URL="file://$RELEASE" KRAM_INSTALL_DIR="$linux_install" \
  bash "$ROOT/scripts/dist-repo/install.sh" >/dev/null
test "$("$linux_install/kram" -version)" = "kram test-linux-amd64"

fake_bin="$TMP/fake-bin"
mkdir -p "$fake_bin"
printf '#!/bin/sh\ncase "$1" in -s) echo Linux ;; -m) echo aarch64 ;; *) exit 1 ;; esac\n' > "$fake_bin/uname"
chmod 755 "$fake_bin/uname"
android_install="$TMP/android-install"
PATH="$fake_bin:$PATH" HOME="$TMP/home" TERMUX_VERSION="test" PREFIX="/data/data/com.termux/files/usr" \
  KRAM_BASE_URL="file://$RELEASE" KRAM_INSTALL_DIR="$android_install" \
  bash "$ROOT/scripts/dist-repo/install.sh" >/dev/null
test "$("$android_install/kram" -version)" = "kram test-android-arm64"

# A checksum failure must leave a previously installed binary untouched.
printf 'tampered' >> "$RELEASE/kram-linux-amd64.tar.gz"
if HOME="$TMP/home" KRAM_BASE_URL="file://$RELEASE" KRAM_INSTALL_DIR="$linux_install" \
  bash "$ROOT/scripts/dist-repo/install.sh" >/dev/null 2>&1; then
  echo "installer accepted a bad checksum" >&2
  exit 1
fi
test "$("$linux_install/kram" -version)" = "kram test-linux-amd64"

echo "install.sh tests passed"
