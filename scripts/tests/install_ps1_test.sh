#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/scripts/dist-repo/install.ps1"

for required in \
  'kram-windows-amd64.zip' \
  'SHA256SUMS' \
  'Get-FileHash' \
  'Expand-Archive' \
  'LOCALAPPDATA' \
  'KRAM_INSTALL_DIR' \
  'SetEnvironmentVariable' \
  'KRAM_VERSION' \
  'kram.exe.new-'; do
  grep -Fq "$required" "$SCRIPT" || {
    echo "install.ps1 is missing required behavior marker: $required" >&2
    exit 1
  }
done

if grep -Eiq 'github[_-]?token|ghp_[A-Za-z0-9]|private[_-]?token' "$SCRIPT"; then
  echo "install.ps1 appears to contain a credential/token reference" >&2
  exit 1
fi

echo "install.ps1 structural checks passed"
