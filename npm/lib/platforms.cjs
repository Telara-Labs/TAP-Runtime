'use strict';

// The platforms TAP ships a prebuilt runner for. Each has its own npm package
// so an install downloads only the runner for the machine it runs on. The
// package names and the os and cpu fields use npm's own words (darwin, linux,
// win32; arm64, x64); key is how the release names the same runner.
const platforms = [
  { key: 'darwin/arm64', os: 'darwin', cpu: ['arm64'], suffix: 'darwin-arm64', releaseName: 'darwin-arm64', binary: 'bin/tap' },
  { key: 'darwin/amd64', os: 'darwin', cpu: ['x64'], suffix: 'darwin-x64', releaseName: 'darwin-amd64', binary: 'bin/tap' },
  { key: 'linux/arm64', os: 'linux', cpu: ['arm64'], suffix: 'linux-arm64', releaseName: 'linux-arm64', binary: 'bin/tap' },
  { key: 'linux/amd64', os: 'linux', cpu: ['x64'], suffix: 'linux-x64', releaseName: 'linux-amd64', binary: 'bin/tap' },
  // Windows on ARM runs the x64 runner under emulation, so npm must not skip
  // this package there.
  { key: 'windows/amd64', os: 'win32', cpu: ['x64', 'arm64'], suffix: 'win32-x64', releaseName: 'windows-amd64.exe', binary: 'bin/tap.exe' },
].map((p) => ({ ...p, name: `@telaralabs/tap-${p.suffix}` }));

const mainPackage = '@telaralabs/tap';

module.exports = { mainPackage, platforms };
