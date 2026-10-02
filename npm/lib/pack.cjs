'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const platforms = new Set([
  'darwin/arm64',
  'darwin/amd64',
  'linux/arm64',
  'linux/amd64',
  'windows/amd64',
]);

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
  if (!platforms.has(platform) || (match[2] === 'windows') !== Boolean(match[4])) return null;
  return { filename, version: match[1], platform };
}

function stagePackage({ templateDir, releaseDir, outputDir, publicKeyFile = path.resolve(templateDir, '../release/release.pub'), allowUnsigned = false }) {
  const sums = verifyRelease(releaseDir, publicKeyFile, allowUnsigned);
  const runners = fs.readdirSync(releaseDir)
    .map(parseRunnerFilename)
    .filter(Boolean)
    .sort((a, b) => a.platform.localeCompare(b.platform));
  if (runners.length !== platforms.size) {
    throw new Error(`release must contain exactly ${platforms.size} supported runner binaries; found ${runners.length}`);
  }
  const versions = new Set(runners.map((runner) => runner.version));
  if (versions.size !== 1) throw new Error('release runner binaries do not share one version');

  const assets = {};
  for (const runner of runners) {
    const expected = sums.get(runner.filename);
    if (!expected) throw new Error(`${runner.filename} is not listed in SHA256SUMS`);
    assets[runner.platform] = { file: runner.filename, sha256: expected };
  }

  fs.rmSync(outputDir, { recursive: true, force: true });
  fs.mkdirSync(path.join(outputDir, 'assets'), { recursive: true });
  fs.mkdirSync(path.join(outputDir, 'bin'), { recursive: true });
  fs.mkdirSync(path.join(outputDir, 'lib'), { recursive: true });
  for (const runner of runners) {
    fs.copyFileSync(path.join(releaseDir, runner.filename), path.join(outputDir, 'assets', runner.filename));
    if (runner.platform.split('/')[0] !== 'windows') fs.chmodSync(path.join(outputDir, 'assets', runner.filename), 0o755);
  }
  fs.copyFileSync(path.join(templateDir, '..', 'LICENSE'), path.join(outputDir, 'LICENSE'));
  fs.copyFileSync(path.join(releaseDir, 'THIRD_PARTY_NOTICES.txt'), path.join(outputDir, 'THIRD_PARTY_NOTICES.txt'));
  for (const file of ['bin/tap.cjs', 'lib/runner.cjs', 'postinstall.cjs', 'README.md']) {
    fs.copyFileSync(path.join(templateDir, file), path.join(outputDir, file));
  }
  fs.chmodSync(path.join(outputDir, 'bin/tap.cjs'), 0o755);
  const packageJson = JSON.parse(fs.readFileSync(path.join(templateDir, 'package.json'), 'utf8'));
  packageJson.version = [...versions][0];
  delete packageJson.private;
  fs.writeFileSync(path.join(outputDir, 'package.json'), `${JSON.stringify(packageJson, null, 2)}\n`);
  fs.writeFileSync(path.join(outputDir, 'runner-assets.json'), `${JSON.stringify({ version: packageJson.version, assets }, null, 2)}\n`);
  return outputDir;
}

module.exports = { parseChecksums, parseRunnerFilename, stagePackage, verifyRelease };
