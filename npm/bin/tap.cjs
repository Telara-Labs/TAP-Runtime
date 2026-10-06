#!/usr/bin/env node
'use strict';

const { run } = require('../lib/runner.cjs');
Promise.resolve(run(process.argv.slice(2))).then((code) => { process.exitCode = code; });
