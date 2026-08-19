# Kram

Install:

```sh
curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
```

Then:

```sh
kram
```

The first run walks you through a short setup wizard.

## Install a specific version

```sh
KRAM_VERSION=v0.2.3 curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
```

## Custom install directory

Defaults to `$HOME/.local/bin`.

```sh
KRAM_INSTALL_DIR=/usr/local/bin curl -fsSL https://raw.githubusercontent.com/codexmark/kram-releases/master/install.sh | sh
```

## Windows

`install.sh` covers Linux and macOS. On Windows, download `kram-windows-amd64.zip` from the [latest release](https://github.com/codexmark/kram-releases/releases/latest) and extract `kram.exe` somewhere on your `PATH`.

## What the installer does

Downloads the right binary for your OS/architecture, verifies its SHA-256 checksum against the release's own `SHA256SUMS`, and installs it — no `sudo`, no shell config edited automatically, no dependency beyond `curl`, `tar`, and a `sha256sum`/`shasum` your system almost certainly already has.

## Source

This repository holds only the installer and release binaries. Kram's source is maintained separately.
