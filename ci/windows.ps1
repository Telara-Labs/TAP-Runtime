# ci/windows.ps1 - what the windows-amd64 job runs (TENG-3040).
#
# The suite drives Unix programs (tee, cat, grep), so on Windows the packages
# that touch no program are tested, the runner is built, and one primitive is
# run in the sandbox: it reads a declared file, writes a declared one, and is
# refused a write outside what it declared.
#
# Everything said goes to windows.log, which the job keeps, and to the
# machine's serial console, which outlives the job's own log for whoever has
# the cloud project and not the pipeline.

$ErrorActionPreference = "Stop"
$log = Join-Path (Get-Location) "windows.log"
Set-Content -Path $log -Value ""
$serial = $null
try {
  $serial = New-Object System.IO.Ports.SerialPort COM1, 9600, None, 8, One
  $serial.Open()
} catch { $serial = $null }

function Say($text) {
  Add-Content -Path $log -Value $text
  Write-Host $text
  if ($serial) { try { $serial.WriteLine("tapci: $text") } catch {} }
}

# Runs one program, says what it printed, and stops the job if it failed.
function Step($what, [scriptblock]$do) {
  Say "== $what"
  $ErrorActionPreference = "Continue"
  $out = & $do 2>&1 | Out-String
  $code = $LASTEXITCODE
  $ErrorActionPreference = "Stop"
  foreach ($line in ($out -split "`r?`n")) { if ($line -ne "") { Say "   $line" } }
  if ($code -ne 0) { Say "FAILED: $what (exit $code)"; throw "$what failed with exit $code" }
}

$failed = $false
try {
  $env:GOWORK = "off"
  $env:GOFLAGS = "-mod=readonly"
  # contract/ and discover/ come from the public module proxy (TENG-3180).

  Step "go version" { go version }
  Step "tests: bind, journal, satisfy" { go test ./bind ./journal ./satisfy -count=1 }
  Push-Location contract
  try { Step "tests: contract glob, manifest" { go test ./glob ./manifest -count=1 } } finally { Pop-Location }
  # discover reads local files only. Windows has no sqlite3 program, so its
  # store readers report themselves unavailable and their tests skip
  # (TENG-3171).
  Push-Location discover
  try { Step "tests: discover" { go test ./... -count=1 } } finally { Pop-Location }
  Step "build the runner" { go build -o tap.exe ./host }

  $env:GOOS = "wasip1"; $env:GOARCH = "wasm"
  try { Step "build the shell interpreter" { go build -o store/sh.wasm ./guest-sh } }
  finally { Remove-Item Env:GOOS; Remove-Item Env:GOARCH }

  New-Item -ItemType Directory -Force -Path work/in | Out-Null
  Set-Content -Path work/in/x.txt -Value "read-on-windows"
  Push-Location work
  try {
    Step "run a primitive in the sandbox" { ../tap.exe --interpreters ../store --runs ../runs --approve ../pkg/windows-smoke }
    if (-not (Test-Path out/y.txt) -or ((Get-Content out/y.txt) -notmatch "written-on-windows")) {
      Say "FAILED: the approved write did not happen"; throw "the approved write did not happen"
    }
    if (Test-Path ../escaped.txt) {
      Say "FAILED: a write left the declared directory"; throw "a write left the declared directory"
    }
    Say "== the primitive read what it declared, wrote what it declared, and nothing else"
  } finally { Pop-Location }
  Say "PASSED"
} catch {
  $failed = $true
  Say "ERROR: $($_.Exception.Message)"
} finally {
  if ($serial) { $serial.Close() }
}
if ($failed) { exit 1 }
