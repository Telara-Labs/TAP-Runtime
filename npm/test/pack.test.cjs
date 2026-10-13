'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { parseChecksums, parseRunnerFilename, stagePackages, verifyRelease } = require('../lib/pack.cjs');
const { platformKey } = require('../lib/runner.cjs');

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

test('stages six packages: five platform packages and the main one, from one verified release', (t) => {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'tap-npm-pack-')));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const releaseDir = path.join(root, 'release');
  const outputDir = path.join(root, 'packages');
  fs.mkdirSync(releaseDir);
  const publicKeyFile = makeRelease(releaseDir);
  const staged = stagePackages({ templateDir, releaseDir, outputDir, publicKeyFile });
  const read = (...p) => JSON.parse(fs.readFileSync(path.join(outputDir, ...p), 'utf8'));

  // Publish order: the platform packages, then the main package last.
  const names = staged.publishOrder.map((p) => p.name);
  assert.deepEqual(names, [
    '@telaralabs/tap-darwin-arm64',
    '@telaralabs/tap-darwin-x64',
    '@telaralabs/tap-linux-arm64',
    '@telaralabs/tap-linux-x64',
    '@telaralabs/tap-win32-x64',
    '@telaralabs/tap',
  ]);
  assert.deepEqual(read('packages.json').publishOrder.map((p) => p.name), names);

  const expected = {
    '@telaralabs/tap-darwin-arm64': [['darwin'], ['arm64'], 'bin/tap'],
    '@telaralabs/tap-darwin-x64': [['darwin'], ['x64'], 'bin/tap'],
    '@telaralabs/tap-linux-arm64': [['linux'], ['arm64'], 'bin/tap'],
    '@telaralabs/tap-linux-x64': [['linux'], ['x64'], 'bin/tap'],
    '@telaralabs/tap-win32-x64': [['win32'], ['x64', 'arm64'], 'bin/tap.exe'],
  };
  for (const { name, dir } of staged.publishOrder.slice(0, 5)) {
    const pkg = read(dir, 'package.json');
    const [os_, cpu, binary] = expected[name];
    assert.equal(pkg.name, name);
    assert.equal(pkg.version, '0.1.1');
    assert.deepEqual(pkg.os, os_);
    assert.deepEqual(pkg.cpu, cpu);
    assert.equal(pkg.scripts, undefined);
    assert.equal(pkg.bin, undefined);
    assert.equal(pkg.dependencies, undefined);
    assert.equal(pkg.publishConfig.access, 'public');
    // Only its own runner, plus the licence and notices.
    const files = [];
    const walk = (d) => fs.readdirSync(d, { withFileTypes: true }).forEach((e) => (e.isDirectory() ? walk(path.join(d, e.name)) : files.push(path.relative(path.join(outputDir, dir), path.join(d, e.name)).split(path.sep).join('/'))));
    walk(path.join(outputDir, dir));
    assert.deepEqual(files.sort(), [binary, 'LICENSE', 'README.md', 'THIRD_PARTY_NOTICES.txt', 'package.json'].sort(), name);
    // Windows does not expose POSIX execute bits through stat(). Keep this
    // permission check on Unix; package contents and platform resolution are
    // still verified on every platform.
    if (process.platform !== 'win32' && !binary.endsWith('.exe')) {
      assert.equal(fs.statSync(path.join(outputDir, dir, binary)).mode & 0o111, 0o111);
    }
  }

  const main = read('tap', 'package.json');
  const manifest = read('tap', 'runner-assets.json');
  assert.equal(main.name, '@telaralabs/tap');
  assert.equal(main.version, '0.1.1');
  assert.equal(main.private, undefined);
  assert.equal(main.repository.url, 'git+https://github.com/Telara-Labs/TAP-Runtime.git');
  assert.ok(!main.files.includes('assets/'));
  assert.deepEqual(main.optionalDependencies, Object.fromEntries(names.slice(0, 5).map((n) => [n, '0.1.1'])));
  assert.equal(Object.keys(manifest.assets).length, 5);
  assert.equal(manifest.assets['linux/amd64'].package, '@telaralabs/tap-linux-x64');
  assert.ok(!fs.existsSync(path.join(outputDir, 'tap', 'assets')));
  for (const f of ['LICENSE', 'THIRD_PARTY_NOTICES.txt', 'README.md', 'lib/runner.cjs', 'lib/platforms.cjs', 'postinstall.cjs']) {
    assert.ok(fs.existsSync(path.join(outputDir, 'tap', f)), f);
  }
  assert.match(fs.readFileSync(path.join(outputDir, 'tap/bin/tap.cjs'), 'utf8'), /runner/);

  // The staged main package resolves the staged platform package once npm
  // would have put it under node_modules.
  const mainDir = path.join(outputDir, 'tap');
  fs.mkdirSync(path.join(mainDir, 'node_modules', '@telaralabs'), { recursive: true });
  fs.cpSync(path.join(outputDir, 'tap-darwin-arm64'), path.join(mainDir, 'node_modules', '@telaralabs', 'tap-darwin-arm64'), { recursive: true });
  const { resolveRunner } = require(path.join(mainDir, 'lib/runner.cjs'));
  assert.equal(resolveRunner(mainDir, 'darwin', 'arm64'), path.join(mainDir, 'node_modules', '@telaralabs', 'tap-darwin-arm64', 'bin/tap'));
  assert.throws(() => resolveRunner(mainDir, 'linux', 'x64'), /Reinstall without omitting optional dependencies/);
  assert.equal(platformKey('win32', 'x64'), 'windows/amd64');
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
