#!/usr/bin/env node
// verify-agent-release.mjs: check that the SERVED agent release is installable.
//
// The publish smoke test used to prove only that a version string and a non-empty
// .sig came back. That cannot catch the failure that matters: a binary whose bytes
// don't match the signature beside it (a truncated upload, a half-finished
// publish, a stale CDN copy). Every agent would reject that binary in silence, on
// its own printer's journal, and the fleet would simply stop updating.
//
// So: download what the world sees and verify it the way an agent does: against
// the public keys baked into main.go.
//
//   node scripts/verify-agent-release.mjs                      # https://app.hotend.app
//   node scripts/verify-agent-release.mjs http://127.0.0.1:8080
//   node scripts/verify-agent-release.mjs --local              # verify the local build dir too
//
// Exit 0 = every arch verifies and self-reports the advertised version.
import { verify, createPublicKey, createHash } from 'node:crypto'
import { readFileSync, existsSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
// The private monorepo keeps the agent in agent-go/; the public hotend-agent repo keeps
// main.go at its root. Find whichever layout we're sitting in, so the SAME script
// can ship in both places and anyone can verify a release against the source.
const AGENT_DIR = [join(HERE, '..', 'agent-go'), join(HERE, '..')].find((d) => existsSync(join(d, 'main.go'))) || join(HERE, '..', 'agent-go')
const ARCHES = ['amd64', 'arm64', 'arm']
const args = process.argv.slice(2)
const alsoLocal = args.includes('--local')
const BASE = (args.find((a) => !a.startsWith('--')) || 'https://app.hotend.app').replace(/\/$/, '')

/** The keys the fleet trusts, read from the agent source so this can't drift. */
export function trustedKeys() {
  const src = readFileSync(join(AGENT_DIR, 'main.go'), 'utf8')
  const block = src.match(/updatePubKeysHex\s*=\s*\[\]string\{([\s\S]*?)\}/)?.[1] || ''
  return [...block.matchAll(/"([0-9a-f]{64})"/g)].map((m) => m[1])
}

// Raw 32-byte ed25519 key → a KeyObject, via the fixed SPKI prefix.
const keyOf = (hex) =>
  createPublicKey({ key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(hex, 'hex')]), format: 'der', type: 'spki' })

const KEYS = trustedKeys().map(keyOf)

// Multi-MB downloads through Cloudflare occasionally hiccup (525s, resets). A
// transient blip must not read as "the release is broken", so each fetch gets a
// couple of retries; a real problem fails all of them.
async function get(path, { text = false, tries = 3 } = {}) {
  let last
  for (let i = 1; i <= tries; i++) {
    try {
      const r = await fetch(`${BASE}${path}`, { signal: AbortSignal.timeout(120_000), headers: { connection: 'close' } })
      // The relay caps binary downloads per IP. Retrying won't help (the window is
      // an hour) and this says NOTHING about the release, so say so plainly rather
      // than letting a publish read it as a corrupt artifact.
      if (r.status === 429) throw new Error(`rate limited by the relay (retry after ${r.headers.get('retry-after') || '?'}s) — NOT a release problem`)
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      return text ? (await r.text()).trim() : Buffer.from(await r.arrayBuffer())
    } catch (e) {
      if (String(e.message).includes('rate limited')) throw e
      last = e
      if (i < tries) await new Promise((s) => setTimeout(s, 2000 * i))
    }
  }
  throw new Error(`${path} → ${last.message} (${tries} attempts)`)
}

const fail = []
const note = (ok, msg) => { console.log(`${ok ? '  ok  ' : ' FAIL '} ${msg}`); if (!ok) fail.push(msg) }

if (KEYS.length === 0) {
  console.error('ERROR: no trusted keys found in agent-go/main.go (updatePubKeysHex)')
  process.exit(1)
}
console.log(`verifying ${BASE}/agent-bin (${KEYS.length} trusted key${KEYS.length === 1 ? '' : 's'})`)

const served = await get('/agent-bin/version', { text: true }).catch((e) => { note(false, `version: ${e.message}`); return null })
if (!served) process.exit(1)
note(/^[0-9]{4}(\.[0-9]{1,4}){1,3}$/.test(served), `advertised version: ${served}`)

// The source constant must match what's advertised, otherwise the run-and-report
// guard in the agent rejects the update on every printer.
const inSource = readFileSync(join(AGENT_DIR, 'main.go'), 'utf8').match(/agentVersion\s*=\s*"([0-9.]+)"/)?.[1]
note(inSource === served, `agent-go/main.go says ${inSource}, relay advertises ${served}`)

// Signed manifest (when published) must verify and match the advertised version
// and each binary's sha256. Older releases may 404, which is a soft skip.
let manifest = null
{
  let manRaw, manSig
  try {
    manRaw = await get('/agent-bin/manifest')
    manSig = Buffer.from((await get('/agent-bin/manifest.sig', { text: true })), 'hex')
  } catch (e) {
    if (String(e.message).includes('HTTP 404')) {
      console.log('  skip  signed manifest not published yet')
    } else {
      note(false, `manifest: ${e.message}`)
    }
    manRaw = null
  }
  if (manRaw) {
    const good = manSig.length === 64 && KEYS.some((k) => verify(null, manRaw, k, manSig))
    note(good, `manifest signature ${good ? 'verifies' : 'DOES NOT VERIFY'}`)
    let man
    try { man = JSON.parse(manRaw.toString('utf8')) } catch { man = null }
    note(!!man?.version, `manifest parses (version ${man?.version || '?'})`)
    if (man) {
      note(man.version === served, `manifest version ${man.version} matches advertised ${served}`)
      for (const arch of ARCHES) {
        const entry = man.files?.[arch]
        note(!!entry?.sha256, `manifest lists ${arch}`)
      }
      manifest = man
    }
  }
}

for (const arch of ARCHES) {
  const name = `hotend-agent-linux-${arch}`
  let bin, sig
  try {
    bin = await get(`/agent-bin/${name}`)
    sig = Buffer.from((await get(`/agent-bin/${name}.sig`, { text: true })), 'hex')
  } catch (e) { note(false, `${arch}: ${e.message}`); continue }
  const good = sig.length === 64 && KEYS.some((k) => verify(null, bin, k, sig))
  note(good, `${arch}: ${bin.length} bytes, signature ${good ? 'verifies' : 'DOES NOT VERIFY'}`)
  if (manifest?.files?.[arch]?.sha256) {
    const got = createHash('sha256').update(bin).digest('hex')
    note(got === manifest.files[arch].sha256, `${arch}: bytes match signed manifest hash`)
    if (manifest.files[arch].size) {
      note(bin.length === manifest.files[arch].size, `${arch}: size matches manifest`)
    }
  }
  if (alsoLocal) {
    const local = join(AGENT_DIR, name)
    note(existsSync(local) && readFileSync(local).equals(bin), `${arch}: served bytes match the local build`)
  }
}

// Exit 2 = "could not check" (the relay rate-limited us), which is NOT the same as
// "the release is bad". The publish script distinguishes them so a throttled verifier
// never gets reported as a corrupt release.
const throttled = fail.length > 0 && fail.every((m) => m.includes('rate limited'))
if (throttled) {
  console.log(`\nVERIFICATION COULD NOT RUN — rate limited by the relay, not a release problem.`)
  console.log('Re-run this in an hour (or from another network) to confirm the release.')
  process.exit(2)
}
console.log(fail.length ? `\n${fail.length} problem(s) — the fleet cannot install this release.` : '\nrelease is servable and installable.')
process.exit(fail.length ? 1 : 0)
