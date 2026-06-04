# Dew installer for Windows
# Usage: irm https://dewvm.dev/install.ps1 | iex

$ErrorActionPreference = "Stop"

$repo = "solcreek/dew"
$dest = "$env:LOCALAPPDATA\dew"

# Pick the binary that matches the host CPU. Windows on ARM runs the
# wrong-arch binary fine via x86_64 emulation, but every dew operation
# shells out to wsl.exe; emulation around that hot path is wasted
# overhead and on ARM-only Windows SKUs it just silently misbehaves.
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "ARM64" { "arm64" }
    "AMD64" { "x86_64" }
    default { throw "dew: unsupported CPU '$env:PROCESSOR_ARCHITECTURE' (need ARM64 or AMD64)" }
}
$binary = "dew-windows-$arch.exe"
$url = "https://github.com/$repo/releases/latest/download/$binary"

Write-Host "dew: downloading $binary..." -ForegroundColor Cyan

New-Item -ItemType Directory -Force -Path $dest | Out-Null
Invoke-WebRequest -Uri $url -OutFile "$dest\dew.exe" -UseBasicParsing

# Add to PATH if not already
$path = [Environment]::GetEnvironmentVariable("Path", "User")
if ($path -notlike "*$dest*") {
    [Environment]::SetEnvironmentVariable("Path", "$path;$dest", "User")
    Write-Host "dew: added $dest to PATH" -ForegroundColor Green
}

Write-Host ""
Write-Host "dew: installed to $dest\dew.exe" -ForegroundColor Green
Write-Host "dew: restart your terminal, then run: dew --help" -ForegroundColor Yellow
Write-Host ""
Write-Host "dew: to upgrade later, re-run:" -ForegroundColor DarkGray
Write-Host "       irm https://dewvm.dev/install.ps1 | iex" -ForegroundColor DarkGray
Write-Host ""
