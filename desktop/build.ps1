# Builds Apex RMM Desktop for Windows.
#
#   ./build.ps1 -Version 1.2.3                 # app + installer
#   ./build.ps1 -Version 1.2.3 -Step app       # dist/ApexRMM-amd64.exe, dist/ApexRMM-arm64.exe
#   ./build.ps1 -Version 1.2.3 -Step installer # dist/ApexRMM-Desktop-Setup-1.2.3.exe (from the exes in dist/)
#
# Code signing (OSSign) signs the two app exes between the "app" and "installer" steps, then the
# installer itself. See SIGNING.md.
param(
  [string]$Version = "0.0.0",
  [ValidateSet("all", "app", "installer")][string]$Step = "all"
)
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
$num = ($Version -split '[-+]')[0]   # Windows version resources need plain numbers
New-Item -ItemType Directory -Force build, dist | Out-Null

if ($Step -in "all", "app") {
  go run ./tools/genicon -out build/icon.ico
  if ($LASTEXITCODE) { throw "genicon failed" }

  go install github.com/tc-hib/go-winres@v0.3.3
  if ($LASTEXITCODE) { throw "installing go-winres failed" }
  $winres = Join-Path (go env GOPATH) "bin\go-winres.exe"
  & $winres simply --arch amd64,arm64 --icon build/icon.ico --manifest gui `
    --product-name "Apex RMM" --file-description "Apex RMM Desktop" `
    --product-version $num --file-version $num `
    --copyright "Copyright (c) 2026 Cade Hardy (Hardy Networks)" --original-filename "ApexRMM.exe"
  if ($LASTEXITCODE) { throw "go-winres failed" }

  foreach ($arch in "amd64", "arm64") {
    $env:GOOS = "windows"; $env:GOARCH = $arch; $env:CGO_ENABLED = "0"
    go build -trimpath -ldflags "-H windowsgui -s -w -X main.version=$Version" -o "dist/ApexRMM-$arch.exe" .
    if ($LASTEXITCODE) { throw "go build ($arch) failed" }
  }
  Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
  Get-ChildItem dist/ApexRMM-*.exe | Format-Table Name, Length
}

if ($Step -in "all", "installer") {
  $makensis = (Get-Command makensis -ErrorAction SilentlyContinue).Source
  if (-not $makensis) { $makensis = "${env:ProgramFiles(x86)}\NSIS\makensis.exe" }
  if (-not (Test-Path $makensis)) {
    Write-Host "Installing NSIS..."
    choco install nsis -y --no-progress | Out-Null
    $makensis = "${env:ProgramFiles(x86)}\NSIS\makensis.exe"
  }
  & $makensis /V2 "/DVERSION=$Version" "/DVERSIONNUM=$num" installer/apex.nsi
  if ($LASTEXITCODE) { throw "makensis failed" }
  Get-ChildItem dist/ApexRMM-Desktop-Setup-*.exe | Format-Table Name, Length
}
