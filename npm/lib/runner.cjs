'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

function platformKey(platform = process.platform, arch = process.arch) {
  const normalizedPlatform = platform === 'win32' ? 'windows' : platform === 'darwin' ? 'darwin' : platform === 'linux' ? 'linux' : platform;
  const normalizedArch = arch === 'x64' || (normalizedPlatform === 'windows' && arch === 'arm64')
    ? 'amd64'
    : arch === 'aarch64' ? 'arm64' : arch;
  return `${normalizedPlatform}/${normalizedArch}`;
}

function resolveRunner(packageDir, platform = process.platform, arch = process.arch) {
  const key = platformKey(platform, arch);
  const manifest = JSON.parse(fs.readFileSync(path.join(packageDir, 'runner-assets.json'), 'utf8'));
  const asset = manifest.assets[key];
  if (!asset) {
    const supported = Object.keys(manifest.assets).sort().join(', ');
    throw new Error(`TAP has no prebuilt runner for ${key}. Prebuilt runners: ${supported}. ` +
      'On another platform, build it from source: go install github.com/Telara-Labs/TAP-Runtime/host@latest (it installs as host; rename it to tap).');
  }
  const runner = path.join(packageDir, 'assets', asset.file);
  const digest = crypto.createHash('sha256').update(fs.readFileSync(runner)).digest('hex');
  if (digest !== asset.sha256) throw new Error(`TAP runner ${asset.file} failed its SHA-256 check`);
  return runner;
}

// setup connects the runner to every agent installed here. The list of
// agents, and how each is connected, lives in the runner (discover's agent
// registry), not here: `tap install --client detected` skips the
// ones that are not installed and says what it did for each.
function setup(packageDir, { output = console.log, spawn = spawnSync } = {}) {
  const runner = resolveRunner(packageDir);
  const result = spawn(runner, ['install', '--client', 'detected'], { stdio: 'inherit' });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`TAP could not connect to every agent installed here (exit ${result.status})`);
  output('To connect another agent later: tap install --client <agent>');
  return result.status;
}

function run(argv, packageDir = path.resolve(__dirname, '..')) {
  const args = argv.slice();
  if (args[0] === 'setup') {
    try {
      setup(packageDir);
      return 0;
    } catch (error) {
      console.error(`tap setup: ${error.message}`);
      return 1;
    }
  }
  let runner;
  try {
    runner = resolveRunner(packageDir);
  } catch (error) {
    console.error(`tap: ${error.message}`);
    return 1;
  }
  const result = spawnSync(runner, args, { stdio: 'inherit' });
  if (result.error) {
    console.error(`tap: ${result.error.message}`);
    return 1;
  }
  return result.status === null ? 1 : result.status;
}

module.exports = { platformKey, resolveRunner, run, setup };
