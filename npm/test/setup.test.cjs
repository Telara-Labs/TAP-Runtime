'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const { setup } = require('../lib/runner.cjs');
const { fakePackage, runnerOf } = require('./fixture.cjs');

// setup keeps no agent list of its own: it asks the verified runner to
// connect every detected agent.
test('setup delegates connection and saved skill reconciliation to native tap setup', () => {
  const dir = fakePackage();
  const calls = [];
  const lines = [];
  const status = setup(dir, {
    output: (l) => lines.push(l),
    spawn: (cmd, args) => { calls.push([cmd, args]); return { status: 0 }; },
  });
  assert.equal(status, 0);
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], runnerOf(dir));
  assert.deepEqual(calls[0][1], ['setup']);
  assert.match(lines.join('\n'), /tap setup/);
});

test('setup reports a failed install', () => {
  const dir = fakePackage();
  assert.throws(() => setup(dir, { output: () => {}, spawn: () => ({ status: 1 }) }), /could not connect/);
});

test('setup refuses a runner that fails its checksum', () => {
  const dir = fakePackage();
  fs.writeFileSync(runnerOf(dir), 'tampered');
  assert.throws(() => setup(dir, { output: () => {}, spawn: () => ({ status: 0 }) }), /SHA-256/);
});
