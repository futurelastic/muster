#!/usr/bin/env node
// Publishes one FINAL muster release to the company release host (#383), next to npm.
//
//   node publish.mjs stage   <distdir> <outdir> <tag>     choose the files to publish and write their SHA256SUMS
//   node publish.mjs publish <dir> <tag> [--dry-run]      sign the sums and upload them through the host's CI publish API
//   node publish.mjs keygen                               make a signing key: the SEED on stdout, the PUBLIC key on stderr
//
// WHAT IS PUBLISHED. The four unix binaries scripts/build-release.sh writes
// (muster-<tag>-<os>-<arch>), and nothing else: the Windows binaries stay on npm's side of the line until the
// Windows package work lands. The host's folder for the release is the version without its "v"; the files keep the
// build script's names, so `sha256sum -c SHA256SUMS` run next to a download verifies it with nobody renaming anything.
//
// THE PROTOCOL (the host's "repos publish from CI" contract; this file is the only caller in this repo):
//   begin     POST /api/v1/publish/items/<slug>/uploads   {version, files, sums, sig, manifest, install_sh}
//                                                         proof: the GitHub Actions OIDC token, audience = PUBLISH_ORIGIN
//   parts     PUT  /api/v1/publish/uploads/<id>/files/<name>?offset=N   raw bytes, proof: the token `begin` returned
//   complete  POST /api/v1/publish/uploads/<id>/complete
// `sig` is ed25519 over "library release v1\n<slug>\n<version>\n" followed by the exact SHA256SUMS bytes. The slug
// and the version are inside the signed bytes, so a signature cannot be replayed on another item or version.
// A re-run of a complete version answers "already published", which is success. A run that died part-way is
// resumed by beginning again: the host says how many bytes of each file it already holds.
//
// CONFIGURATION. Nothing about the host is written in this repository.
//   PUBLISH_ORIGIN       the host's exact address (https://..., no trailing slash). It is also the token's audience.
//   PUBLISH_SLUG         the item this repo is bound to (default "muster").
//   PUBLISH_SIGNING_KEY  the ed25519 seed, 64 hex. Held only by the workflow's `release` environment; read from the
//                        environment, never from an argument, never printed.
// and, set by GitHub Actions for a job with `id-token: write`: ACTIONS_ID_TOKEN_REQUEST_URL / _TOKEN.
//
// Node built-ins only, like the rest of this repository's tooling.
import {
  createHash, createPrivateKey, createPublicKey, randomBytes, sign as edSign, verify as edVerify,
} from 'node:crypto'
import { closeSync, copyFileSync, existsSync, mkdirSync, openSync, readFileSync, readSync, statSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const HERE = fileURLToPath(new URL('.', import.meta.url))

/** The platforms published, in the order they are listed. */
export const TARGETS = ['darwin-arm64', 'darwin-amd64', 'linux-amd64', 'linux-arm64']
/** A final tag. A candidate (-rc.N) is never published here: the host refuses it too. */
const FINAL_TAG_RE = /^v(\d+\.\d+\.\d+)$/
/** What the host accepts as a file name (its RELEASE_FILE_RE). */
const FILE_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._+@=-]{0,199}$/
const SEED_RE = /^[0-9a-f]{64}$/
const ED25519_PKCS8_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex')
const ED25519_SPKI_PREFIX = Buffer.from('302a300506032b6570032100', 'hex')

export class PublishError extends Error {}
const fail = (message) => { throw new PublishError(message) }

// ---- the pure parts ---------------------------------------------------------------------------------------------------

/** "v1.2.3" -> "1.2.3"; anything but a final tag is refused. */
export function versionOf(tag) {
  const m = FINAL_TAG_RE.exec(String(tag))
  if (!m) fail(`'${tag}' is not a final tag (want vMAJOR.MINOR.PATCH): only finals are published`)
  return m[1]
}

export function fileNames(tag) {
  return TARGETS.map((t) => `muster-${tag}-${t}`)
}

export function sha256File(path) {
  const h = createHash('sha256')
  const fd = openSync(path, 'r')
  try {
    const buf = Buffer.allocUnsafe(1 << 20)
    for (let n; (n = readSync(fd, buf, 0, buf.length, null)) > 0;) h.update(buf.subarray(0, n))
  } finally {
    closeSync(fd)
  }
  return h.digest('hex')
}

/** `<sha256>  <name>` per line, sorted by name, newline-terminated: what is signed is these exact bytes. */
export function sumsText(entries) {
  return [...entries].sort((a, b) => (a.name < b.name ? -1 : 1)).map((e) => `${e.sha256}  ${e.name}\n`).join('')
}

/** name -> sha256, from a SHA256SUMS text. Strict: the host parses it the same way and signs what it reads. */
export function parseSums(text) {
  if (text.length === 0 || !text.endsWith('\n') || text.includes('\r')) fail('SHA256SUMS must be non-empty lines ending in a newline')
  const out = new Map()
  for (const line of text.slice(0, -1).split('\n')) {
    const m = /^([0-9a-f]{64}) {2}(.+)$/.exec(line)
    if (!m) fail(`SHA256SUMS has a malformed line: ${JSON.stringify(line)}`)
    if (!FILE_NAME_RE.test(m[2]) || m[2] === 'SHA256SUMS' || m[2] === 'SHA256SUMS.sig') fail(`SHA256SUMS names a file the host would not serve: ${m[2]}`)
    if (out.has(m[2])) fail(`SHA256SUMS lists ${m[2]} twice`)
    out.set(m[2], m[1])
  }
  return out
}

/** The bytes the signature covers. */
export function signedMessage(slug, version, sums) {
  return Buffer.concat([Buffer.from(`library release v1\n${slug}\n${version}\n`, 'utf8'), Buffer.from(sums, 'utf8')])
}

export function privateKeyFromSeed(seedHex) {
  if (!SEED_RE.test(seedHex)) fail('the signing key must be an ed25519 seed: 64 lowercase hex characters')
  return createPrivateKey({ key: Buffer.concat([ED25519_PKCS8_PREFIX, Buffer.from(seedHex, 'hex')]), format: 'der', type: 'pkcs8' })
}

export function publicKeyHex(privateKey) {
  const der = createPublicKey(privateKey).export({ format: 'der', type: 'spki' })
  return Buffer.from(der.subarray(der.length - 32)).toString('hex')
}

export function signSums(privateKey, slug, version, sums) {
  return edSign(null, signedMessage(slug, version, sums), privateKey).toString('hex')
}

export function verifySums(publicHex, slug, version, sums, sigHex) {
  const key = createPublicKey({ key: Buffer.concat([ED25519_SPKI_PREFIX, Buffer.from(publicHex, 'hex')]), format: 'der', type: 'spki' })
  return edVerify(null, signedMessage(slug, version, sums), key, Buffer.from(sigHex, 'hex'))
}

/** A new seed and its public key. */
export function newKey() {
  const seed = randomBytes(32).toString('hex')
  return { seed, publicHex: publicKeyHex(privateKeyFromSeed(seed)) }
}

// ---- stage ------------------------------------------------------------------------------------------------------------

/**
 * Picks the published files out of build-release.sh's output, re-hashes each against the build's own SHA256SUMS
 * (a file changed between the build and here is refused), copies them to outDir and writes the published set's
 * SHA256SUMS beside them. Returns the entries.
 */
export function stage(distDir, outDir, tag) {
  versionOf(tag)
  const built = parseSums(readFileSync(join(distDir, 'SHA256SUMS'), 'utf8'))
  mkdirSync(outDir, { recursive: true })
  const entries = []
  for (const name of fileNames(tag)) {
    const want = built.get(name)
    if (want === undefined) fail(`${name} is not in the build's SHA256SUMS: the release did not build it`)
    const src = join(distDir, name)
    if (!existsSync(src)) fail(`${name} is in the build's SHA256SUMS and missing from ${distDir}`)
    const got = sha256File(src)
    if (got !== want) fail(`${name} does not match the build's SHA256SUMS (expected ${want}, got ${got})`)
    copyFileSync(src, join(outDir, name))
    entries.push({ name, sha256: got, size: statSync(src).size })
  }
  writeFileSync(join(outDir, 'SHA256SUMS'), sumsText(entries))
  return entries
}

// ---- publish ----------------------------------------------------------------------------------------------------------

function settings(env) {
  const origin = env.PUBLISH_ORIGIN ?? ''
  if (!/^https?:\/\/[^/\s]+$/.test(origin)) fail('PUBLISH_ORIGIN must be the host\'s exact origin (https://host, no path and no trailing slash): it is also the token\'s audience')
  return { origin, slug: env.PUBLISH_SLUG || 'muster', seed: (env.PUBLISH_SIGNING_KEY ?? '').trim() }
}

/** Reads a directory staged by `stage` and checks it against its own SHA256SUMS. */
export function readStaged(dir, tag) {
  const sums = readFileSync(join(dir, 'SHA256SUMS'), 'utf8')
  const table = parseSums(sums)
  const expected = new Set(fileNames(tag))
  const files = []
  for (const [name, sha256] of table) {
    if (!expected.has(name)) fail(`${name} is in SHA256SUMS but is not one of the files published for ${tag}`)
    const path = join(dir, name)
    if (!existsSync(path)) fail(`${name} is in SHA256SUMS and missing from ${dir}`)
    if (sha256File(path) !== sha256) fail(`${name} does not match SHA256SUMS: the artifact changed after it was staged`)
    files.push({ name, size: statSync(path).size, path })
  }
  for (const name of expected) if (!table.has(name)) fail(`${name} is not in SHA256SUMS`)
  return { sums, files }
}

function installerText(origin, downloadsDir) {
  const text = readFileSync(join(downloadsDir, 'install.sh'), 'utf8')
  if (!text.includes('@RELEASE_ORIGIN@')) fail('install.sh has no @RELEASE_ORIGIN@ placeholder to stamp')
  return text.replaceAll('@RELEASE_ORIGIN@', origin)
}

async function oidcToken(env, audience) {
  const url = env.ACTIONS_ID_TOKEN_REQUEST_URL
  const bearer = env.ACTIONS_ID_TOKEN_REQUEST_TOKEN
  if (!url || !bearer) fail('no GitHub Actions OIDC token is available: the job needs `permissions: id-token: write`')
  const u = new URL(url)
  u.searchParams.set('audience', audience)
  const res = await fetch(u, { headers: { Authorization: `Bearer ${bearer}`, Accept: 'application/json; api-version=2.0' } })
  if (!res.ok) fail(`GitHub would not issue an OIDC token (HTTP ${res.status})`)
  const body = await res.json()
  if (typeof body.value !== 'string' || !body.value) fail('GitHub answered without an OIDC token')
  return body.value
}

class HostError extends PublishError {
  constructor(status, code, message) {
    super(`${code ? `${code}: ` : ''}${message}`)
    this.status = status
    this.code = code
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const RETRIES = Number(process.env.PUBLISH_RETRIES ?? 3)
const PAUSE_MS = Number(process.env.PUBLISH_RETRY_PAUSE_MS ?? 2000)
/** A 5xx, a 429 or a dropped connection may pass; a 4xx the host chose to send will not. */
const transient = (e) => !(e instanceof HostError) || e.status >= 500 || e.status === 429

async function call(url, init) {
  let res
  try {
    res = await fetch(url, init)
  } catch (e) {
    throw new HostError(0, 'NETWORK', `${init.method} ${new URL(url).pathname}: ${e.cause?.code ?? e.message}`)
  }
  const text = await res.text()
  let body = null
  try { body = text ? JSON.parse(text) : null } catch { /* not JSON: reported below */ }
  if (!res.ok) throw new HostError(res.status, body?.error ?? '', body?.message ?? `${init.method} ${new URL(url).pathname} answered HTTP ${res.status}`)
  return { status: res.status, body: body?.data ?? body ?? {} }
}

async function retrying(label, fn) {
  for (let attempt = 1; ; attempt++) {
    try {
      return await fn()
    } catch (e) {
      if (!transient(e) || attempt >= RETRIES) throw e
      console.log(`${label}: ${e.message} — retrying (${attempt}/${RETRIES - 1})`)
      await sleep(PAUSE_MS * attempt)
    }
  }
}

function readPart(path, offset, length) {
  const fd = openSync(path, 'r')
  try {
    const buf = Buffer.allocUnsafe(length)
    let got = 0
    while (got < length) {
      const n = readSync(fd, buf, got, length - got, offset + got)
      if (n === 0) break
      got += n
    }
    return buf.subarray(0, got)
  } finally {
    closeSync(fd)
  }
}

export async function publish(dir, tag, { dryRun = false, env = process.env, downloadsDir = HERE, log = console.log } = {}) {
  const version = versionOf(tag)
  const { origin, slug, seed } = settings(env)
  // The host only accepts a run whose ref is this tag: say so before any build or signing, not after a refusal.
  const ref = env.GITHUB_REF
  if (!dryRun && ref !== undefined && ref !== `refs/tags/${tag}`) {
    fail(`this run is at ${ref}, not refs/tags/${tag}: the host accepts a publish only from a run at the final tag itself`)
  }
  if (!seed) fail('PUBLISH_SIGNING_KEY is not set: it lives in the `release` environment')
  const key = privateKeyFromSeed(seed)
  const { sums, files } = readStaged(dir, tag)
  const sig = signSums(key, slug, version, sums)
  if (!verifySums(publicKeyHex(key), slug, version, sums, sig)) fail('the signature does not verify against its own public key')
  const manifest = JSON.parse(readFileSync(join(downloadsDir, 'manifest.json'), 'utf8'))
  const install_sh = installerText(origin, downloadsDir)

  log(`${slug} ${version}: ${files.length} files, ${files.reduce((n, f) => n + f.size, 0)} bytes; signing key ${publicKeyHex(key)}`)
  for (const f of files) log(`  ${f.name}  ${f.size}`)
  if (dryRun) {
    log('dry run: signed and checked locally; nothing was sent')
    return { status: 'dry-run' }
  }

  const begun = await retrying('begin', async () => {
    // A token is spent by the attempt that reaches the host, so every attempt asks GitHub for a fresh one.
    const token = await oidcToken(env, origin)
    return call(`${origin}/api/v1/publish/items/${encodeURIComponent(slug)}/uploads`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ version, files: files.map(({ name, size }) => ({ name, size })), sums, sig, manifest, install_sh }),
    })
  })
  if (begun.body.status === 'published') {
    log(`${slug} ${version} is already on the host: nothing to send`)
    return { status: 'already' }
  }
  const { id, token: uploadToken, partBytes } = begun.body
  if (!id || !uploadToken || !Number.isSafeInteger(partBytes) || partBytes < 1) fail('the host answered the begin call without an upload id, token and part size')
  log(`upload ${id} ${begun.body.resumed ? 'resumed' : 'begun'}`)
  const auth = { Authorization: `Bearer ${uploadToken}` }

  // Where the host is, per file: the begin answer, then — after a failed part — a fresh read of it.
  let held = new Map((begun.body.files ?? []).map((f) => [f.name, f.received ?? 0]))
  const reread = async () => {
    const r = await call(`${origin}/api/v1/publish/uploads/${encodeURIComponent(id)}`, { method: 'GET', headers: auth })
    held = new Map((r.body.files ?? []).map((f) => [f.name, f.received ?? 0]))
  }
  for (const f of files) {
    let offset = held.get(f.name) ?? 0
    let stalls = 0 // failures in a row without the host moving forward
    while (offset < f.size) {
      const chunk = readPart(f.path, offset, Math.min(partBytes, f.size - offset))
      try {
        const r = await call(
          `${origin}/api/v1/publish/uploads/${encodeURIComponent(id)}/files/${encodeURIComponent(f.name)}?offset=${offset}`,
          { method: 'PUT', headers: { ...auth, 'Content-Type': 'application/octet-stream' }, body: chunk },
        )
        offset = Number.isSafeInteger(r.body.received) ? r.body.received : offset + chunk.length
        stalls = 0
      } catch (e) {
        if (!transient(e) || ++stalls >= RETRIES) throw e
        log(`${f.name}: ${e.message} — asking the host where it is (${stalls}/${RETRIES - 1})`)
        await sleep(PAUSE_MS * stalls)
        // A part the host may or may not have stored: read where it is instead of guessing the next offset.
        try {
          await reread()
          offset = held.get(f.name) ?? offset
        } catch (re) {
          if (!transient(re)) throw re
        }
      }
    }
    log(`  sent ${f.name}`)
  }

  const done = await retrying('complete', () => call(`${origin}/api/v1/publish/uploads/${encodeURIComponent(id)}/complete`, { method: 'POST', headers: auth }))
  if (done.body.status !== 'published') fail(`the host did not report the version as published: ${JSON.stringify(done.body)}`)
  log(`${slug} ${version} published${done.body.already ? ' (it already was)' : ''}`)
  return { status: 'published' }
}

// ---- the command line -------------------------------------------------------------------------------------------------

const USAGE = `usage: publish.mjs stage <distdir> <outdir> <tag>
       publish.mjs publish <dir> <tag> [--dry-run]
       publish.mjs keygen`

async function main(argv) {
  const [cmd, ...rest] = argv
  const flags = rest.filter((a) => a.startsWith('--'))
  const args = rest.filter((a) => !a.startsWith('--'))
  switch (cmd) {
    case 'stage': {
      if (args.length !== 3 || flags.length) fail(USAGE)
      const entries = stage(args[0], args[1], args[2])
      for (const e of entries) console.log(`staged ${e.name} ${e.size}`)
      return
    }
    case 'publish': {
      if (args.length !== 2 || flags.some((f) => f !== '--dry-run')) fail(USAGE)
      await publish(args[0], args[1], { dryRun: flags.includes('--dry-run') })
      return
    }
    case 'keygen': {
      if (args.length || flags.length) fail(USAGE)
      const { seed, publicHex } = newKey()
      process.stdout.write(`${seed}\n`)
      process.stderr.write(`public key: ${publicHex}\n`)
      return
    }
    default:
      fail(USAGE)
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((e) => {
    console.error(e instanceof PublishError ? `publish: ${e.message}` : e)
    process.exit(1)
  })
}
