#requires -Version 5.1
<#
.SYNOPSIS
  Real-machine smoke test for dew-win (the Windows/WSL2 wrapper).

.DESCRIPTION
  Builds dew.exe from this repo, then exercises the full command
  surface against a live WSL2 distro and asserts behaviour that unit
  tests can't reach: argv fidelity through wsl --exec, exit-code
  propagation, and vm status that doesn't start the distro.

  Run on the Windows box (needs Go + WSL2). Downloads the ~35MB rootfs
  on first run via `dew setup`; reuses the distro on later runs.

    powershell -ExecutionPolicy Bypass -File cmd\dew-win\smoke-test.ps1

  Exits 0 if every check passes, 1 otherwise.
#>

$ErrorActionPreference = 'Stop'
$env:WSL_UTF8 = '1'

$script:Failures = 0
function Check([string]$name, [scriptblock]$cond) {
    $ok = $false
    try { $ok = & $cond } catch { $ok = $false }
    if ($ok) {
        Write-Host "  PASS  $name" -ForegroundColor Green
    } else {
        Write-Host "  FAIL  $name" -ForegroundColor Red
        $script:Failures++
    }
}

# --- Build ---------------------------------------------------------
$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$dew = Join-Path $env:TEMP 'dew-smoke.exe'
Write-Host "== building dew.exe ==" -ForegroundColor Cyan
Push-Location $repo
try { go build -o $dew ./cmd/dew-win/ } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { Write-Host "build failed" -ForegroundColor Red; exit 1 }

# --- Setup (idempotent) --------------------------------------------
Write-Host "== ensuring distro (dew setup) ==" -ForegroundColor Cyan
& $dew setup
if ($LASTEXITCODE -ne 0) { Write-Host "setup failed" -ForegroundColor Red; exit 1 }

# Build/setup are done - from here a stray non-terminating error must
# not abort the run before the result summary. Individual checks catch
# their own errors; this just guarantees we always reach the tally.
$ErrorActionPreference = 'Continue'

# --- exec: argv fidelity (the --exec fix) --------------------------
Write-Host "== exec argv fidelity ==" -ForegroundColor Cyan
Check "spaces + metachars preserved" {
    (& $dew exec printf '[%s]|' a 'b c' d) -join "`n" -eq '[a]|[b c]|[d]|'
}
Check "pipe stays literal (no shell)" {
    (& $dew exec printf '%s' 'a|b') -eq 'a|b'
}
Check "shell var expands in inner sh only" {
    (& $dew exec sh -c 'for t in x y z; do printf "%s," $t; done') -eq 'x,y,z,'
}

# --- exec: exit-code propagation -----------------------------------
Write-Host "== exec exit-code propagation ==" -ForegroundColor Cyan
Check "exit 0 propagates" {
    & $dew exec true | Out-Null; $LASTEXITCODE -eq 0
}
Check "exit 42 propagates" {
    & $dew exec sh -c 'exit 42' | Out-Null; $LASTEXITCODE -eq 42
}
Check "non-zero exit is silent (no dew: prefix)" {
    $err = (& $dew exec sh -c 'exit 3') 2>&1
    ($err | Out-String) -notmatch 'dew: exit status'
}

# --- vm status: no observer effect ---------------------------------
Write-Host "== vm status (must not start the distro) ==" -ForegroundColor Cyan
wsl --terminate dew 2>&1 | Out-Null
Check "stopped distro reports stopped" {
    (& $dew vm status) -match 'stopped'
}
& $dew exec true | Out-Null   # starts it
Check "running distro reports running" {
    (& $dew vm status) -match 'running'
}
wsl --terminate dew 2>&1 | Out-Null
Check "terminated distro reports stopped again" {
    (& $dew vm status) -match 'stopped'
}

# --- command surface: run / vm list / doctor / env ----------------
Write-Host "== command surface: run / vm list / doctor / env ==" -ForegroundColor Cyan
Check "run strips -- and execs the command" {
    (& $dew run -- printf 'ok') -eq 'ok'
}
Check "vm list shows the dew distro" {
    (& $dew vm list | Out-String) -match 'dew'
}
Check "env reports the distro name" {
    (& $dew env | Out-String) -match 'distro\s+dew'
}
Check "doctor passes on a set-up box" {
    & $dew doctor | Out-Null; $LASTEXITCODE -eq 0
}

# --- up: dev server reachable on Windows localhost -----------------
Write-Host "== up: node dev server on localhost ==" -ForegroundColor Cyan
# Ask the OS for a free port (bind :0, read it back, release) so a busy
# fixed port can't fail an otherwise-healthy run.
$listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
$listener.Start()
$port = ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
$listener.Stop()

$proj = Join-Path $env:TEMP 'dew-smoke-proj'
New-Item -ItemType Directory -Force -Path $proj | Out-Null
Set-Content -Path (Join-Path $proj 'package.json') -Encoding ascii -Value '{"name":"smoke","private":true,"scripts":{"dev":"node server.js"}}'
Set-Content -Path (Join-Path $proj 'server.js') -Encoding ascii -Value @"
const http = require('http');
http.createServer((_, res) => res.end('smoke-ok')).listen($port, '0.0.0.0');
"@
$log = Join-Path $env:TEMP 'dew-smoke-up.log'
Remove-Item $log -ErrorAction SilentlyContinue
# curl.exe, not Invoke-WebRequest: IWR is unreliable against WSL2's
# mirrored localhost on some hosts (resolves ::1, hangs), while the
# bundled curl.exe probes 127.0.0.1 cleanly.
$up = Start-Process $dew -ArgumentList "up `"$proj`"" -RedirectStandardOutput $log -RedirectStandardError "$log.err" -PassThru -NoNewWindow
$body = ''
foreach ($i in 1..30) {
    Start-Sleep -Seconds 1
    $body = (& curl.exe -s --max-time 3 "http://localhost:$port") 2>$null
    if ($body -eq 'smoke-ok') { break }
}
Check "dev server reachable at localhost:$port" { $body -eq 'smoke-ok' }
if ($up -and -not $up.HasExited) { $up.Kill() }
wsl --terminate dew 2>&1 | Out-Null

# --- up --with: service starts, is reachable, and is cleaned up ----
Write-Host "== up --with: service lifecycle ==" -ForegroundColor Cyan
$svcProj = Join-Path $env:TEMP 'dew-smoke-svc'
New-Item -ItemType Directory -Force -Path $svcProj | Out-Null
# dev script that self-exits after ~15s so the cleanup path (dev exits ->
# stop()) runs on its own without needing a Ctrl+C we can't send here.
Set-Content -Path (Join-Path $svcProj 'package.json') -Encoding ascii -Value '{"name":"svc","private":true,"scripts":{"dev":"node -e \"setTimeout(()=>process.exit(0),15000)\""}}'
$svcLog = Join-Path $env:TEMP 'dew-smoke-svc.log'
Remove-Item $svcLog -ErrorAction SilentlyContinue
$svc = Start-Process $dew -ArgumentList "up --with redis `"$svcProj`"" -RedirectStandardOutput $svcLog -RedirectStandardError "$svcLog.err" -PassThru -NoNewWindow
$svcReady = $false
foreach ($i in 1..90) {
    Start-Sleep -Seconds 1
    if ((Get-Content $svcLog -Raw -ErrorAction SilentlyContinue) -match 'redis ready') { $svcReady = $true; break }
    if ($svc.HasExited) { break }
}
Check "up --with redis reports the service ready" { $svcReady }
Check "redis reachable on Windows localhost:6379" {
    (Test-NetConnection -ComputerName 127.0.0.1 -Port 6379 -WarningAction SilentlyContinue).TcpTestSucceeded
}
Check "service container is running" {
    (wsl -d dew -e podman ps --format '{{.Names}}' | Out-String) -match 'dew-svc-redis'
}
# Let the dev server self-exit so stop() removes the container, then verify.
# If it overruns the wait, kill it so we don't leak a running dew up into
# later checks.
if (-not $svc.HasExited) { $svc.WaitForExit(25000) | Out-Null }
if (-not $svc.HasExited) { $svc.Kill() }
Start-Sleep -Seconds 2
Check "service container removed after dev server exits" {
    -not ((wsl -d dew -e podman ps -a --format '{{.Names}}' | Out-String) -match 'dew-svc-redis')
}
wsl --terminate dew 2>&1 | Out-Null

# --- Result --------------------------------------------------------
Write-Host ""
if ($script:Failures -eq 0) {
    Write-Host "SMOKE OK - all checks passed" -ForegroundColor Green
    exit 0
} else {
    Write-Host "SMOKE FAILED - $($script:Failures) check(s) failed" -ForegroundColor Red
    exit 1
}
