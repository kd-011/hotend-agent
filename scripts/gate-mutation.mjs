#!/usr/bin/env node
// gate-mutation.mjs: mutation testing for this Go module, in the report shape
// the merge gate reads.
//
//   node scripts/gate-mutation.mjs <file.go> [path:start-end,...]
//
// Runs gremlins over the module (it plants small bugs, runs the tests against
// each one and records which survived), then writes the result for the named
// file as a mutation-testing-elements JSON report to $CORTEX_MUTATION_REPORT.
// The ranges are only used to say how many mutants fall inside them; the gate
// does its own line filtering. Without the variable the report goes to stdout.
//
// gremlins is pinned and installed once into ~/.cache/hotend-agent-gate/bin if
// it is not already on PATH. Exits non-zero, with no report, when it cannot run.
import { spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { homedir, tmpdir } from 'node:os'
import { join } from 'node:path'

const GREMLINS = 'github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0'

// gremlins' status names, mapped onto the ones mutation-testing-elements uses.
const STATUS = {
  KILLED: 'Killed',
  LIVED: 'Survived',
  'NOT COVERED': 'NoCoverage',
  'TIMED OUT': 'Timeout',
  'NOT VIABLE': 'CompileError',
  SKIPPED: 'Ignored',
  RUNNABLE: 'Pending',
}

// Turns gremlins' own JSON into a report holding only `file`'s mutants.
export function toReport(gremlins, file) {
  const mutants = []
  for (const f of gremlins.files ?? []) {
    if (f.file_name !== file && !file.endsWith(`/${f.file_name}`)) continue
    for (const m of f.mutations ?? []) {
      const at = { line: m.line, column: m.column }
      mutants.push({
        id: String(mutants.length),
        mutatorName: m.type,
        status: STATUS[m.status] ?? 'Ignored',
        location: { start: at, end: at },
      })
    }
  }
  return { schemaVersion: '1', thresholds: { high: 80, low: 60 }, files: { [file]: { language: 'go', source: '', mutants } } }
}

function gremlinsBinary() {
  if (spawnSync('gremlins', ['--help'], { stdio: 'ignore' }).status === 0) return 'gremlins'
  const bin = join(homedir(), '.cache', 'hotend-agent-gate', 'bin')
  const path = join(bin, 'gremlins')
  if (!existsSync(path)) {
    const r = spawnSync('go', ['install', GREMLINS], { stdio: 'inherit', env: { ...process.env, GOBIN: bin, CGO_ENABLED: '0' } })
    if (r.status !== 0) throw new Error('could not install gremlins')
  }
  return path
}

function main() {
  const file = process.argv[2]
  if (!file?.endsWith('.go')) {
    console.error('usage: node scripts/gate-mutation.mjs <file.go> [path:start-end,...]')
    process.exit(2)
  }
  const dir = mkdtempSync(join(tmpdir(), 'gate-mutation-'))
  try {
    const out = join(dir, 'gremlins.json')
    const r = spawnSync(gremlinsBinary(), ['unleash', '.', '--workers', '2', '-o', out], { stdio: 'inherit' })
    if (!existsSync(out)) throw new Error(`gremlins wrote no report (exit ${r.status})`)
    const report = JSON.stringify(toReport(JSON.parse(readFileSync(out, 'utf8')), file))
    if (process.env.CORTEX_MUTATION_REPORT) writeFileSync(process.env.CORTEX_MUTATION_REPORT, report)
    else console.log(report)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

if (import.meta.url === `file://${process.argv[1]}`) {
  try {
    main()
  } catch (err) {
    console.error(err instanceof Error ? err.message : err)
    process.exit(1)
  }
}
