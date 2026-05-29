# Dew installer for Windows
# Usage: irm https://github.com/solcreek/dew/releases/latest/download/install.ps1 | iex

$ErrorActionPreference = "Stop"

$repo = "solcreek/dew"
$binary = "dew-windows-x86_64.exe"
$dest = "$env:LOCALAPPDATA\dew"
$url = "https://github.com/$repo/releases/latest/download/$binary"

Write-Host "dew: downloading..." -ForegroundColor Cyan

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
