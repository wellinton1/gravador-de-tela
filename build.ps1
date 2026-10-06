# Builds the native capture library (recorder_native.dll).
#
# The C++ side owns everything low level: adapter detection, DXGI desktop
# duplication with a GDI fallback, Media Foundation encoding and WASAPI audio.
# Go loads the resulting DLL through the plain syscall package, so no cgo and no
# MinGW toolchain are required.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File build.ps1
#   powershell -ExecutionPolicy Bypass -File build.ps1 -Configuration Debug

param(
    [ValidateSet('Release', 'Debug')]
    [string]$Configuration = 'Release'
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$src = Join-Path $root 'native\src'
$inc = Join-Path $root 'native\include'
$out = Join-Path $root 'bin'
$obj = Join-Path $root 'bin\obj'

# Locate MSVC and the Windows SDK.
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
if (-not (Test-Path $vswhere)) {
    throw 'vswhere.exe not found; install Visual Studio 2022 (C++ desktop workload).'
}
$vsRoot = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if (-not $vsRoot) {
    throw 'No Visual Studio installation with the C++ toolset was found.'
}

$msvcRoot = Get-ChildItem (Join-Path $vsRoot 'VC\Tools\MSVC') -Directory |
    Sort-Object Name -Descending | Select-Object -First 1
if (-not $msvcRoot) { throw 'MSVC toolset not found.' }

$sdkRoot = Get-ChildItem (Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10\Include') -Directory |
    Where-Object { $_.Name -match '^\d+\.\d+\.\d+\.\d+$' } |
    Sort-Object Name -Descending | Select-Object -First 1
if (-not $sdkRoot) { throw 'Windows SDK not found.' }
$sdkVersion = $sdkRoot.Name
$sdkLib = Join-Path ${env:ProgramFiles(x86)} "Windows Kits\10\Lib\$sdkVersion"

$cl = Join-Path $msvcRoot.FullName 'bin\Hostx64\x64\cl.exe'
$lib = Join-Path $msvcRoot.FullName 'bin\Hostx64\x64\lib.exe'
if (-not (Test-Path $cl)) { throw "cl.exe not found under $($msvcRoot.FullName)" }

$arch = 'x64'
$libDir = Join-Path $sdkLib "um\$arch"
$ucrtLibDir = Join-Path $sdkLib "ucrt\$arch"
$umDir = Join-Path $sdkRoot.FullName 'um'
$ucrtDir = Join-Path $sdkRoot.FullName 'ucrt'
$sharedDir = Join-Path $sdkRoot.FullName 'shared'

New-Item -ItemType Directory -Force -Path $out, $obj | Out-Null

$flags = if ($Configuration -eq 'Release') {
    @('/O2', '/MD', '/GL')
} else {
    @('/Od', '/MDd', '/Zi')
}

# The library is a leaf DLL: it is loaded by Go and must not depend on a CRT the
# host lacks, so the objects are written into the current directory instead of
# through /Fo, whose trailing backslash cannot survive being quoted on a path that
# contains a space.
#
# Paths are turned into arguments first: an expression such as '/OUT:' + $path
# inside an array literal is split into two elements by PowerShell, which quietly
# hands cl an option with no value.
$dllPath = Join-Path $out 'recorder_native.dll'
$implibPath = Join-Path $obj 'recorder_native.lib'

$includes = @("/I$inc", "/I$($msvcRoot.FullName)\include", "/I$umDir", "/I$sharedDir",
    "/I$ucrtDir")
# The C++ runtime ships with the toolset rather than the SDK, so its library
# directory has to be on the link path as well.
$msvcLibDir = Join-Path $msvcRoot.FullName "lib\$arch"
$libPaths = @("/LIBPATH:$libDir", "/LIBPATH:$ucrtLibDir", "/LIBPATH:$msvcLibDir")
$linkOut = @("/OUT:$dllPath", "/IMPLIB:$implibPath")
$libs = @('dxgi.lib', 'd3d11.lib', 'dxguid.lib', 'windowscodecs.lib',
    'mf.lib', 'mfplat.lib', 'mfreadwrite.lib', 'mfuuid.lib',
    'avrt.lib', 'ole32.lib', 'oleaut32.lib', 'uuid.lib', 'user32.lib', 'gdi32.lib')

$common = @('/nologo', '/LD', '/EHsc', '/std:c++17', '/W3', '/WX',
    '/DWIN32_LEAN_AND_MEAN', '/DNOMINMAX', '/DUNICODE', '/D_UNICODE',
    '/D_CRT_SECURE_NO_WARNINGS', '/DRECORDER_BUILDING_DLL')

$link = @('/link', '/DLL', '/SUBSYSTEM:WINDOWS', '/MACHINE:X64') +
    $libPaths + $linkOut + $libs

$files = @(Get-ChildItem $src -Filter *.cpp | ForEach-Object { $_.FullName })

Write-Host "MSVC   : $($msvcRoot.FullName)"
Write-Host "SDK    : $sdkVersion"
Write-Host "Config : $Configuration"
Write-Host "Sources: $($files.Count)"

$commandLine = @($flags) + $common + $includes + $files + $link
Push-Location $obj
try {
    & $cl @commandLine
    $exitCode = $LASTEXITCODE
} finally {
    Pop-Location
}
if ($exitCode -ne 0) {
    throw "cl.exe failed with exit code $exitCode"
}

Write-Host "Built $(Join-Path $out 'recorder_native.dll')"
