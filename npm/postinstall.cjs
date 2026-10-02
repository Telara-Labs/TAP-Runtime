'use strict';

const { setup } = require('./lib/runner.cjs');

// A global install is the advertised one-command setup. A project dependency
// must not silently rewrite a developer's user-level agent configuration.
if (process.env.npm_config_global === 'true') {
  try {
    setup(__dirname);
  } catch (error) {
    console.error(`TAP setup: ${error.message}`);
    process.exitCode = 1;
  }
} else {
  console.log('TAP is installed for this project. Run `npm exec -- tap setup` to register it with local agent clients.');
}
