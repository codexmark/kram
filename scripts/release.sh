#!/usr/bin/env bash
# The one command that publishes a Kram release: local checks, local
# build, then a GitHub Release on the public distribution repo. No CI
# runner is involved anywhere in this path — see DECISIONS.md,
# "curl-based install distribution", for why that's a deliberate
# constraint (the GitHub account this project uses has no working
# Actions billing, and more importantly: a release the maintainer's own
# machine hasn't successfully built and tested locally should never ship
# regardless of CI availability).
#
# Usage: scripts/release.sh vX.Y.Z [--notes FILE] [--yes]
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# The public repo release assets get uploaded to — override for local
# testing against a scratch repo without touching this file.
RELEASES_REPO="${KRAM_RELEASES_REPO:-codexmark/kram-releases}"

usage() {
  cat <<'EOF'
Usage: scripts/release.sh vX.Y.Z [--notes FILE] [--yes]

Runs the full local release pipeline — gofmt/vet/test gate, cross-
platform build, checksums — then publishes a GitHub Release to the
public distribution repo.

  --notes FILE   Use FILE's contents as the release notes (defaults to
                 a minimal "Kram vX.Y.Z / Commit: <sha>" note).
  --yes          Skip the confirmation prompt before publishing.
EOF
}

VERSION=""
NOTES_FILE=""
ASSUME_YES=0

while [ $# -gt 0 ]; do
  case "$1" in
    --notes)
      [ $# -ge 2 ] || { echo "error: --notes requires a file argument" >&2; exit 1; }
      NOTES_FILE="$2"
      shift 2
      ;;
    --yes)
      ASSUME_YES=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo "error: unknown flag: $1" >&2
      usage >&2
      exit 1
      ;;
    *)
      if [ -n "$VERSION" ]; then
        echo "error: unexpected extra argument: $1" >&2
        exit 1
      fi
      VERSION="$1"
      shift
      ;;
  esac
done

if [ -z "$VERSION" ]; then
  echo "error: version argument required" >&2
  usage >&2
  exit 1
fi

# SemVer with a mandatory "v" prefix, allowing an optional pre-release/
# build suffix (v0.2.3-rc1, v0.2.3.1) — matches the tag naming this
# project has used since v0.1.0.
if ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-].*)?$ ]]; then
  echo "error: version '$VERSION' doesn't look like semver (expected vX.Y.Z, e.g. v0.2.3)" >&2
  exit 1
fi

echo "==> checking local tools"
for tool in gh git go; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "error: required tool '$tool' not found on PATH" >&2
    exit 1
  }
done
if ! gh auth status >/dev/null 2>&1; then
  echo "error: gh is not authenticated — run 'gh auth login' first" >&2
  exit 1
fi
GH_LOGIN="$(gh api user --jq .login)"
if [ "$GH_LOGIN" != "codexmark" ]; then
  echo "error: releases must be published as codexmark (gh is authenticated as '$GH_LOGIN')" >&2
  exit 1
fi
echo "  ok"

echo "==> checking git working tree"
if [ -n "$(git status --porcelain)" ]; then
  echo "Release abortado: existem alterações não commitadas." >&2
  git status --short >&2
  exit 1
fi
COMMIT="$(git rev-parse HEAD)"
echo "  publishing commit: $COMMIT"

echo "==> full local verification"
scripts/verify.sh

echo "==> building release artifacts"
scripts/build-release.sh "$VERSION"

CLEANUP_NOTES=0
if [ -z "$NOTES_FILE" ]; then
  NOTES_FILE="$(mktemp)"
  CLEANUP_NOTES=1
  printf 'Kram %s\n\nCommit: %s\n' "$VERSION" "$COMMIT" > "$NOTES_FILE"
fi
cleanup() {
  if [ "$CLEANUP_NOTES" -eq 1 ]; then
    rm -f "$NOTES_FILE"
  fi
}
trap cleanup EXIT

echo
echo "Version:       $VERSION"
echo "Source commit: $COMMIT"
echo "Target repo:   $RELEASES_REPO"
echo "Assets:"
# shellcheck disable=SC2012
ls dist | sed 's/^/  - /'
echo

if [ "$ASSUME_YES" -ne 1 ]; then
  read -r -p "Publish this release? [y/N] " reply
  case "$reply" in
    y|Y|yes|YES) ;;
    *)
      echo "aborted."
      exit 1
      ;;
  esac
fi

echo "==> creating draft release in $RELEASES_REPO"
gh release create "$VERSION" \
  dist/kram-linux-amd64.tar.gz \
  dist/kram-linux-arm64.tar.gz \
  dist/kram-darwin-amd64.tar.gz \
  dist/kram-darwin-arm64.tar.gz \
  dist/kram-windows-amd64.zip \
  dist/kram-android-arm64.tar.gz \
  dist/SHA256SUMS \
  --repo "$RELEASES_REPO" \
  --title "Kram $VERSION" \
  --notes-file "$NOTES_FILE" \
  --draft

echo "==> synchronizing public installer files"
RELEASES_BRANCH="$(gh api "repos/${RELEASES_REPO}" --jq .default_branch)"
publish_dist_file() {
  local source_path="$1"
  local destination_path="$2"
  local encoded current_sha
  local -a args
  encoded="$(base64 < "$source_path")"
  current_sha="$(gh api "repos/${RELEASES_REPO}/contents/${destination_path}?ref=${RELEASES_BRANCH}" --jq .sha 2>/dev/null || true)"

  args=(
    --method PUT
    "repos/${RELEASES_REPO}/contents/${destination_path}"
    -f "message=Update ${destination_path} for ${VERSION}"
    -f "content=${encoded}"
    -f "branch=${RELEASES_BRANCH}"
  )
  if [ -n "$current_sha" ]; then
    args+=(-f "sha=${current_sha}")
  fi
  gh api "${args[@]}" --silent
  echo "  -> ${destination_path}"
}

publish_dist_file scripts/dist-repo/install.sh install.sh
publish_dist_file scripts/dist-repo/install.ps1 install.ps1
publish_dist_file scripts/dist-repo/README.md README.md

echo "==> verifying public installer blobs"
verify_dist_file() {
  local source_path="$1"
  local destination_path="$2"
  local local_sha remote_sha
  local_sha="$(git hash-object "$source_path")"
  remote_sha="$(gh api "repos/${RELEASES_REPO}/contents/${destination_path}?ref=${RELEASES_BRANCH}" --jq .sha)"
  if [ "$local_sha" != "$remote_sha" ]; then
    echo "error: published ${destination_path} does not match ${source_path}; ${VERSION} remains a draft" >&2
    return 1
  fi
  echo "  ✓ ${destination_path}"
}

verify_dist_file scripts/dist-repo/install.sh install.sh
verify_dist_file scripts/dist-repo/install.ps1 install.ps1
verify_dist_file scripts/dist-repo/README.md README.md

echo "==> promoting verified release to latest"
gh release edit "$VERSION" --repo "$RELEASES_REPO" --draft=false --latest

echo
echo "✓ Release published: $VERSION"
echo
echo "Install with:"
echo "  curl -fsSL https://raw.githubusercontent.com/${RELEASES_REPO}/master/install.sh | sh"
echo "  irm https://raw.githubusercontent.com/${RELEASES_REPO}/master/install.ps1 | iex"
