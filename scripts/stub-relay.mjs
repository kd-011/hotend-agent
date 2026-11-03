#!/usr/bin/env node
// stub-relay.mjs: a stand-in for the Hotend relay and for Moonraker, so you can
// watch what the agent sends and does without an account and without a printer.
//
//   node scripts/stub-relay.mjs ./hotend-agent
//
// It starts a fake relay and a fake Moonraker on 127.0.0.1, runs the agent you
// name against them with a throwaway state file, prints every request the agent
// makes, asks it for two Moonraker paths, then stops the agent and exits.
// Nothing leaves your machine. No dependencies beyond Node 18.
import { createServer } from 'node:http'
import { createHash } from 'node:crypto'
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const agentBin = process.argv[2]
if (!agentBin) {
  console.error('usage: node scripts/stub-relay.mjs <path-to-hotend-agent-binary>')
  process.exit(2)
}

const t0 = Date.now()
const log = (who, text) => console.log(`${String((Date.now() - t0) / 1000).padStart(5)}s  ${who.padEnd(9)} ${text}`)
const shorten = (s) => (s.length > 100 ? `${s.slice(0, 97)}...` : s)

// Fake Moonraker: answers a few paths and records what the agent asked for.
const moonraker = createServer((req, res) => {
  log('moonraker', `${req.method} ${req.url}`)
  res.setHeader('content-type', 'application/json')
  if (req.url.startsWith('/server/config')) return res.end('{"result":{"config":{}}}')
  if (req.url.startsWith('/printer/info')) return res.end('{"result":{"state":"ready","hostname":"stub"}}')
  res.end('{"result":"ok"}')
})

// Fake relay: completes the WebSocket handshake and reads the agent's frames.
const relay = createServer((req, res) => {
  const u = new URL(req.url, 'http://x')
  log('relay', `HTTP ${req.method} ${u.pathname}${u.search ? ` ${u.search}` : ''}`)
  res.end('') // an empty version means "nothing newer"
})

let sockRef = null
relay.on('upgrade', (req, socket) => {
  const u = new URL(req.url, 'http://x')
  log('relay', `WebSocket ${u.pathname}${u.search.replace(/claim=[^&]*/, 'claim=<redacted>')}`)
  for (const k of ['x-hotend-agent-version', 'authorization', 'user-agent']) {
    if (req.headers[k]) log('relay', `  header ${k}: ${k === 'authorization' ? '<redacted>' : req.headers[k]}`)
  }
  const accept = createHash('sha1').update(`${req.headers['sec-websocket-key']}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest('base64')
  socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`)
  sockRef = socket
  let buf = Buffer.alloc(0)
  socket.on('data', (d) => {
    buf = Buffer.concat([buf, d])
    for (;;) {
      if (buf.length < 2) return
      let len = buf[1] & 0x7f
      let off = 2
      if (len === 126) { if (buf.length < 4) return; len = buf.readUInt16BE(2); off = 4 }
      else if (len === 127) { if (buf.length < 10) return; len = Number(buf.readBigUInt64BE(2)); off = 10 }
      const masked = (buf[1] & 0x80) !== 0
      if (buf.length < off + (masked ? 4 : 0) + len) return
      const mask = masked ? buf.subarray(off, off + 4) : null
      const start = off + (masked ? 4 : 0)
      const payload = Buffer.from(buf.subarray(start, start + len))
      if (mask) for (let i = 0; i < payload.length; i++) payload[i] ^= mask[i % 4]
      const op = buf[0] & 0x0f
      buf = buf.subarray(start + len)
      if (op === 1) onFrame(payload.toString('utf8'))
    }
  })
  // Ask the agent for two Moonraker paths, the way the app would. The agent has
  // no list of allowed paths: it forwards whatever the relay names.
  setTimeout(() => ask('1', 'GET', '/printer/info'), 1500)
  setTimeout(() => ask('2', 'POST', '/printer/gcode/script?script=M115'), 2000)
})

function sendFrame(obj) {
  const p = Buffer.from(JSON.stringify(obj))
  const head = p.length < 126 ? Buffer.from([0x81, p.length]) : Buffer.from([0x81, 126, p.length >> 8, p.length & 255])
  sockRef.write(Buffer.concat([head, p]))
}
function ask(id, method, path) {
  log('relay', `asks the agent: ${method} ${path}`)
  sendFrame({ t: 'req', id, method, path, headers: {} })
  sendFrame({ t: 'end', id })
}
function onFrame(text) {
  let m
  try { m = JSON.parse(text) } catch { return log('agent', `sent ${shorten(text)}`) }
  if (m.t === 'res') return log('agent', `sent reply to request ${m.id}: status ${m.status}`)
  if (m.t === 'body') return log('agent', `sent body for request ${m.id}: ${shorten(Buffer.from(m.b64 || '', 'base64').toString())}`)
  if (m.t === 'end') return log('agent', `sent end of request ${m.id}`)
  log('agent', `sent ${shorten(text)}`)
}

await new Promise((r) => moonraker.listen(0, '127.0.0.1', r))
await new Promise((r) => relay.listen(0, '127.0.0.1', r))
const mPort = moonraker.address().port
const rPort = relay.address().port
const state = mkdtempSync(join(tmpdir(), 'hotend-stub-'))
log('stub', `relay on 127.0.0.1:${rPort}, moonraker on 127.0.0.1:${mPort}, state in ${state}`)

const agent = spawn(agentBin, [], {
  env: {
    PATH: process.env.PATH,
    HOME: state,
    HOTEND_RELAY_URL: `ws://127.0.0.1:${rPort}/`,
    HOTEND_LOCAL_URL: `http://127.0.0.1:${mPort}`,
    HOTEND_AGENT_STATE: join(state, 'agent.json'),
    HOTEND_SPOOLMAN_PORT: String(rPort + 1),
  },
  stdio: ['ignore', 'pipe', 'pipe'],
})
for (const s of [agent.stdout, agent.stderr]) {
  s.on('data', (d) => String(d).split('\n').filter((l) => l.trim()).forEach((l) => log('agent', `says ${shorten(l.trim())}`)))
}
agent.on('error', (e) => { console.error(`could not start ${agentBin}: ${e.message}`); process.exit(1) })

setTimeout(() => {
  agent.kill('SIGTERM')
  relay.close(); moonraker.close()
  rmSync(state, { recursive: true, force: true })
  log('stub', 'done, agent stopped')
  process.exit(0)
}, 4000)
