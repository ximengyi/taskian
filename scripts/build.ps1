$ErrorActionPreference = 'Stop'

$repo = Split-Path -Parent $PSScriptRoot
$dist = Join-Path $repo 'dist'
New-Item -ItemType Directory -Path $dist -Force | Out-Null

$version = if ($env:TASKIAN_VERSION) { $env:TASKIAN_VERSION } else { 'dev' }
$ldflags = "-s -w -X main.version=$version"

Push-Location $repo
try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    go build -trimpath -ldflags $ldflags -o (Join-Path $dist 'taskian-windows-amd64.exe') ./cmd/taskian

    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'
    go build -trimpath -ldflags $ldflags -o (Join-Path $dist 'taskian-linux-amd64') ./cmd/taskian

    $env:GOOS = 'darwin'
    $env:GOARCH = 'amd64'
    go build -trimpath -ldflags $ldflags -o (Join-Path $dist 'taskian-darwin-amd64') ./cmd/taskian

    $env:GOARCH = 'arm64'
    go build -trimpath -ldflags $ldflags -o (Join-Path $dist 'taskian-darwin-arm64') ./cmd/taskian
} finally {
    Pop-Location
}

Get-ChildItem -LiteralPath $dist | Select-Object Name, Length, LastWriteTime
