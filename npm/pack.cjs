#!/usr/bin/env node
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { stagePackage } = require('./lib/pack.cjs');

function usage() {
  return 'usage: node npm/pack.cjs <release-directory> <output-directory>';
}

if (require.main === module) {
  const args = process.argv.slice(2);
  const allowUnsigned = args.includes('--allow-unsigned-for-tests');
  const positional = args.filter((arg) => arg !== '--allow-unsigned-for-tests');
  const [releaseDir, outputDir, ...extra] = positional;
  if (!releaseDir || !outputDir || extra.length) {
    console.error(`${usage()}${allowUnsigned ? '' : '\n  --allow-unsigned-for-tests is only for local packaging checks'}`);
    process.exitCode = 2;
  } else {
    try {
      const packageDir = stagePackage({
        templateDir: __dirname,
        releaseDir: path.resolve(releaseDir),
        outputDir: path.resolve(outputDir),
        allowUnsigned,
      });
      console.log(`staged ${JSON.parse(fs.readFileSync(path.join(packageDir, 'package.json'), 'utf8')).name}@${JSON.parse(fs.readFileSync(path.join(packageDir, 'package.json'), 'utf8')).version} at ${packageDir}`);
      console.log(`create the installable archive with: npm pack ${packageDir}`);
    } catch (error) {
      console.error(`tap npm pack: ${error.message}`);
      process.exitCode = 1;
    }
  }
}

module.exports = { usage };
