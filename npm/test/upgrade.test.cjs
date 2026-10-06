'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { faucetFrame, platformKey, upgrade } = require('../lib/runner.cjs');

const pipe = { isTTY: false, write() {} };

function fakePackage(version) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tap-npm-'));
  fs.mkdirSync(path.join(dir, 'assets'));
  const body = Buffer.from('#!/bin/sh\n');
  fs.writeFileSync(path.join(dir, 'assets', 'tap'), body, { mode: 0o755 });
  const sha256 = crypto.createHash('sha256').update(body).digest('hex');
  fs.writeFileSync(path.join(dir, 'runner-assets.json'), JSON.stringify({ assets: { [platformKey()]: { file: 'tap', sha256 } } }));
  fs.writeFileSync(path.join(dir, 'package.json'), JSON.stringify({ name: '@telaralabs/tap', version }));
  return dir;
}

// fakeNpm answers npm view and npm root like npm would, with the new package
// already in place under the returned global folder.
function fakeNpm(latest, globalRoot, calls, { installStatus = 0 } = {}) {
  return (cmd, args) => {
    calls.push([cmd, args]);
    if (args[0] === 'view') return { status: 0, stdout: `${latest}\n` };
    if (args[0] === 'install') return { status: installStatus };
    if (args[0] === 'root') return { status: 0, stdout: `${globalRoot}\n` };
    return { status: 0 };
  };
}

function globalWith(version) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'tap-global-'));
  const pkg = fakePackage(version);
  fs.mkdirSync(path.join(root, '@telaralabs'));
  fs.renameSync(pkg, path.join(root, '@telaralabs', 'tap'));
  return root;
}

test('upgrade installs the newer version and runs the new runner setup', async () => {
  const dir = fakePackage('0.1.8');
  const root = globalWith('0.1.20');
  const calls = [];
  const lines = [];
  assert.equal(await upgrade(dir, { output: (l) => lines.push(l), spawn: fakeNpm('0.1.20', root, calls), stream: pipe }), 0);
  assert.deepEqual(calls.find((c) => c[1][0] === 'install')[1], ['install', '--global', '@telaralabs/tap@0.1.20']);
  const setupCall = calls.find((c) => c[1][0] === 'setup');
  assert.equal(setupCall[0], path.join(root, '@telaralabs', 'tap', 'assets', 'tap'));
  assert.match(lines.join('\n'), /0\.1\.8 to 0\.1\.20/);
  assert.match(lines.join('\n'), /Restart your agents/);
});

test('upgrade does nothing when the install is already the latest', async () => {
  const dir = fakePackage('0.1.20');
  const calls = [];
  const lines = [];
  assert.equal(await upgrade(dir, { output: (l) => lines.push(l), spawn: fakeNpm('0.1.20', '/unused', calls), stream: pipe }), 0);
  assert.equal(calls.length, 1);
  assert.match(lines.join('\n'), /0\.1\.20 is the latest version/);
});

test('upgrade never downgrades a newer local build', async () => {
  const dir = fakePackage('0.1.21');
  const calls = [];
  await upgrade(dir, { output: () => {}, spawn: fakeNpm('0.1.20', '/unused', calls), stream: pipe });
  assert.ok(!calls.some((c) => c[1][0] === 'install'));
});

test('upgrade compares versions numerically, not as text', async () => {
  const dir = fakePackage('0.1.9');
  const root = globalWith('0.1.10');
  const calls = [];
  await upgrade(dir, { output: () => {}, spawn: fakeNpm('0.1.10', root, calls), stream: pipe });
  assert.ok(calls.some((c) => c[1][0] === 'install'));
});

test('upgrade reports a failed npm install and skips setup', async () => {
  const dir = fakePackage('0.1.8');
  const calls = [];
  await assert.rejects(upgrade(dir, { output: () => {}, spawn: fakeNpm('0.1.20', '/unused', calls, { installStatus: 1 }), stream: pipe }), /npm install failed/);
  assert.ok(!calls.some((c) => c[1][0] === 'setup'));
});

function fakeTerminal() {
  const written = [];
  return { isTTY: true, columns: 100, write: (t) => written.push(t), written };
}

test('on a terminal upgrade animates the faucet and keeps npm quiet', async () => {
  const dir = fakePackage('0.1.8');
  const root = globalWith('0.1.24');
  const calls = [];
  const term = fakeTerminal();
  const exec = async (cmd, args) => { calls.push([cmd, args]); return { status: 0, output: 'npm chatter' }; };
  assert.equal(await upgrade(dir, { output: () => {}, spawn: fakeNpm('0.1.24', root, calls), exec, stream: term }), 0);
  const screen = term.written.join('');
  assert.match(screen, /0\.1\.8/);
  assert.match(screen, /0\.1\.24/);
  assert.match(screen, /╰─────────╯/);
  assert.doesNotMatch(screen, /npm chatter/);
  assert.ok(calls.some((c) => c[1][0] === 'setup'));
});

test('on a terminal a failed npm install shows what npm said', async () => {
  const dir = fakePackage('0.1.8');
  const term = fakeTerminal();
  const exec = async () => ({ status: 1, output: 'EACCES: permission denied' });
  await assert.rejects(upgrade(dir, { output: () => {}, spawn: fakeNpm('0.1.24', '/unused', []), exec, stream: term }), /npm install failed/);
  assert.match(term.written.join(''), /EACCES/);
});

test('faucet frames keep one height and fill the glass from the bottom', () => {
  const strip = (l) => l.replace(/\x1b\[[0-9;]*m/g, '');
  const heights = new Set();
  for (let fill = 0; fill <= 4; fill++) {
    for (let f = -1; f < 8; f++) heights.add(faucetFrame(f, '0.1.8', '0.1.24', fill).length);
  }
  assert.equal(heights.size, 1);
  const half = faucetFrame(0, '0.1.8', '0.1.24', 2, false).map(strip);
  const glass = half.filter((l) => l.includes('│'));
  assert.deepEqual(glass.map((l) => l.trim()), ['│         │', '│         │', '│≈≈≈≈≈≈≈≈≈│', '│█████████│']);
  const full = faucetFrame(-1, '0.1.8', '0.1.24', 4, false).map(strip);
  assert.ok(!full.some((l) => l.includes('●')), 'a full glass has no drop');
  assert.ok(full.every((l) => !l.includes('\x1b')), 'no color when color is off');
});
