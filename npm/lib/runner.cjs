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
// registry), not here: native `tap setup` connects detected clients and
// reconciles saved primitive skill pointers. It skips the
// ones that are not installed and says what it did for each.
function setup(packageDir, { output = console.log, spawn = spawnSync } = {}) {
  const runner = resolveRunner(packageDir);
  const result = spawn(runner, ['setup'], { stdio: 'inherit' });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`TAP could not connect to every agent installed here (exit ${result.status})`);
  output('To connect another agent later and refresh saved primitive skills: tap setup');
  return result.status;
}

function newer(a, b) {
  const pa = a.split('.').map(Number);
  const pb = b.split('.').map(Number);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    if ((pa[i] || 0) !== (pb[i] || 0)) return (pa[i] || 0) > (pb[i] || 0);
  }
  return false;
}

// upgrade replaces the global install with the newest published version, then
// runs the new runner's setup itself: npm hides the postinstall output and may
// skip install scripts, so agents would otherwise keep the old runner.
function upgrade(packageDir, { output = console.log, spawn = spawnSync } = {}) {
  const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
  const shell = process.platform === 'win32';
  const current = JSON.parse(fs.readFileSync(path.join(packageDir, 'package.json'), 'utf8')).version;
  const view = spawn(npm, ['view', '@telaralabs/tap', 'version'], { encoding: 'utf8', shell });
  if (view.error) throw view.error;
  if (view.status !== 0) throw new Error(`could not read the latest version from npm (exit ${view.status})`);
  const latest = String(view.stdout).trim();
  if (!newer(latest, current)) {
    output(`tap ${current} is the latest version.`);
    return 0;
  }
  output(`Upgrading tap ${current} to ${latest}`);
  const install = spawn(npm, ['install', '--global', `@telaralabs/tap@${latest}`], { stdio: 'inherit', shell });
  if (install.error) throw install.error;
  if (install.status !== 0) throw new Error(`npm install failed (exit ${install.status})`);
  const root = spawn(npm, ['root', '--global'], { encoding: 'utf8', shell });
  if (root.error) throw root.error;
  if (root.status !== 0) throw new Error(`could not find the global npm folder (exit ${root.status})`);
  setup(path.join(String(root.stdout).trim(), '@telaralabs', 'tap'), { output, spawn });
  output(`tap ${latest} is installed. Restart your agents so they start the new runner.`);
  return 0;
}

function run(argv, packageDir = path.resolve(__dirname, '..')) {
  const args = argv.slice();
  if (args[0] === 'upgrade' || args[0] === 'update') {
    try {
      return upgrade(packageDir);
    } catch (error) {
      console.error(`tap upgrade: ${error.message}`);
      return 1;
    }
  }
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

module.exports = { platformKey, resolveRunner, run, setup, upgrade };
