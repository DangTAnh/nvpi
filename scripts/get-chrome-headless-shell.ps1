# Download Chrome for Testing headless-shell (Windows x64) into ./bin/.
# Lighter than full Chrome (~30% less RAM); chromedp reads it via $env:CHROME_PATH.
#
# Usage:
#   .\scripts\get-chrome-headless-shell.ps1            # download if missing
#   .\scripts\get-chrome-headless-shell.ps1 -Force      # re-download even if present
#   .\scripts\get-chrome-headless-shell.ps1 -Proxy http://host:port
#
# Idempotent: skips download if bin/chrome-headless-shell.exe already exists and -Force not set.
# Writes the absolute binary path to bin/chrome-headless-shell.exe.txt so run.ps1 can pick it up.

[CmdletBinding()]
param(
    [switch]$Force,
    [string]$Proxy = ""
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$binDir     = Join-Path $root "bin"
$markerFile = Join-Path $binDir "chrome-headless-shell.exe.txt"

# Resolve the binary path recorded in the marker file (if present).
function Get-RecordedExe {
    if (Test-Path $markerFile) {
        $p = (Get-Content $markerFile -Raw).Trim()
        if ($p -and (Test-Path $p)) { return $p }
    }
    # Fallback: glob the versioned dir.
    $exe = Get-ChildItem -Path $binDir -Recurse -Filter "chrome-headless-shell.exe" -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($exe) { return $exe.FullName }
    return $null
}

$existing = Get-RecordedExe
if ($existing -and -not $Force) {
    Write-Host "Existing headless-shell found:" -ForegroundColor Green
    Write-Host "  $existing"
    Write-Host "Marker: $markerFile"
    Write-Host "Use -Force to re-download." -ForegroundColor DarkGray
    return
}

New-Item -ItemType Directory -Force -Path $binDir | Out-Null

# Fetch the last-known-good versions manifest from the official endpoint.
$versionsUrl = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions.json"
Write-Host "Fetching version manifest: $versionsUrl" -ForegroundColor Cyan

$invokeParams = @{ Uri = $versionsUrl; UseBasicParsing = $true }
if ($Proxy) { $invokeParams.Proxy = $Proxy }
try {
    $resp = Invoke-RestMethod @invokeParams
} catch {
    Write-Error "Failed to fetch manifest: $($_.Exception.Message)`nSet -Proxy or download manually from https://googlechromelabs.github.io/chrome-for-testing/"
    exit 1
}

# Stable channel -> chrome-headless-shell -> win64.
$stable = $resp.channels.Stable
$version = $stable.version
$downloads = $stable.downloads.'chrome-headless-shell'
$entry = $downloads | Where-Object { $_.platform -eq "win64" } | Select-Object -First 1
if (-not $entry) {
    Write-Error "No win64 chrome-headless-shell entry for version $version"
    exit 1
}
$zipUrl = $entry.url
Write-Host "Version: $version"
Write-Host "URL:     $zipUrl"

$zipPath  = Join-Path $binDir "chrome-headless-shell-$version.zip"
$destDir  = Join-Path $binDir "chrome-headless-shell-$version"

# Download.
Write-Host "Downloading..." -ForegroundColor Cyan
$dlParams = @{ Uri = $zipUrl; OutFile = $zipPath; UseBasicParsing = $true }
if ($Proxy) { $dlParams.Proxy = $Proxy }
try {
    Invoke-WebRequest @dlParams
} catch {
    Write-Error "Download failed: $($_.Exception.Message)"
    exit 1
}

# Extract.
if (Test-Path $destDir) { Remove-Item $destDir -Recurse -Force }
Write-Host "Extracting -> $destDir" -ForegroundColor Cyan
Expand-Archive -Path $zipPath -DestinationPath $destDir -Force
Remove-Item $zipPath -Force

# The zip extracts a top folder named "chrome-headless-shell" containing the exe.
$exe = Get-ChildItem -Path $destDir -Recurse -Filter "chrome-headless-shell.exe" | Select-Object -First 1
if (-not $exe) {
    Write-Error "Extraction finished but chrome-headless-shell.exe not found under $destDir"
    exit 1
}

# Write marker file with absolute path.
$exe.FullName | Out-File $markerFile -Encoding ascii -NoNewline

Write-Host "Done." -ForegroundColor Green
Write-Host "  Binary: $($exe.FullName)"
Write-Host "  Marker: $markerFile"
Write-Host ""
Write-Host "Next: .\run.ps1 -Background   (auto-detects the marker)" -ForegroundColor Green
