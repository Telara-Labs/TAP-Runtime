'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { parseChecksums, parseRunnerFilename, stagePackage, verifyRelease } = require('../lib/pack.cjs');
const { platformKey, resolveRunner } = require('../lib/runner.cjs');

const templateDir = path.resolve(__dirname, '..');
const platforms = [
  ['darwin', 'arm64', ''],
  ['darwin', 'amd64', ''],
  ['linux', 'arm64', ''],
  ['linux', 'amd64', ''],
  ['windows', 'amd64', '.exe'],
];

function makeRelease(dir) {
  const files = [];
  for (const [osName, arch, suffix] of platforms) {
    const name = `tap-0.1.1-${osName}-${arch}${suffix}`;
    const bytes = Buffer.from(`runner:${osName}/${arch}`);
    fs.writeFileSync(path.join(dir, name), bytes);
    files.push(`${crypto.createHash('sha256').update(bytes).digest('hex')}  ${name}`);
  }
  const notices = Buffer.from('Third-party test fixture notices\n');
  fs.writeFileSync(path.join(dir, 'THIRD_PARTY_NOTICES.txt'), notices);
  files.push(`${crypto.createHash('sha256').update(notices).digest('hex')}  THIRD_PARTY_NOTICES.txt`);
  const sums = `${files.join('\n')}\n`;
  fs.writeFileSync(path.join(dir, 'SHA256SUMS'), sums);
  const keys = crypto.generateKeyPairSync('ed25519');
  const rawPublic = keys.publicKey.export({ format: 'der', type: 'spki' }).subarray(-32);
  fs.writeFileSync(path.join(dir, 'SHA256SUMS.sig'), `${crypto.sign(null, Buffer.from(sums), keys.privateKey).toString('hex')}\n`);
  const publicKeyFile = path.join(path.dirname(dir), 'trusted.pub');
  fs.writeFileSync(publicKeyFile, `${rawPublic.toString('hex')}\n`);
  return publicKeyFile;
}

test('parses checksum and only accepts known platform runner names', () => {
  assert.equal(parseChecksums(`${'a'.repeat(64)}  tap-0.1.1-linux-amd64\n`).size, 1);
  assert.equal(parseRunnerFilename('tap-0.1.1-darwin-arm64').platform, 'darwin/arm64');
  assert.equal(parseRunnerFilename('tap-0.1.1-windows-amd64.exe').platform, 'windows/amd64');
  assert.equal(parseRunnerFilename('tap-0.1.1-windows-amd64'), null);
  assert.throws(() => parseChecksums(`${'A'.repeat(64)}  tap`), /invalid SHA256SUMS/);
});

test('stages all five verified binaries and embeds their hashes and release version', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'tap-npm-pack-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const releaseDir = path.join(root, 'release');
  const outputDir = path.join(root, 'package');
  fs.mkdirSync(releaseDir);
  const publicKeyFile = makeRelease(releaseDir);
  stagePackage({ templateDir, releaseDir, outputDir, publicKeyFile });
  const pkg = JSON.parse(fs.readFileSync(path.join(outputDir, 'package.json'), 'utf8'));
  const manifest = JSON.parse(fs.readFileSync(path.join(outputDir, 'runner-assets.json'), 'utf8'));
  assert.equal(pkg.name, '@telaralabs/tap');
  assert.equal(pkg.version, '0.1.1');
  assert.equal(pkg.private, undefined);
  assert.equal(pkg.repository.url, 'git+https://github.com/Telara-Labs/TAP-Runtime.git');
  assert.equal(Object.keys(manifest.assets).length, 5);
  assert.equal(fs.statSync(path.join(outputDir, 'assets', 'tap-0.1.1-linux-amd64')).mode & 0o111, 0o111);
  assert.ok(fs.existsSync(path.join(outputDir, 'LICENSE')));
  assert.ok(fs.existsSync(path.join(outputDir, 'THIRD_PARTY_NOTICES.txt')));
  assert.ok(fs.existsSync(path.join(outputDir, 'README.md')));
  assert.match(fs.readFileSync(path.join(outputDir, 'bin/tap.cjs'), 'utf8'), /runner/);
  assert.doesNotThrow(() => resolveRunner(outputDir, 'darwin', 'arm64'));
  assert.throws(() => resolveRunner(outputDir, 'freebsd', 'x64'), /no prebuilt runner for freebsd\/amd64\. Prebuilt runners: .*darwin\/arm64.*go install github\.com\/Telara-Labs\/TAP-Runtime\/host@latest/);
  assert.equal(platformKey('win32', 'x64'), 'windows/amd64');
  assert.equal(platformKey('win32', 'arm64'), 'windows/amd64');
});

test('rejects an artifact whose content differs from the release checksum', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'tap-npm-bad-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const releaseDir = path.join(root, 'release');
  fs.mkdirSync(releaseDir);
  const publicKeyFile = makeRelease(releaseDir);
  fs.appendFileSync(path.join(releaseDir, 'tap-0.1.1-linux-amd64'), 'tampered');
  assert.throws(() => verifyRelease(releaseDir, publicKeyFile), /does not match SHA256SUMS/);
});

test('refuses unsigned artifacts unless an explicit test-only option is used', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'tap-npm-unsigned-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const releaseDir = path.join(root, 'release');
  fs.mkdirSync(releaseDir);
  makeRelease(releaseDir);
  fs.rmSync(path.join(releaseDir, 'SHA256SUMS.sig'));
  assert.throws(() => verifyRelease(releaseDir, path.join(releaseDir, 'trusted.pub')), /signed release verification is required/);
  assert.equal(verifyRelease(releaseDir, '', true).size, 6);
});
