<#
.SYNOPSIS
    Cross-compiles the cmd/ binaries for Linux and Windows into bin/.

.DESCRIPTION
    Builds every program under cmd/ (currently codereview and codereview-eval)
    for linux/amd64 and windows/amd64. Output layout:

        bin/linux/<name>
        bin/windows/<name>.exe

.EXAMPLE
    ./scripts/build.ps1
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

# Repo root is the parent of this script's directory.
$root = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $root 'bin'

# Each cmd/<name> directory with a main package becomes one binary.
$cmds = Get-ChildItem -Path (Join-Path $root 'cmd') -Directory |
    ForEach-Object { $_.Name }

$targets = @(
    @{ GOOS = 'linux';   GOARCH = 'amd64'; Ext = '' },
    @{ GOOS = 'windows'; GOARCH = 'amd64'; Ext = '.exe' }
)

foreach ($target in $targets) {
    $outDir = Join-Path $binDir $target.GOOS
    New-Item -ItemType Directory -Force -Path $outDir | Out-Null

    foreach ($cmd in $cmds) {
        $out = Join-Path $outDir ($cmd + $target.Ext)
        Write-Host "building $cmd -> $($target.GOOS)/$($target.GOARCH)"

        $env:GOOS = $target.GOOS
        $env:GOARCH = $target.GOARCH
        $env:CGO_ENABLED = '0'
        & go build -trimpath -o $out "./cmd/$cmd"
        if ($LASTEXITCODE -ne 0) {
            throw "build failed for $cmd ($($target.GOOS)/$($target.GOARCH))"
        }
    }
}

Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
Write-Host "done -> $binDir"