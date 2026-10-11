// Tests for the npm packaging (#243): the launcher's resolution rules and the
// allowlist of every assembled package. Run: node --test npm/test/npm.test.mjs
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { assemble, PLATFORMS, versionFromTag, distTag } from '../../scripts/assemble-npm.mjs';

const require = createRequire(import.meta.url);
const { resolveBinary, supportedFrom } = require('../launcher/bin/muster.js');
const repo = join(fileURLToPath(import.meta.url), '../../..');

function fakeBinaries(tag) {
  const dir = mkdtempSync(join(tmpdir(), 'muster-bin-'));
  for (const p of PLATFORMS) writeFileSync(join(dir, `muster-${tag}-${p.goos}-${p.goarch}${p.ext}`), '#!/bin/sh\necho muster\n');
  return dir;
}

test('version comes from the tag; prereleases go to next', () => {
  assert.equal(versionFromTag('v1.2.3'), '1.2.3');
  assert.equal(versionFromTag('v1.2.3-rc.1'), '1.2.3-rc.1');
  assert.throws(() => versionFromTag('1.2.3'));
  assert.throws(() => versionFromTag('v1.2'));
  assert.equal(distTag('1.2.3'), 'latest');
  assert.equal(distTag('1.2.3-rc.1'), 'next');
});

test('a normal release is the four published platforms, and nothing for windows', () => {
  const out = mkdtempSync(join(tmpdir(), 'muster-npm-'));
  const { order } = assemble(fakeBinaries('v1.0.0'), 'v1.0.0', out);
  assert.deepEqual(order, [
    '@futurelastic/muster-darwin-arm64', '@futurelastic/muster-darwin-x64',
    '@futurelastic/muster-linux-x64', '@futurelastic/muster-linux-arm64',
    '@futurelastic/muster',
  ]);
  assert.equal(existsSync(join(out, 'muster-win32-x64')), false, 'windows is not assembled');
  assert.equal(readFileSync(join(out, 'PUBLISH_ORDER'), 'utf8').includes('win32'), false);
  const m = require(join(out, 'muster', 'package.json'));
  assert.equal(Object.keys(m.optionalDependencies).some((n) => n.includes('win32')), false);
});

test('a normal release does not need the windows binaries to exist', () => {
  const dir = fakeBinaries('v1.0.0');
  for (const p of PLATFORMS.filter((x) => !x.published)) rmSync(join(dir, `muster-v1.0.0-${p.goos}-${p.goarch}${p.ext}`));
  assert.doesNotThrow(() => assemble(dir, 'v1.0.0', mkdtempSync(join(tmpdir(), 'muster-npm-'))));
  assert.throws(() => assemble(dir, 'v1.0.0', mkdtempSync(join(tmpdir(), 'muster-npm-')), { all: true }), /missing binary .*windows-amd64\.exe/);
});

test('--all assembles win32 with muster.exe, pinned and named by process.platform', () => {
  const out = mkdtempSync(join(tmpdir(), 'muster-npm-'));
  const { order } = assemble(fakeBinaries('v2.0.0'), 'v2.0.0', out, { all: true });
  assert.equal(order.at(-1), '@futurelastic/muster', 'launcher is still published last');
  assert.equal(order.length, 7);
  for (const cpu of ['x64', 'arm64']) {
    const dir = join(out, `muster-win32-${cpu}`);
    const p = require(join(dir, 'package.json'));
    assert.equal(p.name, `@futurelastic/muster-win32-${cpu}`);
    assert.deepEqual([p.os, p.cpu], [['win32'], [cpu]]);
    assert.deepEqual(p.files, ['bin/muster.exe', 'README.md', 'LICENSE']);
    assert.equal(existsSync(join(dir, 'bin', 'muster.exe')), true);
    assert.equal(existsSync(join(dir, 'bin', 'muster')), false);
    const [info] = JSON.parse(execFileSync('npm', ['pack', '--dry-run', '--json', '--ignore-scripts'], { cwd: dir, encoding: 'utf8' }));
    assert.deepEqual(info.files.map((f) => f.path).sort(), ['LICENSE', 'README.md', 'bin/muster.exe', 'package.json']);
  }
  const m = require(join(out, 'muster', 'package.json'));
  assert.equal(m.optionalDependencies['@futurelastic/muster-win32-x64'], '2.0.0');
  assert.equal(m.optionalDependencies['@futurelastic/muster-win32-arm64'], '2.0.0');
});

test('every package packs exactly its allowlist', () => {
  const out = mkdtempSync(join(tmpdir(), 'muster-npm-'));
  const { order } = assemble(fakeBinaries('v9.9.9-rc.1'), 'v9.9.9-rc.1', out);
  assert.equal(order.at(-1), '@futurelastic/muster', 'launcher is published last');
  assert.equal(order.length, 5);
  for (const name of order) {
    const dir = join(out, name.replace('@futurelastic/', ''));
    const [info] = JSON.parse(execFileSync('npm', ['pack', '--dry-run', '--json', '--ignore-scripts'], { cwd: dir, encoding: 'utf8' }));
    const files = info.files.map((f) => f.path).sort();
    const want = name === '@futurelastic/muster'
      ? ['LICENSE', 'README.md', 'bin/muster.js', 'package.json']
      : ['LICENSE', 'README.md', 'bin/muster', 'package.json'];
    assert.deepEqual(files, want, name);
  }
});

test('launcher pins every platform package to its own exact version', () => {
  const out = mkdtempSync(join(tmpdir(), 'muster-npm-'));
  assemble(fakeBinaries('v1.4.0'), 'v1.4.0', out);
  const m = require(join(out, 'muster', 'package.json'));
  assert.equal(m.version, '1.4.0');
  assert.deepEqual(Object.keys(m.optionalDependencies).sort(), [
    '@futurelastic/muster-darwin-arm64', '@futurelastic/muster-darwin-x64',
    '@futurelastic/muster-linux-arm64', '@futurelastic/muster-linux-x64',
  ]);
  for (const v of Object.values(m.optionalDependencies)) assert.equal(v, '1.4.0');
  assert.equal(m.scripts, undefined, 'no install scripts');
  const p = require(join(out, 'muster-linux-x64', 'package.json'));
  assert.deepEqual([p.os, p.cpu], [['linux'], ['x64']]);
});

test('resolveBinary: ok, unsupported, missing, mismatched', () => {
  const root = mkdtempSync(join(tmpdir(), 'muster-res-'));
  const pkg = join(root, 'muster-linux-x64');
  mkdirSync(pkg);
  writeFileSync(join(pkg, 'package.json'), JSON.stringify({ version: '1.0.0' }));
  const resolve = (id) => {
    if (id === '@futurelastic/muster-linux-x64/package.json') return join(pkg, 'package.json');
    throw new Error('not found');
  };
  // "Supported" is whatever the launcher's own optionalDependencies list.
  const deps = { '@futurelastic/muster-linux-x64': '1.0.0', '@futurelastic/muster-linux-arm64': '1.0.0' };
  assert.equal(resolveBinary('linux', 'x64', resolve, '1.0.0', deps), join(pkg, 'bin', 'muster'));
  assert.throws(() => resolveBinary('win32', 'x64', resolve, '1.0.0', deps), /no prebuilt binary for win32-x64/);
  assert.throws(() => resolveBinary('linux', 'arm64', resolve, '1.0.0', deps), /is not installed/);
  assert.throws(() => resolveBinary('linux', 'x64', resolve, '1.0.1', deps), /version mismatch/);
  assert.throws(() => resolveBinary('linux', 'x64', resolve, '1.0.0', {}), /no prebuilt binary for linux-x64 \(supported: none\)/);
});

test('resolveBinary: win32 resolves to bin/muster.exe, and only where the launcher lists it', () => {
  const root = mkdtempSync(join(tmpdir(), 'muster-res-'));
  const pkg = join(root, 'muster-win32-x64');
  mkdirSync(pkg);
  writeFileSync(join(pkg, 'package.json'), JSON.stringify({ version: '1.0.0' }));
  const resolve = (id) => {
    if (id === '@futurelastic/muster-win32-x64/package.json') return join(pkg, 'package.json');
    throw new Error('not found');
  };
  const listed = { '@futurelastic/muster-win32-x64': '1.0.0' };
  assert.equal(resolveBinary('win32', 'x64', resolve, '1.0.0', listed), join(pkg, 'bin', 'muster.exe'));
  // A launcher assembled without --all does not list win32, so it does not claim it.
  assert.throws(() => resolveBinary('win32', 'x64', resolve, '1.0.0', { '@futurelastic/muster-linux-x64': '1.0.0' }), /no prebuilt binary for win32-x64/);
  assert.deepEqual(supportedFrom({ '@futurelastic/muster-win32-arm64': '1', '@futurelastic/muster-win32-x64': '1', other: '1' }), ['win32-arm64', 'win32-x64']);
});

test('the launcher runs the platform binary end to end', { skip: process.platform === 'win32' }, () => {
  const out = mkdtempSync(join(tmpdir(), 'muster-e2e-'));
  const key = `${process.platform}-${process.arch}`;
  const plat = PLATFORMS.find((p) => `${p.os}-${p.cpu}` === key);
  if (!plat) return;
  assemble(fakeBinaries('v1.0.0'), 'v1.0.0', join(out, 'pkgs'));
  // Lay the packages out as npm would: scope dir inside node_modules.
  const nm = join(out, 'node_modules', '@futurelastic');
  mkdirSync(nm, { recursive: true });
  execFileSync('cp', ['-R', join(out, 'pkgs', 'muster'), join(nm, 'muster')]);
  execFileSync('cp', ['-R', join(out, 'pkgs', `muster-${key}`), join(nm, `muster-${key}`)]);
  const got = execFileSync('node', [join(nm, 'muster', 'bin', 'muster.js'), 'serve'], { encoding: 'utf8' });
  assert.equal(got.trim(), 'muster');
});
