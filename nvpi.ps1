# Nvpi server launcher (headless + background)
# Usage:
#   .\run.ps1                  # foreground (interactive), default -auto -addr :8080
#   .\run.ps1 -Background      # detach into background, no window, logs to file
#   .\run.ps1 -Build           # rebuild binary first (go build ./cmd/serve)
#   .\run.ps1 -InstallChrome   # download Chrome for Testing headless-shell into bin/ (once)
#   .\run.ps1 -InstallStartup  # create .lnk in Windows Startup folder (auto-start on login)
#   .\run.ps1 -Stop            # kill any running nvpi-serve.exe / go-run instance
#   .\run.ps1 -ChromePathParam "C:\path\to\chrome.exe"  # override CHROME_PATH
#
# CHROME_PATH resolution order: -ChromePathParam > existing $env:CHROME_PATH > bin/chrome-headless-shell.exe.txt
# Run .\run.ps1 -InstallChrome first to populate the marker (lighter than full Chrome,
# ~30% less RAM; chromedp switches automatically once the marker exists).
#
# Env overrides (match what the Go code reads):
#   $env:CHROME_PROXY        = "socks5://127.0.0.1:7890"
#   $env:CHROME_PATH         = "C:\path\to\chrome.exe"   (or use -ChromePathParam)
#   $env:CHROMEDP_NO_SANDBOX = "1"

[CmdletBinding()]
param(
    [string]$Addr        = ":8080",
    [switch]$NoAuto,
    [switch]$Build,
    [switch]$Background,
    [switch]$InstallStartup,
    [switch]$InstallChrome,
    [string]$ChromePathParam = "D:\Nvpi\chrome-headless-shell-win64\chrome-headless-shell.exe",   # explicit override for $env:CHROME_PATH
    [switch]$Stop,
    [string]$Captcha     = "",
    [int]$PoolSize       = 3,
    [int]$PoolWorkers    = 1,
    [int]$MaxInflight    = 4,
    [int]$CoalesceMs     = 16,
    [string]$ChromeProxy = ""
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$logPath = Join-Path $root "nvpi.log"
$exeName = "nvpi-serve.exe"
$chromeMarker = Join-Path $root "bin\chrome-headless-shell.exe.txt"

# --- InstallChrome: download headless-shell once, then exit ---
if ($InstallChrome) {
    $dl = Join-Path $root "scripts\get-chrome-headless-shell.ps1"
    if (-not (Test-Path $dl)) { Write-Error "Missing: $dl"; exit 1 }
    & $dl
    return
}

# --- Resolve CHROME_PATH: explicit param > existing env > bin/ marker ---
if ($ChromePathParam -ne "") {
    $env:CHROME_PATH = $ChromePathParam
} elseif (-not $env:CHROME_PATH) {
    if (Test-Path $chromeMarker) {
        $marked = (Get-Content $chromeMarker -Raw).Trim()
        if ($marked -and (Test-Path $marked)) {
            $env:CHROME_PATH = $marked
        }
    }
}
$chromeDesc = if ($env:CHROME_PATH) { $env:CHROME_PATH } else { "<system Chrome / not set>" }

# --- Stop mode: kill running server and exit ---
if ($Stop) {
    Write-Host "Stopping Nvpi..." -ForegroundColor Yellow
    Get-Process -Name "nvpi-serve" -ErrorAction SilentlyContinue | Stop-Process -Force
    # also kill stray `go run` children holding the port
    $port = ($Addr -replace '^:', '') -replace '.*:', ''
    Get-NetTCPConnection -LocalPort $port -ErrorAction SilentlyContinue |
        ForEach-Object { Stop-Process -Id $_.OwningProcess -Force -ErrorAction SilentlyContinue }
    Write-Host "Stopped." -ForegroundColor Green
    return
}

# --- Ensure binary exists (build on demand) ---
$exePath = Join-Path $root $exeName
$needBuild = $Build -or (-not (Test-Path $exePath))
if ($needBuild) {
    Write-Host "Building binary..." -ForegroundColor Yellow
    & go build -o $exePath ./cmd/serve
    if ($LASTEXITCODE -ne 0) { Write-Error "go build failed"; exit 1 }
}

# Assemble arguments for the server binary
$cmdArgs = @(
    "-addr", $Addr,
    "-pool-size",    $PoolSize,
    "-pool-workers", $PoolWorkers,
    "-max-inflight", $MaxInflight,
    "-coalesce-ms",  $CoalesceMs
)
if (-not $NoAuto) {
    $cmdArgs += "-auto"
    if (-not $env:CHROME_PATH) {
        Write-Warning "CHROME_PATH not set: chromedp will look for system Chrome. For a lighter browser, run: .\run.ps1 -InstallChrome"
    }
} elseif ($Captcha -ne "") {
    $cmdArgs += @("-captcha", $Captcha)
} else {
    Write-Warning "Neither -auto nor -captcha set: each request must send nv-captcha-token"
}
if ($ChromeProxy -ne "") { $env:CHROME_PROXY = $ChromeProxy }

# --- InstallStartup: create a .lnk in the Startup folder, then exit ---
if ($InstallStartup) {
    $startup = [Environment]::GetFolderPath('Startup')
    $lnkPath = Join-Path $startup "Nvpi.lnk"
    $shell = New-Object -ComObject WScript.Shell
    $sc = $shell.CreateShortcut($lnkPath)
    $sc.TargetPath  = "powershell.exe"
    # windowStyle 7 = minimized; -WindowStyle Hidden is fully headless
    $sc.Arguments   = "-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$PSCommandPath`" -Background -Addr $Addr"
    if ($ChromeProxy) { $sc.Arguments += " -ChromeProxy `"$ChromeProxy`"" }
    $sc.WorkingDirectory = $root
    $sc.WindowStyle = 7
    $sc.Description = "Nvpi server (headless background)"
    $sc.IconLocation = "$exePath,0"
    $sc.Save()
    Write-Host "Installed startup shortcut:" -ForegroundColor Green
    Write-Host "  $lnkPath" -ForegroundColor Green
    Write-Host "Server will auto-start on login (window hidden, logging to $logPath)."
    return
}

# --- Background: detached, hidden window, redirect output to log ---
if ($Background) {
    Write-Host "Starting Nvpi in background (headless)..." -ForegroundColor Green
    Write-Host "  Endpoint: http://localhost$Addr"
    Write-Host "  Chrome:   $chromeDesc"
    Write-Host "  Log:      $logPath"
    Write-Host "  Stop with: .\run.ps1 -Stop"
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName               = $exePath
    $psi.Arguments              = $cmdArgs -join ' '
    $psi.WorkingDirectory       = $root
    $psi.UseShellExecute        = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError  = $true
    $psi.CreateNoWindow         = $true
    # Pipe env vars through to the child
    foreach ($k in @('CHROME_PROXY','CHROME_PATH','CHROMEDP_NO_SANDBOX','CHROMEDP_ALLOW_IMAGES')) {
        $val = [Environment]::GetEnvironmentVariable($k, 'Process')
        if ($val) { $psi.EnvironmentVariables[$k] = $val }
    }
    $p = New-Object System.Diagnostics.Process
    $p.StartInfo = $psi
    # Append output to nvpi.log
    $logStream = [System.IO.StreamWriter]::new($logPath, $true)
    $p.EnableRaisingEvents = $true
    $outAction = { if ($EventArgs.Data) { $logStream.WriteLine($EventArgs.Data); $logStream.Flush() } }
    Register-ObjectEvent -InputObject $p -EventName OutputDataReceived -Action $outAction | Out-Null
    Register-ObjectEvent -InputObject $p -EventName ErrorDataReceived  -Action $outAction | Out-Null
    [void]$p.Start()
    $p.BeginOutputReadLine()
    $p.BeginErrorReadLine()
    # Save PID so -Stop can target it precisely
    $p.Id | Out-File (Join-Path $root "nvpi.pid") -Encoding ascii
    Start-Sleep -Milliseconds 800
    if ($p.HasExited -and $p.ExitCode -ne 0) {
        $logStream.Close()
        Write-Error "Server failed to start (exit $($p.ExitCode)). See $logPath"
        exit 1
    }
    Write-Host "Started PID $($p.Id)." -ForegroundColor Green
    # Detach: do NOT wait. The detached process keeps running after this script exits.
    return
}

# --- Foreground (interactive): build args, run directly in current window ---
Write-Host "Starting: $exePath $($cmdArgs -join ' ')" -ForegroundColor Green
Write-Host "Chrome:   $chromeDesc"
Write-Host "Endpoint: http://localhost$Addr"
Write-Host "Models:   GET http://localhost$Addr/v1/models"
Write-Host "Health:   GET http://localhost$Addr/healthz"
& $exePath @cmdArgs
