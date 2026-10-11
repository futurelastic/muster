#!/usr/bin/env node
// Assemble the five npm packages for a release (#243).
//
//   node scripts/assemble-npm.mjs [--all] BINDIR TAG OUTDIR
//
//   --all   also assemble the platforms marked published:false (windows). They
//           are built by every release but not published: this flag is how the
//           Windows end-to-end job installs them, and how a later release
//           promotes them. Without it they are neither assembled, nor in
//           PUBLISH_ORDER, nor in the launcher's optionalDependencies.
//   BINDIR  where scripts/build-release.sh put muster-<TAG>-<os>-<arch>[.exe].
//   TAG     the release tag, v<digit>...; the npm version is the tag without
//           the leading "v", and is the SAME for every package.
//   OUTDIR  receives one directory per package, ready for `npm publish`:
//             muster-<os>-<cpu>/   the platform packages (os is npm's
//                                  process.platform: darwin, linux, win32)
//             muster/              the launcher
//           and a PUBLISH_ORDER file: platform packages first, launcher last,
//           so the launcher never points at a version that does not exist yet.
//
// Everything is stamped from the tag here; the manifests under npm/ carry
// version 0.0.0 on purpose, so there is no second place for a version to rot.
import { chmodSync, copyFileSync, cpSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// Go GOOS/GOARCH -> npm os/cpu. The package suffix is npm's vocabulary, i.e.
// process.platform-process.arch, which is how the launcher finds its binary:
// windows is `win32`, not `windows`.
//
// published:false marks a platform that is built and can be assembled, but is
// not part of a normal release (see --all above). `ext` is the binary's suffix,
// both in the build output name and inside the package.
export const PLATFORMS = [
  { goos: 'darwin', goarch: 'arm64', os: 'darwin', cpu: 'arm64', ext: '', published: true },
  { goos: 'darwin', goarch: 'amd64', os: 'darwin', cpu: 'x64', ext: '', published: true },
  { goos: 'linux', goarch: 'amd64', os: 'linux', cpu: 'x64', ext: '', published: true },
  { goos: 'linux', goarch: 'arm64', os: 'linux', cpu: 'arm64', ext: '', published: true },
  { goos: 'windows', goarch: 'amd64', os: 'win32', cpu: 'x64', ext: '.exe', published: false },
  { goos: 'windows', goarch: 'arm64', os: 'win32', cpu: 'arm64', ext: '.exe', published: false },
];

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const SCOPE = '@futurelastic';

export function versionFromTag(tag) {
  const m = /^v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$/.exec(tag);
  if (!m) throw new Error(`'${tag}' is not a release tag (want vMAJOR.MINOR.PATCH[-prerelease])`);
  return m[1];
}

export function distTag(version) {
  return version.includes('-') ? 'next' : 'latest';
}

export function assemble(binDir, tag, outDir, { all = false } = {}) {
  const version = versionFromTag(tag);
  rmSync(outDir, { recursive: true, force: true });
  mkdirSync(outDir, { recursive: true });
  const order = [];
  const optional = {};

  for (const p of PLATFORMS) {
    if (!p.published && !all) continue;
    const suffix = `${p.os}-${p.cpu}`;
    const binName = `muster${p.ext}`;
    const src = join(binDir, `muster-${tag}-${p.goos}-${p.goarch}${p.ext}`);
    if (!existsSync(src)) throw new Error(`missing binary ${src}`);
    const dir = join(outDir, `muster-${suffix}`);
    mkdirSync(join(dir, 'bin'), { recursive: true });
    copyFileSync(src, join(dir, 'bin', binName));
    chmodSync(join(dir, 'bin', binName), 0o755);
    const fill = (s) => s
      .replaceAll('@PLATFORM@', suffix).replaceAll('@OS@', p.os).replaceAll('@CPU@', p.cpu).replaceAll('@BIN@', binName);
    const manifest = JSON.parse(fill(readFileSync(join(root, 'npm/platform/package.json'), 'utf8')));
    manifest.version = version;
    writeFileSync(join(dir, 'package.json'), JSON.stringify(manifest, null, 2) + '\n');
    writeFileSync(join(dir, 'README.md'), fill(readFileSync(join(root, 'npm/platform/README.md'), 'utf8')));
    copyFileSync(join(root, 'LICENSE'), join(dir, 'LICENSE'));
    optional[`${SCOPE}/muster-${suffix}`] = version; // exact, never a range
    order.push(`${SCOPE}/muster-${suffix}`);
  }

  const dir = join(outDir, 'muster');
  cpSync(join(root, 'npm/launcher/bin'), join(dir, 'bin'), { recursive: true });
  chmodSync(join(dir, 'bin', 'muster.js'), 0o755);
  const manifest = JSON.parse(readFileSync(join(root, 'npm/launcher/package.json'), 'utf8'));
  manifest.version = version;
  manifest.optionalDependencies = optional;
  writeFileSync(join(dir, 'package.json'), JSON.stringify(manifest, null, 2) + '\n');
  copyFileSync(join(root, 'README.md'), join(dir, 'README.md'));
  copyFileSync(join(root, 'LICENSE'), join(dir, 'LICENSE'));
  order.push(`${SCOPE}/muster`);

  writeFileSync(join(outDir, 'PUBLISH_ORDER'), order.map((n) => n.replace(`${SCOPE}/`, '')).join('\n') + '\n');
  return { version, order };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const rest = process.argv.slice(2);
  const all = rest.includes('--all');
  const args = rest.filter((a) => a !== '--all');
  if (args.length !== 3) {
    console.error('usage: node scripts/assemble-npm.mjs [--all] BINDIR TAG OUTDIR');
    process.exit(2);
  }
  try {
    const { version, order } = assemble(resolve(args[0]), args[1], resolve(args[2]), { all });
    console.log(`assemble-npm: ${version} (dist-tag ${distTag(version)}): ${order.join(', ')}`);
  } catch (err) {
    console.error(`assemble-npm: ${err.message}`);
    process.exit(1);
  }
}
