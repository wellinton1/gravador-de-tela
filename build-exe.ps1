# Builds recorder.exe with the native library embedded (single-file output).
#
# The DLL cannot be referenced by go:embed from bin/, so it is copied next to
# internal/native/embedded/embedded.go first; on the first run the program
# extracts it beside the exe (TEMP fallback when that folder is read-only)
# and loads it from there. Exe and DLL must always be rebuilt together:
# run build.ps1 first when the C++ sources changed.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File build-exe.ps1
#   powershell -ExecutionPolicy Bypass -File build-exe.ps1 -Go C:\go\bin\go.exe

param(
    [string]$Go = ''
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$dll = Join-Path $root 'bin\recorder_native.dll'
if (-not (Test-Path $dll)) {
    throw 'bin\recorder_native.dll not found; run build.ps1 first to compile it.'
}
$embDir = Join-Path $root 'internal\native\embedded'
New-Item -ItemType Directory -Force -Path $embDir | Out-Null
Copy-Item $dll (Join-Path $embDir 'recorder_native.dll') -Force
Write-Host "Embedded $(Join-Path $embDir 'recorder_native.dll')"

$uiEmbDir = Join-Path $root 'internal\ui\embedded'
New-Item -ItemType Directory -Force -Path $uiEmbDir | Out-Null
foreach ($name in @('icon-app.ico', 'icon-tray.ico', 'icon-rec.ico', 'bg.bmp')) {
    $src = Join-Path $root "assets\$name"
    if (-not (Test-Path $src)) {
        throw "assets\$name not found; run assets\make-icons.ps1 first to generate it."
    }
    Copy-Item $src (Join-Path $uiEmbDir $name) -Force
}
Write-Host "Embedded icons + background from assets\"

$goExe = $Go
if (-not $goExe) {
    $found = Get-Command go -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($found) { $goExe = $found.Source }
}
if (-not $goExe) {
    $tempGo = Join-Path ${env:TEMP} 'opencode\go\bin\go.exe'
    if (Test-Path $tempGo) { $goExe = $tempGo }
}
if (-not $goExe -or -not (Test-Path $goExe)) {
    throw 'go toolchain not found; install Go or pass -Go <path to go.exe>.'
}

Push-Location $root
try {
    # -H windowsgui: double-click opens only the program window (no black
    # console box). CLI commands still print via AttachConsole.
    & $goExe build -ldflags "-H windowsgui" -o (Join-Path $root 'bin\recorder.exe') ./cmd/recorder
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

Write-Host "Built $(Join-Path $root 'bin\recorder.exe') (single file, DLL embedded)"
