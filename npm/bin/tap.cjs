#!/usr/bin/env node
'use strict';

const { run } = require('../lib/runner.cjs');
process.exitCode = run(process.argv.slice(2));
