#!/usr/bin/env bash
# Cross-compiles cmd/kram for every supported platform into dist/.
#
# This works at all — no C cross-compiler toolchain, no target-specific
# build image — because of a deliberate choice made early in this
# project: the SQLite driver is modernc.org/sqlite, a pure-Go transpile
# of SQLite's C source, specifically so the daemon's storage layer never
# needs cgo. CGO_ENABLED=0 below is that choice paying off: every target
# in this matrix was verified to actually build and run, not just
# theoretically cross-compile (see DECISIONS.md).
#
# Asset names are stable — kram-linux-amd64.tar.gz, never
# kram-v0.2.3-linux-amd64.tar.gz — and the binary inside every archive is
# named just "kram" (kram.exe on Windows), never the versioned form
# either. This is deliberate, not a simplification made in passing: the
# curl-based installer (see the public kram-releases repo's install.sh)
# needs to construct a download URL and an in-archive path purely from
# OS/ARCH, with the version living only in the release tag/URL segment
# (releases/latest/download/... or releases/download/vX.Y.Z/...) — a
# name that embeds the version would force the installer to either call
# the GitHub API to discover it (an extra dependency and failure point
# curl+tar alone don't need) or parse it out of a redirect. See
# DECISIONS.md, "curl-based install distribution".
#
# Usage: scripts/build-release.sh [version]
# version defaults to the output of `git describe`, falling back to "dev".
# It's injected into the binary via -ldflags (so `kram -version` reports
# it) and, for the host's own OS/ARCH only, verified by actually running
# the freshly built binary — cross-compiled targets can't be executed
# here to check the same way, so they're trusted to match via the same
# ldflags mechanism the verified native build just proved works.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT_DIR="dist"
rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

HOST_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  x86_64) HOST_ARCH="amd64" ;;
  aarch64) HOST_ARCH="arm64" ;;
esac

# GOOS GOARCH pairs — every one of these was hand-verified to build and
# run before being added here (see the release CI's own smoke-test step
# for the automated version of that check).
TARGETS=(
  "linux amd64"
  "linux arm64"
  "darwin amd64"
  "darwin arm64"
  "windows amd64"
)

for target in "${TARGETS[@]}"; do
  read -r os arch <<< "$target"
  ext=""
  bin_name="kram"
  [ "$os" = "windows" ] && ext=".exe" && bin_name="kram.exe"

  work_dir="${OUT_DIR}/.build-${os}-${arch}"
  mkdir -p "$work_dir"
  bin="${work_dir}/${bin_name}"

  echo "building ${os}/${arch}..."
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$bin" ./cmd/kram

  if [ "$os" = "$HOST_OS" ] && [ "$arch" = "$HOST_ARCH" ]; then
    got="$("$bin" -version 2>&1 | awk '{print $NF}')"
    if [ "$got" != "$VERSION" ]; then
      echo "error: built binary reports version '${got}', expected '${VERSION}'" >&2
      exit 1
    fi
    echo "  self-check: kram -version -> ${got} (matches)"
  fi

  asset="${OUT_DIR}/kram-${os}-${arch}"
  if [ "$os" = "windows" ]; then
    archive="${asset}.zip"
    (cd "$work_dir" && zip -q "$(basename "$archive")" "$bin_name" && mv "$(basename "$archive")" "../$(basename "$archive")")
  else
    archive="${asset}.tar.gz"
    (cd "$work_dir" && tar -czf "$(basename "$archive")" "$bin_name" && mv "$(basename "$archive")" "../$(basename "$archive")")
  fi
  rm -rf "$work_dir"
  echo "  -> $archive"
done

echo "generating checksums..."
(
  cd "$OUT_DIR"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum kram-*.tar.gz kram-*.zip > SHA256SUMS
  else
    shasum -a 256 kram-*.tar.gz kram-*.zip > SHA256SUMS
  fi
)
echo "  -> ${OUT_DIR}/SHA256SUMS"

echo
echo "done: $OUT_DIR (version ${VERSION})"
ls -la "$OUT_DIR"
