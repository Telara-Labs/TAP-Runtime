'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { platformKey } = require('../lib/runner.cjs');
const { platforms } = require('../lib/platforms.cjs');

// platformFor is the entry for this machine, or for an explicit key.
function platformFor(key = platformKey()) {
  return platforms.find((p) => p.key === key);
}

// fakePackage lays out an installed main package the way npm does: the
// launcher's folder, with the platform package for key under node_modules.
// A script runner stands in for the binary.
function fakePackage({ version = '0.1.1', key = platformKey(), body = '#!/bin/sh\n', withPlatform = true, platformVersion = version } = {}) {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'tap-npm-')));
  const platform = platformFor(key);
  const bytes = Buffer.from(body);
  const sha256 = crypto.createHash('sha256').update(bytes).digest('hex');
  fs.writeFileSync(path.join(dir, 'package.json'), JSON.stringify({ name: '@telaralabs/tap', version }));
  fs.writeFileSync(path.join(dir, 'runner-assets.json'), JSON.stringify({
    version,
    assets: Object.fromEntries(platforms.map((p) => [p.key, { package: p.name, file: p.binary, sha256 }])),
  }));
  if (withPlatform) {
    const pkgDir = path.join(dir, 'node_modules', ...platform.name.split('/'));
    fs.mkdirSync(path.join(pkgDir, 'bin'), { recursive: true });
    fs.writeFileSync(path.join(pkgDir, 'package.json'), JSON.stringify({ name: platform.name, version: platformVersion }));
    fs.writeFileSync(path.join(pkgDir, platform.binary), bytes, { mode: 0o755 });
  }
  return dir;
}

function runnerOf(dir, key = platformKey()) {
  const platform = platformFor(key);
  return path.join(dir, 'node_modules', ...platform.name.split('/'), platform.binary);
}

module.exports = { fakePackage, platformFor, runnerOf };
