'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const { mainPackage, platforms } = require('./platforms.cjs');

const platformKeys = new Set(platforms.map((p) => p.key));

function parseChecksums(contents) {
  const entries = new Map();
  for (const line of contents.trim().split(/\r?\n/)) {
    const match = /^([a-f0-9]{64})  ([^/\\]+)$/.exec(line);
    if (!match) throw new Error(`invalid SHA256SUMS entry: ${line}`);
    if (entries.has(match[2])) throw new Error(`duplicate SHA256SUMS entry: ${match[2]}`);
    entries.set(match[2], match[1]);
  }
  return entries;
}

function trustedPublicKey(publicKeyFile) {
  const keyBytes = Buffer.from(fs.readFileSync(publicKeyFile, 'utf8').trim(), 'hex');
  if (keyBytes.length !== 32) throw new Error('trusted release public key must contain 32 bytes');
  return crypto.createPublicKey({
    key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), keyBytes]),
    format: 'der',
    type: 'spki',
  });
}

function verifyRelease(releaseDir, publicKeyFile, allowUnsigned = false) {
  const sumsBytes = fs.readFileSync(path.join(releaseDir, 'SHA256SUMS'));
  const sums = parseChecksums(sumsBytes.toString('utf8'));
  const signaturePath = path.join(releaseDir, 'SHA256SUMS.sig');
  const signed = fs.existsSync(signaturePath);
  if (!signed && !allowUnsigned) {
    throw new Error('release has no SHA256SUMS.sig; signed release verification is required');
  }
  if (signed) {
    const signature = Buffer.from(fs.readFileSync(signaturePath, 'utf8').trim(), 'hex');
    if (signature.length !== 64 || !crypto.verify(null, sumsBytes, trustedPublicKey(publicKeyFile), signature)) {
      throw new Error('SHA256SUMS signature does not match the trusted release key');
    }
  }
  for (const [filename, expected] of sums) {
    const bytes = fs.readFileSync(path.join(releaseDir, filename));
    const actual = crypto.createHash('sha256').update(bytes).digest('hex');
    if (actual !== expected) throw new Error(`${filename} does not match SHA256SUMS`);
  }
  for (const entry of fs.readdirSync(releaseDir)) {
    if (!sums.has(entry) && !['SHA256SUMS', 'SHA256SUMS.sig', 'SHA256SUMS.pub'].includes(entry)) {
      throw new Error(`${entry} is in the release but not in SHA256SUMS`);
    }
  }
  return sums;
}

function parseRunnerFilename(filename) {
  const match = /^tap-(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)-(darwin|linux|windows)-(amd64|arm64)(\.exe)?$/.exec(filename);
  if (!match) return null;
  const platform = `${match[2]}/${match[3]}`;
  if (!platformKeys.has(platform) || (match[2] === 'windows') !== Boolean(match[4])) return null;
  return { filename, version: match[1], platform };
}

const repository = { type: 'git', url: 'git+https://github.com/Telara-Labs/TAP-Runtime.git' };

function writeJson(file, value) {
  fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
}

// stagePackages verifies the signed release and writes six packages under
// outputDir: one per platform, holding only that platform's runner, and the
// main package, which holds the launcher and records the digest of every
// runner. packages.json lists them in the order they must be published: the
// platform packages first, because the main package depends on them.
function stagePackages({ templateDir, releaseDir, outputDir, publicKeyFile = path.resolve(templateDir, '../release/release.pub'), allowUnsigned = false }) {
  const sums = verifyRelease(releaseDir, publicKeyFile, allowUnsigned);
  const runners = fs.readdirSync(releaseDir)
    .map(parseRunnerFilename)
    .filter(Boolean)
    .sort((a, b) => a.platform.localeCompare(b.platform));
  if (runners.length !== platforms.length) {
    throw new Error(`release must contain exactly ${platforms.length} supported runner binaries; found ${runners.length}`);
  }
  const versions = new Set(runners.map((runner) => runner.version));
  if (versions.size !== 1) throw new Error('release runner binaries do not share one version');
  const version = [...versions][0];
  const license = path.join(templateDir, '..', 'LICENSE');
  const notices = path.join(releaseDir, 'THIRD_PARTY_NOTICES.txt');

  fs.rmSync(outputDir, { recursive: true, force: true });
  fs.mkdirSync(outputDir, { recursive: true });
  const order = [];
  const assets = {};
  for (const platform of platforms) {
    const runner = runners.find((r) => r.platform === platform.key);
    const expected = sums.get(runner.filename);
    if (!expected) throw new Error(`${runner.filename} is not listed in SHA256SUMS`);
    assets[platform.key] = { package: platform.name, file: platform.binary, sha256: expected };

    const dir = path.join(outputDir, `tap-${platform.suffix}`);
    fs.mkdirSync(path.join(dir, 'bin'), { recursive: true });
    const target = path.join(dir, platform.binary);
    fs.copyFileSync(path.join(releaseDir, runner.filename), target);
    if (platform.os !== 'win32') fs.chmodSync(target, 0o755);
    fs.copyFileSync(license, path.join(dir, 'LICENSE'));
    fs.copyFileSync(notices, path.join(dir, 'THIRD_PARTY_NOTICES.txt'));
    fs.writeFileSync(path.join(dir, 'README.md'),
      `# ${platform.name}\n\nThe TAP runner for ${platform.os} on ${platform.cpu.join(' or ')}. ` +
      `Install \`${mainPackage}\` instead: it depends on this package and runs it.\n`);
    writeJson(path.join(dir, 'package.json'), {
      name: platform.name,
      version,
      description: `The TAP runner for ${platform.os} (${platform.cpu.join(', ')}); installed by ${mainPackage}`,
      license: 'MIT',
      repository,
      os: [platform.os],
      cpu: platform.cpu,
      files: ['bin/', 'LICENSE', 'README.md', 'THIRD_PARTY_NOTICES.txt'],
      preferUnplugged: true,
      publishConfig: { access: 'public' },
    });
    order.push({ name: platform.name, dir: path.basename(dir) });
  }

  const mainDir = path.join(outputDir, 'tap');
  fs.mkdirSync(path.join(mainDir, 'bin'), { recursive: true });
  fs.mkdirSync(path.join(mainDir, 'lib'), { recursive: true });
  fs.copyFileSync(license, path.join(mainDir, 'LICENSE'));
  fs.copyFileSync(notices, path.join(mainDir, 'THIRD_PARTY_NOTICES.txt'));
  for (const file of ['bin/tap.cjs', 'lib/runner.cjs', 'lib/platforms.cjs', 'postinstall.cjs', 'README.md']) {
    fs.copyFileSync(path.join(templateDir, file), path.join(mainDir, file));
  }
  fs.chmodSync(path.join(mainDir, 'bin/tap.cjs'), 0o755);
  const packageJson = JSON.parse(fs.readFileSync(path.join(templateDir, 'package.json'), 'utf8'));
  packageJson.version = version;
  delete packageJson.private;
  packageJson.optionalDependencies = Object.fromEntries(platforms.map((p) => [p.name, version]));
  writeJson(path.join(mainDir, 'package.json'), packageJson);
  writeJson(path.join(mainDir, 'runner-assets.json'), { version, assets });
  order.push({ name: mainPackage, dir: 'tap' });

  writeJson(path.join(outputDir, 'packages.json'), { version, publishOrder: order });
  return { outputDir, version, publishOrder: order };
}

module.exports = { parseChecksums, parseRunnerFilename, stagePackages, verifyRelease };
