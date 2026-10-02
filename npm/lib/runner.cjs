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
  if (!asset) throw new Error(`TAP has no runner for ${key}`);
  const runner = path.join(packageDir, 'assets', asset.file);
  const digest = crypto.createHash('sha256').update(fs.readFileSync(runner)).digest('hex');
  if (digest !== asset.sha256) throw new Error(`TAP runner ${asset.file} failed its SHA-256 check`);
  return runner;
}

function findCommand(name, envPath = process.env.PATH || '') {
  const suffixes = process.platform === 'win32'
    ? (process.env.PATHEXT || '.EXE;.CMD;.BAT').split(';')
    : [''];
  for (const dir of envPath.split(path.delimiter)) {
    if (!dir) continue;
    for (const suffix of suffixes) {
      const candidate = path.join(dir, `${name}${suffix}`);
      try {
        fs.accessSync(candidate, fs.constants.X_OK);
        return candidate;
      } catch {}
    }
  }
  return null;
}

function setup(packageDir, { envPath = process.env.PATH || '', output = console.log, errorOutput = console.error } = {}) {
  const runner = resolveRunner(packageDir);
  let registered = 0;
  for (const client of ['claude', 'codex']) {
    if (!findCommand(client, envPath)) continue;
    const result = spawnSync(runner, ['install', '--client', client], { stdio: 'inherit' });
    if (result.error) throw result.error;
    if (result.status !== 0) throw new Error(`TAP could not register with ${client} (exit ${result.status})`);
    registered += 1;
  }
  if (registered === 0) {
    output('No supported TAP client was found. Install Claude Code or Codex, then run `tap setup`.');
  } else {
    output(`TAP is registered with ${registered} local agent client${registered === 1 ? '' : 's'}.`);
  }
  errorOutput('Gemini support is experimental; VS Code uses the separate TAP extension.');
  return registered;
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

module.exports = { findCommand, platformKey, resolveRunner, run, setup };
