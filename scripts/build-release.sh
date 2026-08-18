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
# Usage: scripts/build-release.sh [version]
# version defaults to the output of `git describe`, falling back to "dev".
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT_DIR="dist"
rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

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
  [ "$os" = "windows" ] && ext=".exe"

  name="kram-${VERSION}-${os}-${arch}"
  bin="${OUT_DIR}/${name}${ext}"

  echo "building ${os}/${arch}..."
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$bin" ./cmd/kram

  archive="${OUT_DIR}/${name}.tar.gz"
  if [ "$os" = "windows" ]; then
    archive="${OUT_DIR}/${name}.zip"
    (cd "$OUT_DIR" && zip -q "$(basename "$archive")" "$(basename "$bin")")
  else
    (cd "$OUT_DIR" && tar -czf "$(basename "$archive")" "$(basename "$bin")")
  fi
  rm "$bin"
  echo "  -> $archive"
done

echo
echo "done: $OUT_DIR"
ls -la "$OUT_DIR"
