$ErrorActionPreference = "Stop"

# Kram installer for Windows amd64. It uses only public release assets,
# verifies SHA-256 before touching an existing installation, and installs for
# the current user without Administrator privileges.
$Repository = if ($env:KRAM_RELEASES_REPO) { $env:KRAM_RELEASES_REPO } else { "codexmark/kram-releases" }
$RequestedVersion = $env:KRAM_VERSION
$Asset = "kram-windows-amd64.zip"

$Architecture = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
if ($Architecture -ne "AMD64") {
    throw "Kram currently supports Windows amd64 only (detected: $Architecture)"
}

$InstallDir = if ($env:KRAM_INSTALL_DIR) {
    $env:KRAM_INSTALL_DIR
} else {
    Join-Path $env:LOCALAPPDATA "Programs\Kram"
}

$BaseUrl = if ($env:KRAM_BASE_URL) {
    $env:KRAM_BASE_URL.TrimEnd("/")
} elseif ($RequestedVersion) {
    "https://github.com/$Repository/releases/download/$RequestedVersion"
} else {
    "https://github.com/$Repository/releases/latest/download"
}

$TempDir = Join-Path ([IO.Path]::GetTempPath()) ("kram-install-" + [Guid]::NewGuid().ToString("N"))
$ZipPath = Join-Path $TempDir $Asset
$SumsPath = Join-Path $TempDir "SHA256SUMS"
$ExtractDir = Join-Path $TempDir "extracted"
$Staged = $null

try {
    New-Item -ItemType Directory -Path $TempDir -Force | Out-Null
    Write-Host "Kram installer"
    Write-Host "Platform: windows/amd64"
    Write-Host ("Version:  " + $(if ($RequestedVersion) { $RequestedVersion } else { "latest" }))
    Write-Host "Downloading..."
    Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/$Asset" -OutFile $ZipPath
    Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/SHA256SUMS" -OutFile $SumsPath

    Write-Host "Verifying SHA-256..."
    $Expected = $null
    foreach ($Line in Get-Content -LiteralPath $SumsPath) {
        if ($Line -match "^([0-9a-fA-F]{64})\s+\*?kram-windows-amd64\.zip$") {
            $Expected = $Matches[1].ToLowerInvariant()
            break
        }
    }
    if (-not $Expected) {
        throw "SHA256SUMS has no checksum entry for $Asset"
    }
    $Actual = (Get-FileHash -LiteralPath $ZipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($Actual -ne $Expected) {
        throw "checksum mismatch for $Asset; existing installation was not touched"
    }

    New-Item -ItemType Directory -Path $ExtractDir -Force | Out-Null
    Expand-Archive -LiteralPath $ZipPath -DestinationPath $ExtractDir -Force
    $ExtractedExe = Join-Path $ExtractDir "kram.exe"
    if (-not (Test-Path -LiteralPath $ExtractedExe -PathType Leaf)) {
        throw "archive did not contain kram.exe"
    }

    # Validate the candidate before replacing a working installation.
    $CandidateVersion = & $ExtractedExe -version
    if ($LASTEXITCODE -ne 0 -or -not $CandidateVersion) {
        throw "downloaded kram.exe failed its version self-check"
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $Destination = Join-Path $InstallDir "kram.exe"
    $Staged = Join-Path $InstallDir ("kram.exe.new-" + [Guid]::NewGuid().ToString("N"))
    Copy-Item -LiteralPath $ExtractedExe -Destination $Staged
    Move-Item -LiteralPath $Staged -Destination $Destination -Force

    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $Entries = @($UserPath -split ";" | Where-Object { $_ })
    $AlreadyPresent = $Entries | Where-Object { $_.TrimEnd("\") -ieq $InstallDir.TrimEnd("\") }
    if (-not $AlreadyPresent) {
        $NewUserPath = if ($UserPath) { "$UserPath;$InstallDir" } else { $InstallDir }
        [Environment]::SetEnvironmentVariable("Path", $NewUserPath, "User")
    }
    $SessionEntries = @($env:Path -split ";" | Where-Object { $_ })
    if (-not ($SessionEntries | Where-Object { $_.TrimEnd("\") -ieq $InstallDir.TrimEnd("\") })) {
        $env:Path = "$env:Path;$InstallDir"
    }

    $InstalledVersion = & $Destination -version
    if ($LASTEXITCODE -ne 0 -or -not $InstalledVersion) {
        throw "installed kram.exe failed its version self-check"
    }
    Write-Host ""
    Write-Host "$InstalledVersion installed successfully at $Destination"
} finally {
    if ($Staged -and (Test-Path -LiteralPath $Staged)) {
        Remove-Item -LiteralPath $Staged -Force -ErrorAction SilentlyContinue
    }
    if (Test-Path -LiteralPath $TempDir) {
        Remove-Item -LiteralPath $TempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
