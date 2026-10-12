'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const { platformKey, resolveRunner } = require('../lib/runner.cjs');
const { platforms } = require('../lib/platforms.cjs');
const { fakePackage, runnerOf } = require('./fixture.cjs');

// Node's own platform and arch names for each runner the release holds.
const nodeNames = {
  'darwin/arm64': ['darwin', 'arm64'],
  'darwin/amd64': ['darwin', 'x64'],
  'linux/arm64': ['linux', 'arm64'],
  'linux/amd64': ['linux', 'x64'],
  'windows/amd64': ['win32', 'x64'],
};

test('every platform resolves to its own package and nothing else', () => {
  for (const platform of platforms) {
    const dir = fakePackage({ key: platform.key });
    const [nodeOS, nodeArch] = nodeNames[platform.key];
    assert.equal(resolveRunner(dir, nodeOS, nodeArch), runnerOf(dir, platform.key), platform.key);
    assert.ok(runnerOf(dir, platform.key).includes(path.join('@telaralabs', platform.name.split('/')[1])));
  }
});

test('windows on ARM uses the x64 runner', () => {
  const dir = fakePackage({ key: 'windows/amd64' });
  assert.equal(resolveRunner(dir, 'win32', 'arm64'), runnerOf(dir, 'windows/amd64'));
  assert.equal(platformKey('win32', 'arm64'), 'windows/amd64');
});

test('a runner that fails its SHA-256 check is refused', () => {
  const dir = fakePackage();
  fs.writeFileSync(runnerOf(dir), 'tampered');
  assert.throws(() => resolveRunner(dir), /failed its SHA-256 check/);
});

test('a missing platform package says to reinstall with optional dependencies', () => {
  const dir = fakePackage({ withPlatform: false });
  assert.throws(() => resolveRunner(dir), (error) => {
    assert.match(error.message, /@telaralabs\/tap-\S+ is not installed/);
    assert.match(error.message, /--omit=optional/);
    assert.match(error.message, /another operating system or CPU/);
    assert.match(error.message, /Reinstall without omitting optional dependencies/);
    return true;
  });
});

test('a platform package from another version is refused', () => {
  const dir = fakePackage({ version: '0.2.0', platformVersion: '0.1.9' });
  assert.throws(() => resolveRunner(dir), /is not version 0\.2\.0/);
});

test('a platform package without its runner is refused', () => {
  const dir = fakePackage();
  fs.rmSync(runnerOf(dir));
  assert.throws(() => resolveRunner(dir), /does not contain its runner/);
});

test('an unsupported platform names the runners that exist and the source build', () => {
  const dir = fakePackage();
  assert.throws(() => resolveRunner(dir, 'freebsd', 'x64'),
    /no prebuilt runner for freebsd\/amd64\. Prebuilt runners: .*darwin\/arm64.*go install github\.com\/Telara-Labs\/TAP-Runtime\/host@latest/);
  assert.throws(() => resolveRunner(dir, 'linux', 'ia32'), /no prebuilt runner for linux\/ia32/);
});

test('a runner that lost its executable bit gets it back', { skip: process.platform === 'win32' }, () => {
  const dir = fakePackage();
  fs.chmodSync(runnerOf(dir), 0o644);
  resolveRunner(dir);
  assert.equal(fs.statSync(runnerOf(dir)).mode & 0o111, 0o111);
});
