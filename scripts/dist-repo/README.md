# Kram

## Install now / Instale agora mesmo

The installers download the correct prebuilt binary, verify it against the
release's `SHA256SUMS`, install it for the current user, and validate
`kram -version`. Go and Administrator/root access are not required.

Linux and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
kram
```

The first run walks you through a short setup wizard.

## Install a specific version

The variable has to go right before `sh`, not before `curl` — `VAR=x cmd1 | cmd2` only sets `VAR` for `cmd1` in POSIX shells, and `sh` is the one that actually reads it here.

```sh
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | KRAM_VERSION=v0.2.7 sh
```

## Custom install directory

Defaults to `$HOME/.local/bin`.

```sh
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | KRAM_INSTALL_DIR=/usr/local/bin sh
```

## Windows

Run from PowerShell on Windows amd64 (no Administrator shell required):

```powershell
irm https://raw.githubusercontent.com/codexmark/kram-releases/master/install.ps1 | iex
kram
```

To pin a version, make the environment variable visible to the script block:

```powershell
$env:KRAM_VERSION = "v0.2.7"
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/codexmark/kram-releases/master/install.ps1)))
```

The installer verifies `kram-windows-amd64.zip` against `SHA256SUMS`, installs under `%LOCALAPPDATA%\Programs\Kram` by default, updates the current user's `PATH` without duplication, and validates `kram -version` before finishing. Override the location with `KRAM_INSTALL_DIR`.

## Termux / Android arm64

Install the small runtime prerequisites, then use the normal installer:

```sh
pkg update
pkg install curl tar coreutils git
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
kram
```

Termux is detected conservatively and receives `kram-android-arm64.tar.gz`, installed to `$PREFIX/bin` by default. Git remains optional for basic use and recommended for snapshot features. Workspaces under `$HOME` are the supported baseline; Android shared storage has separate permission/filesystem semantics.

## What the installer does

Downloads the right binary for your OS/architecture, verifies its SHA-256 checksum against the release's own `SHA256SUMS`, and installs it. The Unix path does not use `sudo` or edit shell config; the Windows path updates only the current user's `PATH`.

## Source

This repository holds only the installer and release binaries. Kram's source is maintained separately.
