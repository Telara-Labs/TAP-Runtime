'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { spawn: spawnAsync, spawnSync } = require('node:child_process');

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

const paint = (on, code, t) => (on ? `\x1b[${code}m${t}\x1b[0m` : t);
const GLASS_ROWS = 4;

// faucetFrame is one frame of the upgrade animation: the old version on a
// faucet dripping into a glass that fills toward the new version. fill is how
// many of the glass's rows hold water.
function faucetFrame(frame, from, to, fill, color = true) {
  const tap = (t) => paint(color, '38;5;99', t); // Telara violet, as termart.AccentSGR
  const water = (t) => paint(color, '36', t);
  const dim = (t) => paint(color, '2', t);
  const label = ` ${from} `.padEnd(9, ' ');
  const lines = [
    `      ${tap('┌─────────┐')}`,
    ` ${tap('═════╡')}${paint(color, '1', label)}${tap('╞═══╗')}`,
    `      ${tap('└─────────┘')}   ${tap('║')}`,
    `                   ${tap('═╩═')}`,
  ];
  const dropping = fill < GLASS_ROWS;
  for (let row = 0; row < 3; row++) {
    lines.push(dropping && frame >= 0 && frame % 4 === row ? `                    ${water('●')}` : '');
  }
  lines.push(`               ${dim('╭─────────╮')}`);
  for (let row = 0; row < GLASS_ROWS; row++) {
    const level = GLASS_ROWS - row; // 4 is the top row
    let inside = '         ';
    if (level <= fill) inside = level === fill && fill < GLASS_ROWS ? water('≈≈≈≈≈≈≈≈≈') : water('█████████');
    lines.push(`               ${dim('│')}${inside}${dim('│')}`);
  }
  lines.push(`               ${dim('╰─────────╯')} ${fill >= GLASS_ROWS ? paint(color, '1', to) : dim(to)}`);
  return lines;
}

// animate redraws frames in place on a terminal until the returned stop is
// called; stop draws the last frame and leaves it on screen.
function animate(stream, render) {
  let drawn = 0;
  let frame = 0;
  const draw = (f) => {
    const lines = render(f);
    let out = drawn ? `\r\x1b[${drawn}A` : '';
    for (const l of lines) out += `\r\x1b[K${l}\n`;
    drawn = lines.length;
    stream.write(out);
  };
  draw(frame++);
  const timer = setInterval(() => draw(frame++), 90);
  return () => {
    clearInterval(timer);
    draw(-1);
  };
}

function runAsync(cmd, args, opts) {
  return new Promise((resolve) => {
    const child = spawnAsync(cmd, args, { ...opts, stdio: ['ignore', 'pipe', 'pipe'] });
    let out = '';
    child.stdout.on('data', (d) => { out += d; });
    child.stderr.on('data', (d) => { out += d; });
    child.on('error', (error) => resolve({ error, output: out }));
    child.on('close', (status) => resolve({ status, output: out }));
  });
}

// upgrade replaces the global install with the newest published version, then
// runs the new runner's setup itself: npm hides the postinstall output and may
// skip install scripts, so agents would otherwise keep the old runner. On a
// terminal npm works quietly behind the faucet; its output is shown only if
// it fails.
async function upgrade(packageDir, { output = console.log, spawn = spawnSync, exec = runAsync, stream = process.stderr } = {}) {
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
  const installArgs = ['install', '--global', `@telaralabs/tap@${latest}`];
  if (stream.isTTY && (stream.columns || 80) >= 40) {
    let fill = 0;
    let filled = false;
    const started = Date.now();
    const stop = animate(stream, (f) => {
      if (!filled) fill = Math.min(GLASS_ROWS - 1, Math.floor((Date.now() - started) / 1500));
      return faucetFrame(f, current, latest, fill);
    });
    const install = await exec(npm, installArgs, { shell });
    if (!install.error && install.status === 0) {
      filled = true;
      fill = GLASS_ROWS;
    }
    stop();
    if (install.error) throw install.error;
    if (install.status !== 0) {
      stream.write(install.output);
      throw new Error(`npm install failed (exit ${install.status})`);
    }
    output(` ${paint(true, '32', '✓')} tap ${latest} installed`);
  } else {
    output(`Upgrading tap ${current} to ${latest}`);
    const install = spawn(npm, installArgs, { stdio: 'inherit', shell });
    if (install.error) throw install.error;
    if (install.status !== 0) throw new Error(`npm install failed (exit ${install.status})`);
  }
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
    return upgrade(packageDir).catch((error) => {
      console.error(`tap upgrade: ${error.message}`);
      return 1;
    });
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

module.exports = { faucetFrame, platformKey, resolveRunner, run, setup, upgrade };
