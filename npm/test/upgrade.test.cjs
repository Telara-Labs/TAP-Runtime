'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { platformKey, upgrade } = require('../lib/runner.cjs');

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

test('upgrade installs the newer version and runs the new runner setup', () => {
  const dir = fakePackage('0.1.8');
  const root = globalWith('0.1.20');
  const calls = [];
  const lines = [];
  assert.equal(upgrade(dir, { output: (l) => lines.push(l), spawn: fakeNpm('0.1.20', root, calls) }), 0);
  assert.deepEqual(calls.find((c) => c[1][0] === 'install')[1], ['install', '--global', '@telaralabs/tap@0.1.20']);
  const setupCall = calls.find((c) => c[1][0] === 'setup');
  assert.equal(setupCall[0], path.join(root, '@telaralabs', 'tap', 'assets', 'tap'));
  assert.match(lines.join('\n'), /0\.1\.8 to 0\.1\.20/);
  assert.match(lines.join('\n'), /Restart your agents/);
});

test('upgrade does nothing when the install is already the latest', () => {
  const dir = fakePackage('0.1.20');
  const calls = [];
  const lines = [];
  assert.equal(upgrade(dir, { output: (l) => lines.push(l), spawn: fakeNpm('0.1.20', '/unused', calls) }), 0);
  assert.equal(calls.length, 1);
  assert.match(lines.join('\n'), /0\.1\.20 is the latest version/);
});

test('upgrade never downgrades a newer local build', () => {
  const dir = fakePackage('0.1.21');
  const calls = [];
  upgrade(dir, { output: () => {}, spawn: fakeNpm('0.1.20', '/unused', calls) });
  assert.ok(!calls.some((c) => c[1][0] === 'install'));
});

test('upgrade compares versions numerically, not as text', () => {
  const dir = fakePackage('0.1.9');
  const root = globalWith('0.1.10');
  const calls = [];
  upgrade(dir, { output: () => {}, spawn: fakeNpm('0.1.10', root, calls) });
  assert.ok(calls.some((c) => c[1][0] === 'install'));
});

test('upgrade reports a failed npm install and skips setup', () => {
  const dir = fakePackage('0.1.8');
  const calls = [];
  assert.throws(() => upgrade(dir, { output: () => {}, spawn: fakeNpm('0.1.20', '/unused', calls, { installStatus: 1 }) }), /npm install failed/);
  assert.ok(!calls.some((c) => c[1][0] === 'setup'));
});
