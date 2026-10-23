#!/usr/bin/env node
// colab-handbook: publish-downloads @ v1.17.0-rc.1
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
//     installers:                       # repo paths, uploaded to /<app>/ with @DOWNLOADS_URL@ and
//       - scripts/install.sh            #   @DOWNLOADS_REQUIRES@ stamped (optional key)
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
// THE APPS IT REQUIRES (#653) come from manifest.json's optional `requires`: [{"slug": "<app>", "min": "X.Y.Z"}],
// other apps on the same site that its installers install first. Before anything is sent, every slug must have a final
// on the site at >= min, and following the site's manifests from them must never lead back here (a cycle). The list
// is stamped into each installer as `slug:min slug:min` at @DOWNLOADS_REQUIRES@ — an installer of an app that requires
// anything must carry that placeholder. The installer side (order, once per run, opt-out) is docs/downloads.md.
//
// WHAT A PORTAL SHOWS (#655) is manifest.json's optional description, icon, screenshots, examples, install.prompt,
// install.handover and listed (docs/downloads.md). The icon and screenshot files are repo paths relative to the
// manifest; they are checked (at most 1 MB each, their bytes the type their name says, a square icon) and published
// INSIDE <X.Y.Z>/ next to the binaries — in its SHA256SUMS, so a version's images are as immutable as its files, but
// not in latest.json's `files`, which lists what an installer downloads. The manifest the site gets names them as
// `X.Y.Z/<name>` (relative to the app's folder) and has `<site>` replaced by the site root in every install text.
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
export const REQUIRES_PLACEHOLDER = '@DOWNLOADS_REQUIRES@'
const VERSION = /^\d+\.\d+\.\d+$/
export const DEFAULT_MANIFEST = '.github/downloads/manifest.json'
/** Names the script itself writes at the app's root; an installer may not take one. */
const ROOT_NAMES = new Set(['latest.json', 'manifest.json'])
/** What the manifest's install texts write for the site root; replaced in the copy the site gets. */
export const SITE_TOKEN = '<site>'
const MANIFEST_KEYS = ['schema', 'name', 'summary', 'description', 'icon', 'screenshots', 'examples', 'install', 'requires', 'listed']
export const LIMITS = { images: 6, examples: 6, imageBytes: 1024 * 1024, descriptionChars: 2000, outputLines: 10 }
const IMAGE_TYPES = { png: 'png', jpg: 'jpeg', jpeg: 'jpeg', webp: 'webp', svg: 'svg' }
const SCREENSHOT_EXT = ['png', 'jpg', 'jpeg', 'webp']
const ICON_EXT = ['png', 'svg']

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

/** Problems with a `description` text: the Markdown subset docs/downloads.md gives, nothing heavier. */
function markdownProblems(s, at) {
  const out = []
  if (s.length > LIMITS.descriptionChars) out.push(`${at}: longer than ${LIMITS.descriptionChars} characters`)
  if (/<[A-Za-z/!?]/.test(s)) out.push(`${at}: raw HTML is not shown`)
  if (/^ {0,3}#/m.test(s)) out.push(`${at}: no headings (#) — the portal titles the app itself`)
  if (/!\[/.test(s)) out.push(`${at}: no inline images — use screenshots`)
  if (/^ {0,3}(```|~~~)/m.test(s)) out.push(`${at}: no code blocks — use examples`)
  for (const [, url] of s.matchAll(/\]\(([^)\s]*)/g))
    if (!/^https:\/\/\S+$/.test(url)) out.push(`${at}: the link ${JSON.stringify(url)} is not an https address`)
  return out
}

/** Problems with a manifest image path: relative to the manifest, inside the repo, a plain file name of a known type. */
function imagePathProblems(p, at, exts) {
  if (typeof p !== 'string' || p === '') return [`${at}: a file path, relative to manifest.json`]
  if (p.startsWith('/') || p.includes('\\') || p.split('/').some((seg) => seg === '..' || seg === ''))
    return [`${at}: ${JSON.stringify(p)} is not a path inside the repo, relative to manifest.json`]
  const name = basename(p)
  const ext = name.includes('.') ? name.slice(name.lastIndexOf('.') + 1).toLowerCase() : ''
  if (name.startsWith('.') || name === 'SHA256SUMS' || !exts.includes(ext))
    return [`${at}: ${JSON.stringify(name)} is not a ${exts.map((e) => `.${e}`).join(' / ')} file`]
  return []
}

/** Throws naming every problem with a manifest object; returns it unchanged when valid. Fields: docs/downloads.md. */
export function validateManifest(m) {
  const problems = []
  if (m === null || typeof m !== 'object' || Array.isArray(m)) throw new Error('manifest.json is not a JSON object')
  const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)
  for (const k of Object.keys(m)) if (!MANIFEST_KEYS.includes(k)) problems.push(`unknown field "${k}"`)
  if (m.schema !== undefined && m.schema !== 1) problems.push('schema must be 1')
  if (!isText(m.name, true)) problems.push('name: a one-line text is required')
  if (!isText(m.summary, true)) problems.push('summary: a one-line text is required')
  if (m.description !== undefined) {
    if (!isText(m.description)) problems.push('description: a text')
    else
      for (const [lang, s] of typeof m.description === 'string' ? [[null, m.description]] : Object.entries(m.description))
        problems.push(...markdownProblems(s, lang ? `description.${lang}` : 'description'))
  }
  if (m.icon !== undefined) problems.push(...imagePathProblems(m.icon, 'icon', ICON_EXT))
  if (m.screenshots !== undefined) {
    if (!Array.isArray(m.screenshots) || m.screenshots.length > LIMITS.images)
      problems.push(`screenshots: a list of at most ${LIMITS.images} {"file", "alt", "caption", "theme"}`)
    else
      m.screenshots.forEach((s, i) => {
        const at = `screenshots[${i}]`
        if (!isObject(s)) return problems.push(`${at}: an object {"file", "alt", "caption", "theme"}`)
        for (const k of Object.keys(s)) if (!['file', 'alt', 'caption', 'theme'].includes(k)) problems.push(`${at}.${k}: unknown field`)
        problems.push(...imagePathProblems(s.file, `${at}.file`, SCREENSHOT_EXT))
        if (!isText(s.alt, true)) problems.push(`${at}.alt: a one-line text is required (what the image shows)`)
        if (s.caption !== undefined && !isText(s.caption, true)) problems.push(`${at}.caption: a one-line text`)
        if (s.theme !== undefined && !['light', 'dark'].includes(s.theme)) problems.push(`${at}.theme: "light" or "dark"`)
      })
  }
  if (m.examples !== undefined) {
    if (!Array.isArray(m.examples) || m.examples.length > LIMITS.examples)
      problems.push(`examples: a list of at most ${LIMITS.examples} {"caption", "command", "output"}`)
    else
      m.examples.forEach((x, i) => {
        const at = `examples[${i}]`
        if (!isObject(x)) return problems.push(`${at}: an object {"caption", "command", "output"}`)
        for (const k of Object.keys(x)) if (!['caption', 'command', 'output'].includes(k)) problems.push(`${at}.${k}: unknown field`)
        if (!isText(x.caption, true)) problems.push(`${at}.caption: a one-line text is required`)
        if (typeof x.command !== 'string' || x.command.trim() === '' || /[\r\n]/.test(x.command))
          problems.push(`${at}.command: one line, the command as typed`)
        if (x.output !== undefined) {
          if (typeof x.output !== 'string') problems.push(`${at}.output: a plain text`)
          else if (x.output.replace(/\r?\n$/, '').split(/\r?\n/).length > LIMITS.outputLines)
            problems.push(`${at}.output: more than ${LIMITS.outputLines} lines — trim it to what shows the point`)
        }
      })
  }
  const inst = m.install
  const platforms = isObject(inst) ? Object.keys(inst).filter((k) => !['prompt', 'handover'].includes(k)) : []
  if (!isObject(inst) || platforms.length === 0) {
    problems.push(`install: an object keyed by platform (${PLATFORMS.join(', ')}) is required`)
  } else {
    for (const os of platforms) {
      const how = inst[os]
      if (!PLATFORMS.includes(os)) {
        problems.push(`install.${os}: not a platform (${PLATFORMS.join(', ')}) nor prompt / handover`)
        continue
      }
      if (!isObject(how)) {
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
    if (inst.prompt !== undefined && !isText(inst.prompt)) problems.push('install.prompt: a text (what a person pastes into an agent)')
    if (inst.handover !== undefined && (!Array.isArray(inst.handover) || inst.handover.length === 0 || !inst.handover.every((s) => isText(s))))
      problems.push('install.handover: a non-empty list of texts')
  }
  if (m.requires !== undefined) {
    if (!Array.isArray(m.requires)) problems.push('requires: a list of {"slug", "min"}')
    else {
      const seen = new Set()
      m.requires.forEach((r, i) => {
        const at = `requires[${i}]`
        if (!isObject(r)) return problems.push(`${at}: an object {"slug", "min"}`)
        for (const k of Object.keys(r)) if (!['slug', 'min'].includes(k)) problems.push(`${at}.${k}: unknown field`)
        if (typeof r.slug !== 'string' || !SLUG.test(r.slug)) problems.push(`${at}.slug: an app slug, [a-z0-9][a-z0-9-]*`)
        else if (seen.has(r.slug)) problems.push(`${at}.slug: ${r.slug} is listed twice`)
        else seen.add(r.slug)
        if (typeof r.min !== 'string' || !VERSION.test(r.min)) problems.push(`${at}.min: a version X.Y.Z`)
      })
    }
  }
  if (m.listed !== undefined && typeof m.listed !== 'boolean') problems.push('listed: true or false')
  if (problems.length) throw new Error(`manifest.json: ${problems.join('; ')}`)
  return m
}

/** The manifest's `requires`, as given (an empty list when it declares none). */
export const requiresOf = (manifest) => (Array.isArray(manifest?.requires) ? manifest.requires : [])

/** What an installer's @DOWNLOADS_REQUIRES@ becomes: `slug:min slug:min`, in the manifest's order. */
export const requiresStamp = (manifest) => requiresOf(manifest).map((r) => `${r.slug}:${r.min}`).join(' ')

/** The manifest's image paths, in order: the icon first, then each screenshot. */
export const imagesOf = (manifest) => [
  ...(typeof manifest?.icon === 'string' ? [{ field: 'icon', path: manifest.icon }] : []),
  ...(Array.isArray(manifest?.screenshots) ? manifest.screenshots : []).map((s, i) => ({ field: `screenshots[${i}].file`, path: s.file })),
]

/** The type a file's first bytes say it is (png, jpeg, webp, svg), or null. */
export function sniffImage(bytes) {
  const head = bytes.subarray(0, 12)
  if (head.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]))) return 'png'
  if (head[0] === 0xff && head[1] === 0xd8 && head[2] === 0xff) return 'jpeg'
  if (head.subarray(0, 4).toString('latin1') === 'RIFF' && head.subarray(8, 12).toString('latin1') === 'WEBP') return 'webp'
  if (/<svg[\s>]/.test(bytes.subarray(0, 4096).toString('utf8'))) return 'svg'
  return null
}

/** An image's width and height when the file says so cheaply (a PNG's header, an SVG's viewBox), else null. */
function imageSize(bytes, type) {
  if (type === 'png' && bytes.length >= 24) return { w: bytes.readUInt32BE(16), h: bytes.readUInt32BE(20) }
  if (type === 'svg') {
    const vb = /<svg[^>]*\sviewBox\s*=\s*["']\s*[-\d.]+[\s,]+[-\d.]+[\s,]+([\d.]+)[\s,]+([\d.]+)\s*["']/.exec(bytes.toString('utf8'))
    if (vb) return { w: Number(vb[1]), h: Number(vb[2]) }
  }
  return null
}

/**
 * Checks the files behind the manifest's icon and screenshots — present, at most LIMITS.imageBytes, their bytes the
 * type their name says, the icon square, no two published under one name — and returns them as `{ name, source }`.
 * Paths are relative to the manifest file (`manifestPath`, itself relative to `root`).
 */
export function imageFiles(root, manifestPath, manifest) {
  const dir = join(root, manifestPath, '..')
  const out = []
  const problems = []
  for (const { field, path: p } of imagesOf(manifest)) {
    const source = join(dir, p)
    const name = basename(p)
    const at = `${field} ${p}`
    if (!existsSync(source) || !statSync(source).isFile()) {
      problems.push(`${at}: no such file next to ${manifestPath}`)
      continue
    }
    const size = statSync(source).size
    if (size > LIMITS.imageBytes) problems.push(`${at}: ${size} bytes, over the ${LIMITS.imageBytes}-byte limit`)
    const bytes = readFileSync(source)
    const type = sniffImage(bytes)
    const want = IMAGE_TYPES[name.slice(name.lastIndexOf('.') + 1).toLowerCase()]
    if (type !== want) problems.push(`${at}: its bytes are ${type ? `a ${type}` : 'not an image'}, not the ${want} its name says`)
    else if (field === 'icon') {
      const dim = imageSize(bytes, type)
      if (!dim) problems.push(`${at}: an SVG icon needs a viewBox, to be checked square`)
      else if (dim.w !== dim.h) problems.push(`${at}: ${dim.w}×${dim.h}, an icon is square`)
    }
    const twin = out.find((o) => o.name === name)
    if (twin && twin.source !== source) problems.push(`${at}: another image is also named ${name}, and both would be ${name} on the site`)
    if (!twin) out.push({ name, source })
  }
  if (problems.length) throw new Error(`${manifestPath}: ${problems.join('; ')}`)
  return out
}

function stampSite(v, base) {
  if (typeof v === 'string') return v.split(SITE_TOKEN).join(base)
  if (Array.isArray(v)) return v.map((x) => stampSite(x, base))
  if (v !== null && typeof v === 'object') return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, stampSite(x, base)]))
  return v
}

/**
 * The manifest as the site gets it: the icon and screenshots named `X.Y.Z/<name>` (relative to the app's folder,
 * where this version published them) and `<site>` replaced by the site root in every install text.
 */
export function publishedManifest(manifest, { version, base }) {
  const m = { ...manifest }
  if (typeof m.icon === 'string') m.icon = `${version}/${basename(m.icon)}`
  if (Array.isArray(m.screenshots)) m.screenshots = m.screenshots.map((s) => ({ ...s, file: `${version}/${basename(s.file)}` }))
  if (m.install !== undefined) m.install = stampSite(m.install, base)
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
  if (requiresOf(manifest).some((r) => r.slug === config.app))
    throw new Error(`${config.manifest}: requires names ${config.app} itself`)
  imageFiles(root, config.manifest, manifest)
  for (const p of config.installers) {
    const f = join(root, p)
    if (!existsSync(f)) throw new Error(`downloads.installers: ${p} does not exist`)
    const text = readFileSync(f, 'utf8')
    if (!text.includes(PLACEHOLDER)) throw new Error(`${p} has no ${PLACEHOLDER} to stamp the downloads address into`)
    if (requiresOf(manifest).length > 0 && !text.includes(REQUIRES_PLACEHOLDER))
      throw new Error(
        `${p} has no ${REQUIRES_PLACEHOLDER}, so it would install ${config.app} without the apps its manifest requires — add the dependency step (docs/downloads.md)`,
      )
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
  const images = imageFiles(root, config.manifest, manifest)
  const clash = images.find((i) => names.includes(i.name))
  if (clash) throw new Error(`${config.manifest}: the image ${clash.name} has the name of a file of this version; rename the image`)
  const folder = join(outDir, version)
  mkdirSync(folder, { recursive: true })
  const files = {}
  const inFolder = {}
  for (const name of names) {
    copyFileSync(join(assetsDir, name), join(folder, name))
    files[name] = { sha256: await sha256File(join(folder, name)), size: statSync(join(folder, name)).size }
    inFolder[name] = files[name]
  }
  // The images join the version folder and its SHA256SUMS, not latest.json's `files` (what an installer downloads).
  for (const { name, source } of images) {
    copyFileSync(source, join(folder, name))
    inFolder[name] = { sha256: await sha256File(join(folder, name)), size: statSync(join(folder, name)).size }
  }
  const sums = Object.keys(inFolder)
    .sort()
    .map((n) => `${inFolder[n].sha256}  ${n}\n`)
    .join('')
  writeFileSync(join(folder, 'SHA256SUMS'), sums)
  const rootFiles = []
  for (const p of config.installers) {
    const source = readFileSync(join(root, p), 'utf8')
    const stamped = source.split(PLACEHOLDER).join(base).split(REQUIRES_PLACEHOLDER).join(requiresStamp(manifest))
    writeFileSync(join(outDir, basename(p)), stamped)
    rootFiles.push(basename(p))
  }
  writeFileSync(join(outDir, 'manifest.json'), `${JSON.stringify(publishedManifest(manifest, { version, base }), null, 2)}\n`)
  rootFiles.push('manifest.json')
  writeFileSync(join(outDir, 'latest.json'), `${JSON.stringify({ version, files }, null, 2)}\n`)
  return { version, files, images: images.map((i) => i.name), sums, rootFiles }
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
 * Checks, reading the site only, that every app `manifest` requires can be installed from it: each slug has a final
 * (`latest.json`) at >= its min and a valid manifest, and following those manifests' own `requires` never leads back
 * to `app` or round any other loop. Throws naming the slug and the reason; returns the slugs visited, in order.
 */
export async function checkRequires({ app, manifest, baseUrl, fetchImpl = fetch }) {
  const base = siteRoot(baseUrl)
  const visited = []
  const done = new Set()
  async function walk(requires, chain) {
    for (const { slug, min } of requires) {
      const via = chain.join(' → ')
      if (chain.includes(slug)) throw new Error(`requires: a cycle — ${via} → ${slug}; nothing was published`)
      const latest = await readBytes(fetchImpl, `${base}/${slug}/latest.json`)
      if (latest === null) throw new Error(`requires: ${slug} (required by ${via}) has no final on the site — publish it first; nothing was published`)
      let version = null
      try {
        version = JSON.parse(latest.toString('utf8')).version
      } catch {
        version = null
      }
      if (typeof version !== 'string' || !VERSION.test(version))
        throw new Error(`requires: ${base}/${slug}/latest.json names no version; nothing was published`)
      if (compareVersions(version, min) < 0)
        throw new Error(`requires: ${via} needs ${slug} >= ${min}, and the site's newest final of it is ${version}; nothing was published`)
      if (done.has(slug)) continue
      done.add(slug)
      visited.push(slug)
      const raw = await readBytes(fetchImpl, `${base}/${slug}/manifest.json`)
      if (raw === null) throw new Error(`requires: ${slug} has no manifest.json on the site; nothing was published`)
      let theirs
      try {
        theirs = validateManifest(JSON.parse(raw.toString('utf8')))
      } catch (error) {
        throw new Error(`requires: ${slug}'s manifest.json on the site is not valid (${error.message}); nothing was published`)
      }
      await walk(requiresOf(theirs), [...chain, slug])
    }
  }
  await walk(requiresOf(manifest), [app])
  return visited
}

/** A SHA256SUMS file's lines as `{ <name>: { sha256 } }`. */
export function parseSums(bytes) {
  const out = {}
  for (const line of bytes.toString('utf8').split('\n')) if (line.length > 66) out[line.slice(66)] = { sha256: line.slice(0, 64) }
  return out
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
  const { version } = JSON.parse(latestBytes.toString('utf8'))
  const localSums = readFileSync(join(outDir, version, 'SHA256SUMS'))
  // Everything in the version folder — the files and the manifest's images — is what SHA256SUMS lists.
  const files = parseSums(localSums)
  const rootFiles = readdirSync(outDir).filter((n) => n !== 'latest.json' && n !== version && n !== 'manifest.json')
  rootFiles.sort()
  rootFiles.push('manifest.json') // installers first, manifest after them, latest.json last
  const out = { version, uploaded: [], unchanged: [], skippedLatest: false, dryRun, requires: [] }
  const slash = /[/:]$/.test(dest) ? '' : '/'

  // Every app this one requires must be installable from the site before anything of this one is on it.
  const manifest = JSON.parse(readFileSync(join(outDir, 'manifest.json'), 'utf8'))
  out.requires = await checkRequires({ app, manifest, baseUrl, fetchImpl })
  if (out.requires.length) log(`requires: ${out.requires.join(', ')} — each on the site at the version needed`)

  const remoteSums = await readBytes(fetchImpl, `${root}/${version}/SHA256SUMS`)
  if (remoteSums !== null && !remoteSums.equals(localSums)) {
    const theirs = parseSums(remoteSums)
    const differ = [...new Set([...Object.keys(files), ...Object.keys(theirs)])]
      .filter((n) => files[n]?.sha256 !== theirs[n]?.sha256)
      .sort()
    const images = new Set(imagesOf(manifest).map((i) => basename(i.path)))
    const hint =
      differ.length > 0 && differ.every((n) => images.has(n))
        ? ' — only images differ: publish from the tag\'s own commit (run the workflow from the tag), whose images this version carries'
        : '; publish a new version instead'
    throw new Error(
      `${version} is already published and its SHA256SUMS differs from this build's (${differ.join(', ') || 'its bytes'}) — versions are immutable, nothing was changed${hint}`,
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
    const req = requiresOf(manifest).length ? `, requires ${requiresStamp(manifest)}` : ''
    const img = imagesOf(manifest).length ? `, ${imagesOf(manifest).length} image(s)` : ''
    const unlisted = manifest.listed === false ? ', unlisted' : ''
    console.log(`downloads: ${config.app} — ${config.files.length} file(s), ${config.optional.length} optional, ${config.installers.length} installer(s), manifest ${config.manifest} valid${img}${req}${unlisted}`)
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
  console.log(`Staged ${config.app} ${staged.version}: ${[...Object.keys(staged.files), ...staged.images].join(', ')}`)
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
