package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// installers writes the two scripts a person runs to install this release:
// install.sh for macOS and Linux, install.ps1 for Windows. Each carries the
// version and the digest of every runner, so what it installs is decided when
// the release is built and not when the script is run.
func installers(dir, version, base string, runners map[string]string) (map[string][]byte, error) {
	var platforms []string
	for p := range runners {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)
	var sh, ps strings.Builder
	for _, p := range platforms {
		raw, err := os.ReadFile(filepath.Join(dir, runners[p]))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		goos, goarch, _ := strings.Cut(p, "/")
		if goos == "windows" {
			fmt.Fprintf(&ps, "  '%s' = '%s'\n", goarch, hex.EncodeToString(sum[:]))
		} else {
			fmt.Fprintf(&sh, "  %s-%s) sum=%s ;;\n", goos, goarch, hex.EncodeToString(sum[:]))
		}
	}
	fill := strings.NewReplacer("@VERSION@", version, "@BASE@", base, "@SUMS@", strings.TrimRight(sh.String(), "\n"), "@PSSUMS@", strings.TrimRight(ps.String(), "\n"))
	out := map[string][]byte{}
	if sh.Len() > 0 {
		out["install.sh"] = []byte(fill.Replace(installSh))
	}
	if ps.Len() > 0 {
		out["install.ps1"] = []byte(fill.Replace(installPs1))
	}
	return out, nil
}

const installSh = `#!/bin/sh
# Installs tap @VERSION@ (the TAP runner) and connects it, as an MCP server, to
# every coding agent installed here (tap install --client detected).
#
#   curl -fsSL @BASE@/install.sh | sh
#   curl -fsSL @BASE@/install.sh | sh -s -- --client codex --dir /usr/local/bin
#
#   --client AGENTS|none         agents to connect (tap install --client takes the
#                                names), or none. Default: detected, every agent
#                                installed here.
#   --dir DIR                    where the program is put. Default: ~/.local/bin
#
# The program is checked against the digest written below before it is put
# anywhere. One that does not match is deleted and nothing is installed.
#
# Installation starts only after the final brace group is complete. A cut
# inside the function or its final invocation cannot run a partial command.
set -eu

main() {
version=@VERSION@
base=@BASE@
client=""
dir="$HOME/.local/bin"
while [ $# -gt 0 ]; do
  case "$1" in
    --client) client=$2; shift 2 ;;
    --dir) dir=$2; shift 2 ;;
    *) echo "install: $1 is not an option; see the top of this script" >&2; exit 2 ;;
  esac
done

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; esac
sum=""
case "$os-$arch" in
@SUMS@
esac
if [ -z "$sum" ]; then
  echo "install: this release has no program for $os on $arch" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  digest() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  digest() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  echo "install: neither sha256sum nor shasum is on this machine, so the download cannot be checked" >&2
  exit 1
fi

mkdir -p "$dir"
tmp=$(mktemp "$dir/.tap.XXXXXX")
trap 'rm -f "$tmp"' EXIT
file="tap-$version-$os-$arch"
echo "downloading $base/$file"
curl -fsSL -o "$tmp" "$base/$file"
got=$(digest "$tmp")
if [ "$got" != "$sum" ]; then
  echo "install: the download has sha256 $got and this release says $sum; nothing was installed" >&2
  exit 1
fi
chmod 755 "$tmp"
mv -f "$tmp" "$dir/tap"
trap - EXIT
echo "installed $("$dir/tap" version) at $dir/tap"

# The runner knows the agents and how each is connected.
if [ "$client" != none ]; then
  "$dir/tap" install --client "${client:-detected}"
  echo "To connect another agent later: $dir/tap install --client <agent>"
fi
# The runner was called tap-runtime before. Registering above pointed the
# clients at the new program, so the old one can go.
if [ "$client" != none ] && [ -e "$dir/tap-runtime" ]; then
  rm -f "$dir/tap-runtime"
  echo "removed the old tap-runtime program from $dir; the runner is now called tap"
fi
}

{
  main "$@"
}
`

const installPs1 = `# Installs tap @VERSION@ (the TAP runner) and registers it with Claude Code and Codex
# as an MCP server.
#
#   irm @BASE@/install.ps1 | iex
#
# To choose, save the script and run it:
#   ./install.ps1 -Client codex -Dir C:\tools
#
#   -Client AGENTS|none         agents to connect (tap install --client takes the
#                               names), or none. Default: detected, every agent
#                               installed here.
#   -Dir DIR                    where the program is put.
#                               Default: %LOCALAPPDATA%\Programs\tap
#
# The program is checked against the digest written below before it is put
# anywhere. One that does not match is deleted and nothing is installed.
param(
  [string]$Client = '',
  [string]$Dir = (Join-Path $env:LOCALAPPDATA 'Programs\tap')
)
$ErrorActionPreference = 'Stop'
$version = '@VERSION@'
$base = '@BASE@'
$sums = @{
@PSSUMS@
}
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$sum = $sums[$arch]
# Windows on ARM runs amd64 programs, and this release may hold no other.
if (-not $sum -and $arch -eq 'arm64') { $arch = 'amd64'; $sum = $sums[$arch] }
if (-not $sum) { throw "this release has no program for Windows on $arch" }

New-Item -ItemType Directory -Force -Path $Dir | Out-Null
$tmp = Join-Path $Dir '.tap.download'
$file = "tap-$version-windows-$arch.exe"
Write-Host "downloading $base/$file"
Invoke-WebRequest -UseBasicParsing -Uri "$base/$file" -OutFile $tmp
$got = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash.ToLower()
if ($got -ne $sum) {
  Remove-Item -Force $tmp
  throw "the download has sha256 $got and this release says $sum; nothing was installed"
}
$exe = Join-Path $Dir 'tap.exe'
Move-Item -Force $tmp $exe
Write-Host "installed $(& $exe version) at $exe"

# The runner knows the agents and how each is connected.
if ($Client -ne 'none') {
  $agents = if ($Client) { $Client } else { 'detected' }
  & $exe install --client $agents
  if ($LASTEXITCODE -ne 0) { throw "connecting the runner to $agents failed" }
  Write-Host "To connect another agent later: $exe install --client <agent>"
}
# The runner was called tap-runtime before. Registering above pointed the
# clients at the new program, so the old one can go.
if ($Client -ne 'none') {
  foreach ($old in (Join-Path $Dir 'tap-runtime.exe'), (Join-Path $env:LOCALAPPDATA 'Programs\tap-runtime\tap-runtime.exe')) {
    if (Test-Path $old) {
      Remove-Item -Force $old
      Write-Host "removed the old tap-runtime program at $old; the runner is now called tap"
    }
  }
}
`
