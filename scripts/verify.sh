#!/usr/bin/env bash
# Reproducible local quality gate used in place of unavailable hosted CI.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

echo "==> git diff check"
git diff --check

echo "==> gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "$unformatted" >&2
  exit 1
fi

echo "==> go vet"
go vet ./...

echo "==> go test -race"
go test ./... -race -count=1

echo "==> host build"
go build ./...
go build -o "$tmp_dir/kram" ./cmd/kram
"$tmp_dir/kram" -version >/dev/null

echo "==> Windows cross-build"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o "$tmp_dir/kram.exe" ./cmd/kram

echo "==> Android/Termux cross-build"
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o "$tmp_dir/kram-android-arm64" ./cmd/kram

echo "==> installer syntax and behavior"
bash -n scripts/dist-repo/install.sh scripts/build-release.sh scripts/release.sh scripts/tests/install_test.sh scripts/tests/install_ps1_test.sh
bash scripts/tests/install_test.sh
bash scripts/tests/install_ps1_test.sh
if command -v pwsh >/dev/null 2>&1; then
  pwsh -NoProfile -Command '$errors=$null; [void][System.Management.Automation.Language.Parser]::ParseFile("scripts/dist-repo/install.ps1", [ref]$null, [ref]$errors); if ($errors.Count) { $errors | ForEach-Object { Write-Error $_ }; exit 1 }'
else
  echo "  pwsh unavailable: install.ps1 parser check skipped"
fi

echo "verification passed"
