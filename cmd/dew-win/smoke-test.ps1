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

# --- up: dev server reachable on Windows localhost -----------------
Write-Host "== up: node dev server on localhost ==" -ForegroundColor Cyan
$proj = Join-Path $env:TEMP 'dew-smoke-proj'
New-Item -ItemType Directory -Force -Path $proj | Out-Null
Set-Content -Path (Join-Path $proj 'package.json') -Encoding ascii -Value '{"name":"smoke","private":true,"scripts":{"dev":"node server.js"}}'
Set-Content -Path (Join-Path $proj 'server.js') -Encoding ascii -Value @'
const http = require('http');
http.createServer((_, res) => res.end('smoke-ok')).listen(5199, '0.0.0.0');
'@
$log = Join-Path $env:TEMP 'dew-smoke-up.log'
Remove-Item $log -ErrorAction SilentlyContinue
# curl.exe, not Invoke-WebRequest: IWR is unreliable against WSL2's
# mirrored localhost on some hosts (resolves ::1, hangs), while the
# bundled curl.exe probes 127.0.0.1 cleanly.
$up = Start-Process $dew -ArgumentList "up `"$proj`"" -RedirectStandardOutput $log -RedirectStandardError "$log.err" -PassThru -NoNewWindow
$body = ''
foreach ($i in 1..30) {
    Start-Sleep -Seconds 1
    $body = (& curl.exe -s --max-time 3 http://localhost:5199) 2>$null
    if ($body -eq 'smoke-ok') { break }
}
Check "dev server reachable at localhost:5199" { $body -eq 'smoke-ok' }
if ($up -and -not $up.HasExited) { $up.Kill() }
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
