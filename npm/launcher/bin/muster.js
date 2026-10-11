#!/usr/bin/env node
// Launcher for the muster binary (#243).
//
// This package carries no binary. npm installs exactly one of the sibling
// @futurelastic/muster-<os>-<cpu> packages (their `os`/`cpu` fields do the
// choosing, via optionalDependencies), and this file finds it and runs it.
// Nothing is downloaded at install time, so `--ignore-scripts` changes nothing.
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');

const SCOPE = '@futurelastic/muster-';

// The platforms this launcher supports are the ones its own package.json lists
// as optionalDependencies — assembled from the release, so there is no second
// list here to drift from it. An unpublished platform (windows, today) is
// absent from that list in a normal release and present when assembled with
// --all, and the launcher follows without a code change.
function supportedFrom(optionalDependencies) {
  return Object.keys(optionalDependencies || {})
    .filter((n) => n.startsWith(SCOPE))
    .map((n) => n.slice(SCOPE.length))
    .sort();
}

function fail(message) {
  process.stderr.write(`muster: ${message}\n`);
  process.exit(1);
}

// resolveBinary returns the absolute path of the platform package's binary, or
// throws an Error whose message says what to do. Exported through `require.main`
// so the test can drive it without spawning anything. `optionalDependencies` is
// this package's own, which is what defines "supported".
function resolveBinary(platform, arch, resolve, ownVersion, optionalDependencies) {
  const key = `${platform}-${arch}`;
  const supported = supportedFrom(optionalDependencies);
  if (!supported.includes(key)) {
    throw new Error(
      `no prebuilt binary for ${key} (supported: ${supported.join(', ') || 'none'}). ` +
        'Build from source: https://github.com/futurelastic/muster#install'
    );
  }
  const pkg = `@futurelastic/muster-${key}`;
  let manifest;
  try {
    manifest = resolve(`${pkg}/package.json`);
  } catch {
    throw new Error(
      `${pkg} is not installed. It is an optional dependency of @futurelastic/muster, ` +
        'so an install that skipped optional dependencies (--omit=optional) or copied ' +
        'node_modules between platforms leaves it out. Reinstall on this machine.'
    );
  }
  const theirs = JSON.parse(fs.readFileSync(manifest, 'utf8')).version;
  if (theirs !== ownVersion) {
    // The versions are published together and must stay together: a launcher
    // running a different build than the one it was released with is a
    // support problem nobody can see from `muster --version` alone.
    throw new Error(
      `version mismatch: @futurelastic/muster is ${ownVersion} but ${pkg} is ${theirs}. ` +
        'Reinstall so both come from the same release.'
    );
  }
  // Npm names follow process.platform, so windows is win32 and its binary is
  // muster.exe.
  return path.join(path.dirname(manifest), 'bin', platform === 'win32' ? 'muster.exe' : 'muster');
}

function run() {
  const manifest = require('../package.json');
  let bin;
  try {
    bin = resolveBinary(process.platform, process.arch, require.resolve, manifest.version, manifest.optionalDependencies);
  } catch (err) {
    fail(err.message);
  }
  // A tarball extracted without the executable bit (some mirrors do this)
  // would otherwise fail with an opaque EACCES. Windows has no such bit.
  if (process.platform !== 'win32') {
    try {
      fs.accessSync(bin, fs.constants.X_OK);
    } catch {
      try {
        fs.chmodSync(bin, 0o755);
      } catch (err) {
        fail(`${bin} is not executable and could not be made so: ${err.message}`);
      }
    }
  }

  const child = spawn(bin, process.argv.slice(2), { stdio: 'inherit' });
  // The service handles these itself (a graceful stop); pass them on rather
  // than dying first and orphaning it. Not on Windows: there a signal sent to a
  // child is a hard kill, which is the opposite of passing it on — and a console
  // Ctrl+C already reaches the child itself, as it shares the console.
  if (process.platform !== 'win32') {
    for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
      process.on(sig, () => child.kill(sig));
    }
  }
  child.on('error', (err) => fail(`could not start ${bin}: ${err.message}`));
  child.on('exit', (code, signal) => {
    if (signal) {
      process.removeAllListeners(signal);
      process.kill(process.pid, signal);
      return;
    }
    process.exit(code === null ? 1 : code);
  });
}

if (require.main === module) {
  run();
} else {
  module.exports = { resolveBinary, supportedFrom };
}
