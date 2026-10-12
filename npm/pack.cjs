#!/usr/bin/env node
'use strict';

const path = require('node:path');
const { stagePackages } = require('./lib/pack.cjs');

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
      const staged = stagePackages({
        templateDir: __dirname,
        releaseDir: path.resolve(releaseDir),
        outputDir: path.resolve(outputDir),
        allowUnsigned,
      });
      for (const { name, dir } of staged.publishOrder) {
        console.log(`staged ${name}@${staged.version} at ${path.join(staged.outputDir, dir)}`);
      }
      console.log('publish them in that order; packages.json lists it');
    } catch (error) {
      console.error(`tap npm pack: ${error.message}`);
      process.exitCode = 1;
    }
  }
}

module.exports = { usage };
