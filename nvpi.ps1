<#
.SYNOPSIS
    nvpi.ps1 — Build (if needed) and run nvpi-serve.exe.
    Place in repo root. Called by PowerShell `nvpi` function or directly.

.DESCRIPTION
    Auto-rebuilds nvpi-serve.exe when any source file is newer than the binary,
    then executes it with all passed arguments.

.EXAMPLE
    nvpi -addr :8080 -auto
    nvpi -captcha "P1_..." -pool-size 4
#>

[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments=$true)]
    [string[]]$Args
)

# Resolve repo root (where this script lives)
$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Definition
$exePath  = Join-Path $repoRoot "nvpi-serve.exe"

# Determine if we need to rebuild
$needBuild = $false
if (-not (Test-Path $exePath)) {
    $needBuild = $true
}
else {
    $exeTime = (Get-Item $exePath).LastWriteTimeUtc

    # Source directories / files to watch
    $cmdSrc     = @(Get-ChildItem -Path (Join-Path $repoRoot "cmd")      -Recurse -Filter "*.go" -ErrorAction SilentlyContinue)
    $internalSrc = @(Get-ChildItem -Path (Join-Path $repoRoot "internal") -Recurse -Filter "*.go" -ErrorAction SilentlyContinue)
    $typesGo     = Get-Item (Join-Path $repoRoot "types.go")  -ErrorAction SilentlyContinue
    $clientGo    = Get-Item (Join-Path $repoRoot "client.go") -ErrorAction SilentlyContinue
    $thisScript  = Get-Item (Join-Path $repoRoot "nvpi.ps1")  -ErrorAction SilentlyContinue
    $sources = @($cmdSrc + $internalSrc + $typesGo + $clientGo + $thisScript) | Where-Object { $_ }

    foreach ($src in $sources) {
        if ($src.LastWriteTimeUtc -gt $exeTime) {
            $needBuild = $true
            break
        }
    }
}

if ($needBuild) {
    Write-Host "build: nvpi-serve.exe (sources updated or missing)" -ForegroundColor Cyan
    $build = Start-Process -FilePath "go" -ArgumentList "build -o $exePath ./cmd/serve" `
        -WorkingDirectory $repoRoot -Wait -PassThru
    if ($build.ExitCode -ne 0) {
        throw "go build failed with exit code $($build.ExitCode)"
    }
}

# Execute the binary with all args
& $exePath @Args