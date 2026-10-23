#!/usr/bin/env node
// Publish one FINAL version's files to a self-hosted downloads site — TEMPLATE (colab-handbook #648).
// Copy me into your repo beside the workflow (`colab template publish-downloads.mjs --dest
// .github/downloads/publish-downloads.mjs`); from then on YOU OWN THIS FILE. Node >= 18, zero dependencies,
// shells out to `rsync` (and `ssh` through it). Docs: docs/downloads.md in the handbook.
//
//   node publish-downloads.mjs check                                   validate the downloads: block and the manifest
//   node publish-downloads.mjs names   vX.Y.Z                          the Release files this publishes, one per line
//   node publish-downloads.mjs publish vX.Y.Z <assetsDir> [--dry-run]  stage, upload, read back
//   (every command also takes --root <repo root>, default the current directory)
//
// WHAT IS PUBLISHED comes from the repo's `.github/project.yml`:
//
//   downloads:
//     app: myapp                        # the app's folder on the site: [a-z0-9][a-z0-9-]*
//     files:                            # every final carries these; {v} is X.Y.Z
//       - myapp-{v}.tgz
//       - MyApp_{v}_aarch64.dmg
//     optional: [MyApp_{v}_x64.msi]     # published when the Release has it (optional key)
//     installers:                       # repo paths, uploaded to /<app>/ with @DOWNLOADS_URL@ stamped (optional key)
//       - scripts/install.sh
//     manifest: .github/downloads/manifest.json   # optional key, this is the default path
//
// Only that subset of YAML is read here (scalars, `- item` lists, `[a, b]` lists, comments): this script has no
// YAML library on purpose, and refuses — naming the line — anything else inside the block.
//
// EVERYTHING ABOUT THE SITE COMES FROM THE ENVIRONMENT; nothing about it is written in the repository:
//
//   DOWNLOADS_URL                   the site's root address (a repo variable); /<app> is appended here
//   DOWNLOADS_SSH_HOST/PORT/USER    where rsync goes (repo secrets). The account is restricted to the app's
//                                   folder server-side and cannot delete: its root IS /<app>/
//   DOWNLOADS_KEY_FILE              the private key file the workflow wrote from DOWNLOADS_SSH_KEY
//   DOWNLOADS_KNOWN_HOSTS_FILE      the known-hosts file the workflow wrote from DOWNLOADS_SSH_KNOWN_HOSTS
//   DOWNLOADS_RESERVED              names the site keeps for itself, refused as an app slug (space/comma list)
//
// THE ORDER ON THE SITE, which is what makes a half-finished run harmless:
//   1. <X.Y.Z>/ with every file except SHA256SUMS        (rsync --ignore-existing: a published file is never replaced)
//   2. every file read back over HTTPS against the SHA256SUMS this build computed
//   3. <X.Y.Z>/SHA256SUMS                                (its presence = the folder is complete)
//   4. the installers (address stamped in), then manifest.json
//   5. latest.json LAST                                  (unless the site already names a newer version)
//   6. SHA256SUMS and every file sent in 4–5 read back from the site
// A published version is immutable: a folder whose SHA256SUMS differs from this build's is an error and nothing is
// changed. A complete folder is verified and left alone; a partial one gets only its missing files. Installers,
// manifest.json and latest.json are sent only when the site's copy differs, so a re-run is a verified no-op.
// Never a candidate: the tag must be `vX.Y.Z`.
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { lookup } from 'node:dns/promises'
import {
  copyFileSync,
  createReadStream,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  realpathSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { basename, join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

const FINAL_TAG = /^v(\d+\.\d+\.\d+)$/
const SLUG = /^[a-z0-9][a-z0-9-]*$/
const PLATFORMS = ['macos', 'windows', 'linux']
export const PLACEHOLDER = '@DOWNLOADS_URL@'
export const DEFAULT_MANIFEST = '.github/downloads/manifest.json'
/** Names the script itself writes at the app's root; an installer may not take one. */
const ROOT_NAMES = new Set(['latest.json', 'manifest.json'])

/** The version of a final tag. A candidate (`vX.Y.Z-rc.N`) and anything else is refused. */
export function finalVersion(tag) {
  const m = typeof tag === 'string' ? FINAL_TAG.exec(tag) : null
  if (!m)
    throw new Error(
      `${JSON.stringify(tag)} is not a final tag: expected vX.Y.Z (a candidate is never published)`,
    )
  return m[1]
}

// ── the downloads: block ─────────────────────────────────────────────────────────────────────────

function scalar(raw, where) {
  let s = raw.trim()
  if (s.startsWith('"') || s.startsWith("'")) {
    const q = s[0]
    const end = s.indexOf(q, 1)
    if (end < 0) throw new Error(`${where}: unterminated quote`)
    const rest = s.slice(end + 1).trim()
    if (rest && !rest.startsWith('#')) throw new Error(`${where}: text after the closing quote`)
    return s.slice(1, end)
  }
  const hash = s.search(/\s#/)
  if (hash >= 0) s = s.slice(0, hash).trim()
  if (s.startsWith('#')) s = ''
  return s
}

function flowList(raw, where) {
  const s = scalar(raw.replace(/\s+#.*$/, ''), where)
  if (!s.startsWith('[') || !s.endsWith(']')) return null
  const inner = s.slice(1, -1).trim()
  return inner ? inner.split(',').map((x) => scalar(x, where)) : []
}

/**
 * The `downloads:` block of project.yml text, as a plain object — or null when the repo declares none. Reads the
 * subset documented in the header and throws, naming the line, on anything else inside the block.
 */
export function parseDownloadsBlock(text) {
  const lines = text.split(/\r?\n/)
  const start = lines.findIndex((l) => /^downloads:\s*(#.*)?$/.test(l))
  if (start < 0) {
    const inline = lines.findIndex((l) => /^downloads:\s*\S/.test(l))
    if (inline >= 0)
      throw new Error(
        `project.yml line ${inline + 1}: write downloads: as a block (one key per line), not inline`,
      )
    return null
  }
  const out = {}
  let keyIndent = null
  let listKey = null
  for (let i = start + 1; i < lines.length; i++) {
    const line = lines[i]
    const where = `project.yml line ${i + 1}`
    if (/^\s*(#.*)?$/.test(line)) continue
    const indent = line.match(/^ */)[0].length
    if (indent === 0) break // the next top-level key ends the block
    if (line.includes('\t')) throw new Error(`${where}: a tab in the downloads: block`)
    const body = line.slice(indent)
    if (keyIndent === null) keyIndent = indent
    if (indent > keyIndent && listKey && body.startsWith('- ')) {
      out[listKey].push(scalar(body.slice(2), where))
      continue
    }
    if (indent === keyIndent && listKey && body.startsWith('- ')) {
      // `- item` at the key's own indent is valid YAML for a list under that key
      out[listKey].push(scalar(body.slice(2), where))
      continue
    }
    const m = indent === keyIndent ? /^([a-z][a-z-]*):(.*)$/.exec(body) : null
    if (!m) throw new Error(`${where}: not a form this script reads inside downloads: (see its header)`)
    const [, key, rest] = m
    if (key in out) throw new Error(`${where}: downloads.${key} is given twice`)
    const value = scalar(rest, where)
    if (value === '') {
      out[key] = []
      listKey = key
      continue
    }
    listKey = null
    const list = flowList(rest, where)
    out[key] = list ?? value
  }
  return out
}

const KEYS = ['app', 'files', 'optional', 'installers', 'manifest']

function fileName(pattern, where) {
  if (typeof pattern !== 'string' || pattern === '') throw new Error(`${where}: an empty name`)
  if (/[/\\]/.test(pattern) || pattern.startsWith('.') || pattern === 'SHA256SUMS')
    throw new Error(`${where}: ${JSON.stringify(pattern)} is not a plain file name`)
  return pattern
}

/** Validates a parsed block (see parseDownloadsBlock) and returns it normalised. */
export function readConfig(block, { reserved = [] } = {}) {
  if (!block) throw new Error('.github/project.yml has no downloads: block — this repo publishes nothing')
  for (const k of Object.keys(block))
    if (!KEYS.includes(k)) throw new Error(`downloads.${k} is not a key (known: ${KEYS.join(', ')})`)
  const app = block.app
  if (typeof app !== 'string' || app === '') throw new Error('downloads.app is required')
  if (app.startsWith('_')) throw new Error(`downloads.app ${JSON.stringify(app)}: names starting with _ belong to the site`)
  if (!SLUG.test(app)) throw new Error(`downloads.app ${JSON.stringify(app)} is not a slug: [a-z0-9][a-z0-9-]*`)
  if (reserved.includes(app)) throw new Error(`downloads.app ${JSON.stringify(app)} is a name the site reserves for itself`)
  const list = (k, required) => {
    const v = block[k]
    if (v === undefined && !required) return []
    if (!Array.isArray(v) || (required && v.length === 0))
      throw new Error(`downloads.${k} must be a ${required ? 'non-empty ' : ''}list`)
    return v
  }
  const files = list('files', true).map((p) => fileName(p, 'downloads.files'))
  const optional = list('optional', false).map((p) => fileName(p, 'downloads.optional'))
  const all = [...files, ...optional]
  const dup = all.find((n, i) => all.indexOf(n) !== i)
  if (dup) throw new Error(`downloads: ${dup} is listed twice`)
  const installers = list('installers', false)
  const seen = new Set()
  for (const p of installers) {
    const b = basename(String(p))
    if (!p || p.startsWith('/') || String(p).split(/[/\\]/).includes('..'))
      throw new Error(`downloads.installers: ${JSON.stringify(p)} is not a path inside the repo`)
    if (ROOT_NAMES.has(b) || /^\d+\.\d+\.\d+$/.test(b) || seen.has(b))
      throw new Error(`downloads.installers: ${b} would collide with another file at the app's root`)
    seen.add(b)
  }
  const manifest = block.manifest ?? DEFAULT_MANIFEST
  if (typeof manifest !== 'string' || manifest.startsWith('/') || manifest.split(/[/\\]/).includes('..'))
    throw new Error('downloads.manifest must be a path inside the repo')
  return { app, files, optional, installers, manifest }
}

export const namesFor = (patterns, v) => patterns.map((p) => p.split('{v}').join(v))

// ── manifest.json ────────────────────────────────────────────────────────────────────────────────

/** A text the portal shows: a string, or one string per language code (`{"en": "…", "vi": "…"}`). */
function isText(v, oneLine = false) {
  const ok = (s) => typeof s === 'string' && s.trim() !== '' && (!oneLine || !/[\r\n]/.test(s))
  if (typeof v === 'string') return ok(v)
  return (
    v !== null &&
    typeof v === 'object' &&
    !Array.isArray(v) &&
    Object.keys(v).length > 0 &&
    Object.entries(v).every(([k, s]) => /^[a-z]{2}(-[A-Z]{2})?$/.test(k) && ok(s))
  )
}

/** Throws naming every problem with a manifest object; returns it unchanged when valid. Fields: docs/downloads.md. */
export function validateManifest(m) {
  const problems = []
  if (m === null || typeof m !== 'object' || Array.isArray(m)) throw new Error('manifest.json is not a JSON object')
  for (const k of Object.keys(m))
    if (!['schema', 'name', 'summary', 'install'].includes(k)) problems.push(`unknown field "${k}"`)
  if (m.schema !== undefined && m.schema !== 1) problems.push('schema must be 1')
  if (!isText(m.name, true)) problems.push('name: a one-line text is required')
  if (!isText(m.summary, true)) problems.push('summary: a one-line text is required')
  const inst = m.install
  if (inst === null || typeof inst !== 'object' || Array.isArray(inst) || Object.keys(inst).length === 0) {
    problems.push(`install: an object keyed by platform (${PLATFORMS.join(', ')}) is required`)
  } else {
    for (const [os, how] of Object.entries(inst)) {
      if (!PLATFORMS.includes(os)) {
        problems.push(`install.${os}: not a platform (${PLATFORMS.join(', ')})`)
        continue
      }
      if (how === null || typeof how !== 'object' || Array.isArray(how)) {
        problems.push(`install.${os}: an object with steps (and optional prompts)`)
        continue
      }
      for (const k of Object.keys(how))
        if (!['steps', 'prompts'].includes(k)) problems.push(`install.${os}.${k}: unknown field`)
      if (!Array.isArray(how.steps) || how.steps.length === 0 || !how.steps.every((s) => isText(s)))
        problems.push(`install.${os}.steps: a non-empty list of texts`)
      if (how.prompts !== undefined && (!Array.isArray(how.prompts) || !how.prompts.every((s) => isText(s))))
        problems.push(`install.${os}.prompts: a list of texts`)
    }
  }
  if (problems.length) throw new Error(`manifest.json: ${problems.join('; ')}`)
  return m
}

/** Reads and validates the repo's config + manifest. */
export function loadRepo(root, env = {}) {
  const yml = join(root, '.github', 'project.yml')
  if (!existsSync(yml)) throw new Error(`${yml} does not exist`)
  const reserved = String(env.DOWNLOADS_RESERVED ?? '')
    .split(/[\s,]+/)
    .filter(Boolean)
  const config = readConfig(parseDownloadsBlock(readFileSync(yml, 'utf8')), { reserved })
  const mPath = join(root, config.manifest)
  if (!existsSync(mPath)) throw new Error(`${config.manifest} does not exist — every app on the site carries a manifest`)
  let manifest
  try {
    manifest = JSON.parse(readFileSync(mPath, 'utf8'))
  } catch (error) {
    throw new Error(`${config.manifest} is not JSON: ${error.message}`)
  }
  validateManifest(manifest)
  for (const p of config.installers) {
    const f = join(root, p)
    if (!existsSync(f)) throw new Error(`downloads.installers: ${p} does not exist`)
    if (!readFileSync(f, 'utf8').includes(PLACEHOLDER))
      throw new Error(`${p} has no ${PLACEHOLDER} to stamp the downloads address into`)
  }
  return { config, manifest }
}

// ── staging ──────────────────────────────────────────────────────────────────────────────────────

export function sha256File(path) {
  return new Promise((resolveHash, reject) => {
    const h = createHash('sha256')
    createReadStream(path)
      .on('data', (c) => h.update(c))
      .on('error', reject)
      .on('end', () => resolveHash(h.digest('hex')))
  })
}

/** Compares X.Y.Z versions: negative when a < b. */
export function compareVersions(a, b) {
  const pa = a.split('.').map(Number)
  const pb = b.split('.').map(Number)
  for (let i = 0; i < 3; i++) if (pa[i] !== pb[i]) return pa[i] - pb[i]
  return 0
}

export function siteRoot(baseUrl) {
  if (typeof baseUrl !== 'string' || !/^https:\/\/[^\s'"\\]+$/.test(baseUrl)) {
    throw new Error('DOWNLOADS_URL must be an https address (the repo variable is missing or malformed)')
  }
  return baseUrl.replace(/\/+$/, '')
}

/**
 * Lays out what goes to the site under `outDir`: `<X.Y.Z>/` (the files + a combined SHA256SUMS), the installers with
 * the site root stamped in, manifest.json and latest.json. Throws, uploading nothing, when a required file is missing.
 */
export async function stage({ tag, assetsDir, outDir, baseUrl, root, config, manifest }) {
  const version = finalVersion(tag)
  const base = siteRoot(baseUrl)
  const present = new Set(readdirSync(assetsDir))
  const missing = namesFor(config.files, version).filter((n) => !present.has(n))
  if (missing.length > 0) {
    throw new Error(
      `the Release of ${tag} lacks ${missing.join(', ')} — nothing was published; finish the builds, then dispatch the publish again`,
    )
  }
  const names = [...namesFor(config.files, version), ...namesFor(config.optional, version).filter((n) => present.has(n))]
  const folder = join(outDir, version)
  mkdirSync(folder, { recursive: true })
  const files = {}
  for (const name of names) {
    copyFileSync(join(assetsDir, name), join(folder, name))
    files[name] = { sha256: await sha256File(join(folder, name)), size: statSync(join(folder, name)).size }
  }
  const sums = Object.keys(files)
    .sort()
    .map((n) => `${files[n].sha256}  ${n}\n`)
    .join('')
  writeFileSync(join(folder, 'SHA256SUMS'), sums)
  const rootFiles = []
  for (const p of config.installers) {
    const source = readFileSync(join(root, p), 'utf8')
    writeFileSync(join(outDir, basename(p)), source.split(PLACEHOLDER).join(base))
    rootFiles.push(basename(p))
  }
  writeFileSync(join(outDir, 'manifest.json'), `${JSON.stringify(manifest, null, 2)}\n`)
  rootFiles.push('manifest.json')
  writeFileSync(join(outDir, 'latest.json'), `${JSON.stringify({ version, files }, null, 2)}\n`)
  return { version, files, sums, rootFiles }
}

// ── the site ─────────────────────────────────────────────────────────────────────────────────────

function runRsync({ rsync, sshArgs, args, log }) {
  const full = [...(sshArgs ? ['-e', sshArgs] : []), ...args]
  log(`$ rsync ${full.join(' ')}`)
  const r = spawnSync(rsync, full, { encoding: 'utf8' })
  if (r.status !== 0) throw new Error(`rsync failed (${r.status ?? r.signal}): ${(r.stderr || r.stdout || '').trim()}`)
}

async function get(fetchImpl, url, attempts = 3) {
  let last
  for (let i = 0; i < attempts; i++) {
    try {
      const res = await fetchImpl(url, { headers: { 'cache-control': 'no-cache' } })
      if (res.status < 500) return res
      last = new Error(`${url} answered ${res.status}`)
    } catch (error) {
      last = new Error(`${url} could not be read (${error.cause?.code ?? error.message}) — is the site reachable from this runner?`)
    }
    if (i + 1 < attempts) await new Promise((r) => setTimeout(r, 1000 * (i + 1)))
  }
  throw last
}

/** The bytes at `url`, or null on 404. Any other failure throws. */
async function readBytes(fetchImpl, url) {
  const res = await get(fetchImpl, url)
  if (res.status === 404) return null
  if (!res.ok) throw new Error(`${url} answered ${res.status}`)
  return Buffer.from(await res.arrayBuffer())
}

async function hashUrl(fetchImpl, url) {
  const res = await get(fetchImpl, url)
  if (!res.ok) return { status: res.status }
  const h = createHash('sha256')
  for await (const chunk of res.body) h.update(chunk)
  return { status: res.status, sha256: h.digest('hex') }
}

/**
 * Fails clearly, before anything else is tried, when the site's host does not resolve from this runner — the site
 * may be reachable only on a private network, and only a runner on it can publish and read back.
 */
export async function assertResolves(baseUrl, resolver = lookup) {
  const host = new URL(siteRoot(baseUrl)).hostname
  try {
    await resolver(host)
  } catch (error) {
    throw new Error(
      `${host} does not resolve from this runner (${error.code ?? error.message}) — the site may be private-network only: run the job on a runner that can reach it (the workflow's runs-on input)`,
    )
  }
}

/**
 * Uploads a staged tree (see `stage`) in the order the header gives. `dest` is the rsync destination (`user@host:`
 * in production — the account's root is the app's folder), `sshArgs` the `-e` command line.
 */
export async function publish({
  outDir,
  app,
  dest,
  sshArgs,
  baseUrl,
  dryRun = false,
  rsync = 'rsync',
  fetchImpl = fetch,
  log = console.log,
}) {
  const root = `${siteRoot(baseUrl)}/${app}`
  const latestBytes = readFileSync(join(outDir, 'latest.json'))
  const { version, files } = JSON.parse(latestBytes.toString('utf8'))
  const localSums = readFileSync(join(outDir, version, 'SHA256SUMS'))
  const rootFiles = readdirSync(outDir).filter((n) => n !== 'latest.json' && n !== version && n !== 'manifest.json')
  rootFiles.sort()
  rootFiles.push('manifest.json') // installers first, manifest after them, latest.json last
  const out = { version, uploaded: [], unchanged: [], skippedLatest: false, dryRun }
  const slash = /[/:]$/.test(dest) ? '' : '/'

  const remoteSums = await readBytes(fetchImpl, `${root}/${version}/SHA256SUMS`)
  if (remoteSums !== null && !remoteSums.equals(localSums)) {
    throw new Error(
      `${version} is already published and its SHA256SUMS differs from this build's — versions are immutable, nothing was changed; publish a new version instead`,
    )
  }
  log(remoteSums === null ? `${version}: not complete on the site yet` : `${version}: complete on the site — verifying only`)

  const remoteLatest = await readBytes(fetchImpl, `${root}/latest.json`)
  let remoteVersion = null
  if (remoteLatest !== null) {
    try {
      const v = JSON.parse(remoteLatest.toString('utf8')).version
      if (typeof v === 'string' && /^\d+\.\d+\.\d+$/.test(v)) remoteVersion = v
    } catch {
      remoteVersion = null
    }
  }
  out.skippedLatest = remoteVersion !== null && compareVersions(remoteVersion, version) > 0

  const common = { rsync, sshArgs, log }
  if (dryRun) {
    log('dry run — nothing is uploaded. Would send, in this order:')
    if (remoteSums === null) {
      for (const n of Object.keys(files)) log(`  ${version}/${n}`)
      log(`  ${version}/SHA256SUMS`)
    }
    for (const n of rootFiles) log(`  ${n} (when the site's copy differs)`)
    log(out.skippedLatest ? `  (latest.json stays: the site names ${remoteVersion})` : '  latest.json (when the site\'s copy differs)')
    return out
  }

  if (remoteSums === null) {
    runRsync({ ...common, args: ['-rt', '--ignore-existing', '--exclude', 'SHA256SUMS', join(outDir, version), dest] })
    // Check every file as a visitor would get it before the folder is declared complete.
    for (const [name, want] of Object.entries(files)) {
      const got = await hashUrl(fetchImpl, `${root}/${version}/${name}`)
      if (got.sha256 !== want.sha256) {
        throw new Error(
          `${version}/${name} on the site is ${got.status === 200 ? 'a different file' : `answering ${got.status}`} than this build's — the folder was left without SHA256SUMS; a person removes that file on the server, then dispatch the same tag again`,
        )
      }
      out.uploaded.push(`${version}/${name}`)
    }
    runRsync({ ...common, args: ['-t', join(outDir, version, 'SHA256SUMS'), `${dest}${slash}${version}/`] })
    out.uploaded.push(`${version}/SHA256SUMS`)
  } else {
    for (const [name, want] of Object.entries(files)) {
      const got = await hashUrl(fetchImpl, `${root}/${version}/${name}`)
      if (got.sha256 !== want.sha256)
        throw new Error(`${version}/${name} on the site does not match its SHA256SUMS (${got.status})`)
    }
  }

  // The app-root files are replaced only when they differ, so a re-run sends nothing.
  const mutable = out.skippedLatest ? rootFiles : [...rootFiles, 'latest.json']
  for (const name of mutable) {
    const local = readFileSync(join(outDir, name))
    const remote = await readBytes(fetchImpl, `${root}/${name}`)
    if (remote !== null && remote.equals(local)) {
      out.unchanged.push(name)
      continue
    }
    runRsync({ ...common, args: ['-t', join(outDir, name), dest] })
    out.uploaded.push(name)
  }
  if (out.skippedLatest) log(`latest.json stays: the site already names ${remoteVersion}, newer than ${version}`)

  // Read back what a visitor reads.
  const sumsBack = await readBytes(fetchImpl, `${root}/${version}/SHA256SUMS`)
  if (sumsBack === null || !sumsBack.equals(localSums))
    throw new Error(`${root}/${version}/SHA256SUMS does not read back as this build's`)
  for (const name of mutable) {
    const back = await readBytes(fetchImpl, `${root}/${name}`)
    if (back === null || !back.equals(readFileSync(join(outDir, name))))
      throw new Error(`${root}/${name} does not read back as the file just sent`)
  }
  return out
}

function sshCommand(env) {
  const need = (k) => {
    if (!env[k]) throw new Error(`${k} is not set — the upload secrets are missing on this repo`)
    return env[k]
  }
  const port = need('DOWNLOADS_SSH_PORT')
  if (!/^\d+$/.test(port)) throw new Error('DOWNLOADS_SSH_PORT is not a port number')
  const key = need('DOWNLOADS_KEY_FILE')
  const known = need('DOWNLOADS_KNOWN_HOSTS_FILE')
  for (const f of [key, known]) if (!existsSync(f)) throw new Error(`${f} does not exist`)
  return {
    dest: `${need('DOWNLOADS_SSH_USER')}@${need('DOWNLOADS_SSH_HOST')}:`,
    sshArgs: `ssh -p ${port} -i ${key} -o UserKnownHostsFile=${known} -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes -o BatchMode=yes`,
  }
}

async function main(argv, env) {
  const rootAt = argv.indexOf('--root')
  const root = resolve(rootAt >= 0 ? argv[rootAt + 1] ?? '' : '.')
  const args = argv.filter((a, i) => !(rootAt >= 0 && (i === rootAt || i === rootAt + 1)))
  const [command, tag, ...rest] = args
  const usage = 'usage: publish-downloads.mjs check | names <tag> | publish <tag> <assetsDir> [--dry-run]  [--root <repo>]'
  if (!['check', 'names', 'publish'].includes(command)) throw new Error(usage)
  const { config, manifest } = loadRepo(root, env)
  if (command === 'check') {
    console.log(`downloads: ${config.app} — ${config.files.length} file(s), ${config.optional.length} optional, ${config.installers.length} installer(s), manifest ${config.manifest} valid`)
    return 0
  }
  if (command === 'names') {
    const v = finalVersion(tag)
    console.log([...namesFor(config.files, v), ...namesFor(config.optional, v)].join('\n'))
    return 0
  }
  const dryRun = rest.includes('--dry-run')
  const assetsDir = resolve(rest.filter((a) => !a.startsWith('--'))[0] ?? '')
  const baseUrl = env.DOWNLOADS_URL
  const outDir = mkdtempSync(join(tmpdir(), 'publish-downloads-'))
  const staged = await stage({ tag, assetsDir, outDir, baseUrl, root, config, manifest })
  console.log(`Staged ${config.app} ${staged.version}: ${Object.keys(staged.files).join(', ')}`)
  await assertResolves(baseUrl)
  const target = dryRun ? { dest: 'DRY-RUN:', sshArgs: undefined } : sshCommand(env)
  const result = await publish({ outDir, app: config.app, ...target, baseUrl, dryRun })
  if (dryRun) console.log(`Dry run of ${result.version} done — nothing was uploaded`)
  else
    console.log(
      `Published ${config.app} ${result.version}: sent ${result.uploaded.length ? result.uploaded.join(', ') : 'nothing (a verified no-op)'}` +
        `${result.unchanged.length ? `; unchanged ${result.unchanged.join(', ')}` : ''}${result.skippedLatest ? ' (latest.json names a newer version)' : ''}`,
    )
  return 0
}

// realpath: a script run through a symlinked path (macOS /var → /private/var) is still this script.
if (process.argv[1] && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href) {
  main(process.argv.slice(2), process.env).then(
    (code) => {
      process.exitCode = code
    },
    (error) => {
      console.error(error instanceof Error ? error.message : String(error))
      process.exitCode = 1
    },
  )
}
