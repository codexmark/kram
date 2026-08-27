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

echo "==> architecture boundary check"
# Fail fast on a layering breach (CLI reaching into daemon/gateway/router
# internals, or the daemon importing the gateway) before the slow test
# sweep runs. The same test also runs inside the coverage step below; this
# early, named invocation just surfaces a boundary regression immediately
# with a clear message. See internal/archcheck.
go test ./internal/archcheck/ -run TestKramLayeringHolds -count=1

echo "==> go test -race with global coverage"
coverage_profile="$tmp_dir/coverage.out"
# devtools/ is intentionally gitignored local scratch space, so it must not
# make the reproducible repository gate depend on whichever disposable mocks
# happen to exist on one maintainer's machine.
package_output="$(go list ./...)"
coverage_packages=()
while IFS= read -r package; do
  case "$package" in
    */devtools/*) ;;
    *) coverage_packages+=("$package") ;;
  esac
done <<< "$package_output"
if [ "${#coverage_packages[@]}" -eq 0 ]; then
  echo "coverage gate failed: go list returned no tracked packages" >&2
  exit 1
fi
go test "${coverage_packages[@]}" -race -count=1 -covermode=atomic -coverprofile="$coverage_profile"
coverage_stats="$(awk 'NR > 1 {
  total += $2
  if ($3 > 0) covered += $2
}
END {
  if (total <= 0) exit 1
  printf "%d %d %.6f", covered, total, covered * 100 / total
}' "$coverage_profile")"
read -r coverage_covered coverage_statements coverage_percent <<< "$coverage_stats"
echo "  total statement coverage: ${coverage_percent}% (${coverage_covered}/${coverage_statements})"
awk -v covered="$coverage_covered" -v total="$coverage_statements" 'BEGIN {
  if (covered * 100 < total * 90) {
    printf "coverage gate failed: %.6f%% is below 90.0%%\n", covered * 100 / total > "/dev/stderr"
    exit 1
  }
}'

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
