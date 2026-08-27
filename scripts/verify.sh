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

echo "==> go test -race with per-package coverage gate"
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

# Per-package coverage gate. A single global threshold created the wrong
# incentive (issue #77): for the TUI, where most lines are rendering, the
# cheapest way to hit a high global number is trivial `View() != ""`-style
# asserts rather than tests that would catch a regression. A per-package
# floor keeps the bar high where it protects real logic and honest where a
# uniform number would just reward coverage-chasing.
#
#   - 90%: core business logic — a real strength worth defending.
#   - 70%: internal/cli/app — rendering-dominated; meaningful behavioral
#          tests, not trivial asserts written only to feed the gate.
#   - 60%: internal/localstore — a thin atomic-write helper whose remaining
#          uncovered lines are defensive fs-error branches (fsync/rename
#          failures) that can't be triggered portably; forcing them would be
#          exactly the coverage-chasing this change removes.
#   - 80%: everything else (the default floor).
package_threshold() {
  case "$1" in
    */internal/daemon/agent|*/internal/router|*/internal/permission|*/internal/provider|*/internal/daemon/compaction|*/internal/breaker|*/internal/config|*/internal/daemon/contextpolicy|*/internal/gateway)
      echo 90 ;;
    */internal/cli/app) echo 70 ;;
    */internal/localstore) echo 60 ;;
    *) echo 80 ;;
  esac
}

# Reduce the profile to "package covered total" lines (the package is the
# file path minus its trailing /file.go and the :range suffix).
per_package_coverage="$(awk 'NR > 1 {
  pkg = $1; sub(/:.*/, "", pkg); sub(/\/[^/]*$/, "", pkg)
  total[pkg] += $2
  if ($3 > 0) covered[pkg] += $2
}
END { for (p in total) printf "%s %d %d\n", p, covered[p], total[p] }' "$coverage_profile" | sort)"

coverage_failed=0
grand_covered=0
grand_total=0
while read -r pkg covered total; do
  [ -z "$pkg" ] && continue
  grand_covered=$((grand_covered + covered))
  grand_total=$((grand_total + total))
  threshold="$(package_threshold "$pkg")"
  short="${pkg#github.com/codexmark/kram/}"
  pct="$(awk -v c="$covered" -v t="$total" 'BEGIN { printf (t > 0 ? "%.1f" : "n/a"), (t > 0 ? c * 100 / t : 0) }')"
  # Integer compare (covered*100 >= threshold*total) to avoid float rounding.
  if [ "$total" -gt 0 ] && [ $((covered * 100)) -lt $((threshold * total)) ]; then
    printf "  FAIL %-40s %5s%% < %d%% (%d/%d)\n" "$short" "$pct" "$threshold" "$covered" "$total" >&2
    coverage_failed=1
  else
    printf "  ok   %-40s %5s%% (>= %d%%)\n" "$short" "$pct" "$threshold"
  fi
done <<< "$per_package_coverage"

if [ "$grand_total" -gt 0 ]; then
  awk -v c="$grand_covered" -v t="$grand_total" 'BEGIN { printf "  overall: %.1f%% (%d/%d)\n", c * 100 / t, c, t }'
fi
if [ "$coverage_failed" -ne 0 ]; then
  echo "coverage gate failed: one or more packages are below their per-package threshold (see above)" >&2
  exit 1
fi

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
