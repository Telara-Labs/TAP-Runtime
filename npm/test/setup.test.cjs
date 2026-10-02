'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { platformKey, setup } = require('../lib/runner.cjs');

function fakePackage() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tap-npm-'));
  fs.mkdirSync(path.join(dir, 'assets'));
  const body = Buffer.from('#!/bin/sh\n');
  fs.writeFileSync(path.join(dir, 'assets', 'tap'), body, { mode: 0o755 });
  const sha256 = crypto.createHash('sha256').update(body).digest('hex');
  fs.writeFileSync(path.join(dir, 'runner-assets.json'), JSON.stringify({ assets: { [platformKey()]: { file: 'tap', sha256 } } }));
  return dir;
}

// setup keeps no agent list of its own: it asks the verified runner to
// connect every detected agent (TENG-3114).
test('setup delegates to tap install --client detected', () => {
  const dir = fakePackage();
  const calls = [];
  const lines = [];
  const status = setup(dir, {
    output: (l) => lines.push(l),
    spawn: (cmd, args) => { calls.push([cmd, args]); return { status: 0 }; },
  });
  assert.equal(status, 0);
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], path.join(dir, 'assets', 'tap'));
  assert.deepEqual(calls[0][1], ['install', '--client', 'detected']);
  assert.match(lines.join('\n'), /tap install --client <agent>/);
});

test('setup reports a failed install', () => {
  const dir = fakePackage();
  assert.throws(() => setup(dir, { output: () => {}, spawn: () => ({ status: 1 }) }), /could not connect/);
});

test('setup refuses a runner that fails its checksum', () => {
  const dir = fakePackage();
  fs.writeFileSync(path.join(dir, 'assets', 'tap'), 'tampered');
  assert.throws(() => setup(dir, { output: () => {}, spawn: () => ({ status: 0 }) }), /SHA-256/);
});
