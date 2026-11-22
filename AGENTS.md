---
slug: hotend-agent-agents
name: hotend-agent agent guide
repo: .
status: active
last_verified: 2026-11-22
---

# hotend-agent: guide for coding agents

The doc index is docs/README.md. Read docs/ARCHITECTURE.md before changing structure.

## Commands

- Needs Go 1.22 or newer; the scripts need Node 18. <!-- src: go.mod, CONTRIBUTING.md:25, README.md:11 -->
- `make` builds the host binary `./hotend-agent`. `make dist` cross-compiles linux amd64, arm (GOARM=7) and arm64, plus `verify-sig`, into `dist/`. <!-- src: Makefile:18-30 -->
- `make test` runs `go vet ./...` then `go test ./...`. CI runs `CGO_ENABLED=0 go vet ./...` and `CGO_ENABLED=0 go test ./...`. <!-- src: Makefile:32-34, .github/workflows/ci.yml:29-47 -->
- Format with `gofmt -w .`; CI fails when `gofmt -l .` prints anything. There is no other linter. <!-- src: .github/workflows/ci.yml:26-27 -->
- Run the agent by hand with `HOTEND_RELAY_URL=wss://... ./hotend-agent`. `./hotend-agent -version` prints the version and exits. <!-- src: main.go:269-273, main.go:281-284 -->
- `make && node scripts/stub-relay.mjs ./hotend-agent` runs the agent against a stand-in relay and Moonraker on loopback, with a throwaway state file, and prints every request. CI runs it against the amd64 build. <!-- src: scripts/stub-relay.mjs:1-10, .github/workflows/ci.yml:49-56 -->
- `node scripts/verify-agent-release.mjs [base-url] [--local]` downloads what the relay serves and checks signatures, the signed manifest and its hashes against the keys in `main.go`. <!-- src: scripts/verify-agent-release.mjs:1-20 -->
- The merge gate runs `go test` per changed package and gremlins mutation testing through `scripts/gate-mutation.mjs`. <!-- src: .cortex-gate.json, scripts/gate-mutation.mjs:1-16 -->

## Conventions

- One `package main` in `main.go` holds the whole agent, with tests in `main_test.go`. `verify-sig/` is a second `package main` in the same module. <!-- src: main.go:4, verify-sig/main.go:16, go.mod -->
- Dependencies stay at the standard library plus `gorilla/websocket`. <!-- src: CONTRIBUTING.md:37, go.mod -->
- Configuration is environment variables only, read once at package init. <!-- src: main.go:59-80 -->
- A change to a guard (signature check, version check, rollback, local-only request targets) needs a test that fails when the guard is weakened. <!-- src: CONTRIBUTING.md:38, .github/PULL_REQUEST_TEMPLATE.md:7-8 -->
- Builds are `CGO_ENABLED=0 -trimpath -buildvcs=false -ldflags "-s -w"`. Keep them: they make the binary static and reproducible. <!-- src: Makefile:4-14 -->
- Write state through `mutateState`, not by building an `agentState` and calling `saveState`, which drops fields the caller forgot. <!-- src: main.go:244-259 -->
- The version is date-based (`YYYY.MM.DD.N`) in `agentVersion`. Only the maintainer cuts releases, so do not bump it. <!-- src: main.go:35-37, CONTRIBUTING.md:21 -->
- Commit messages and prose are plain, with no em dashes. <!-- src: CONTRIBUTING.md:39 -->
- Docs, `scripts/stub-relay.mjs`, templates and `.github/` are edited here. The agent source is a published copy of a private monorepo: `main.go`, `main_test.go`, `go.mod`, `go.sum`, `restart_*.go`, `verify-sig/` and `scripts/verify-agent-release.mjs` are overwritten on the next release, so a change to them here is a proposal the maintainer ports upstream. <!-- src: CONTRIBUTING.md:5-17 -->
- `AGENTS.md` and `docs/ARCHITECTURE.md` are pinned with dossier-scribe through symlinks in `dossiers/`. After changing a cited file run `dossier-scribe check`, fix the claims that are now wrong, run `dossier-scribe pin --all` and commit `dossiers/.dossier-lock.json`. The weekly upkeep job opens an issue when a claim drifts. <!-- src: dossier-scribe.json, .github/workflows/upkeep.yml:50-80 -->

## Traps

- The release guard tests skip when there is nothing to check. `TestReleaseSignaturesVerify` skips without a signed `hotend-agent-linux-<arch>` and `.sig` in the repo root, and `TestVersionFlagMatchesConstant` skips without a built amd64 binary there. A green `go test` on a fresh clone proves neither. <!-- src: main_test.go:103-155, .github/workflows/ci.yml:32-34 -->
- `-version` must print exactly `agentVersion`. The updater test-runs the downloaded binary with `-version` and discards it on a mismatch, so a binary built before a version bump never installs. <!-- src: main.go:696-701, main_test.go:141-155 -->
- Rollback depends on `<binary>.prev` and the probation fields in the state file. A new binary has 15 minutes or 3 boots to reach the relay, otherwise `revertTo` puts the old one back and records the version in `badVersions`, which keeps the last 10. <!-- src: main.go:325-331, main.go:464-492, main.go:702-714 -->
- The first thing `main` does is run the probation check and start the update loop, before any check that can exit. Keep that order, because it lets a bad release be fixed remotely. <!-- src: main.go:274-280 -->
- The relay URL must be `wss://`, or start with `ws://127.0.0.1` or `ws://localhost`. Anything else makes the agent exit. <!-- src: main.go:285-289 -->
- The tunnel check must dial the IP it validated, not the host name again, or DNS rebinding reopens the hole. <!-- src: main.go:950-970, main.go:977-982, main_test.go:330-356 -->
- Relay paths arrive already percent-encoded. `buildLocalURL` sets `Path` (decoded) and `RawPath` (as received) together; setting only `Path` turns `%20` into `%2520` and Moonraker 404s files with spaces or `+`. <!-- src: main.go:1395-1407, main_test.go:271-308 -->
- Exactly one component counts filament. With Moonraker `[spoolman]` configured, Moonraker counts and the agent only mirrors the active spool every 60 s; without it, the agent counts from `print_stats`. Both at once double-counts. <!-- src: main.go:1506-1544, main.go:1618-1620 -->
- The webcam URL the relay sets is not checked against private ranges, and the response goes back to the relay. This is documented as a known trust limit, so changing it means updating `SECURITY.md` and `docs/trust.md` too. <!-- src: main.go:161-177, main.go:1176-1197, SECURITY.md:17 -->
- `scripts/verify-agent-release.mjs` reads the trusted keys from the `updatePubKeysHex` block, and the reproducible workflow takes every 64-character hex string in quotes from `main.go` as a key. Do not add other such literals to `main.go`. <!-- src: scripts/verify-agent-release.mjs:38-42, .github/workflows/reproducible.yml:83-91 -->
- The reproducible workflow pins Go 1.22.2 and sets `GOARM=7`, and a Go bump needs the pin changed there. The Makefile `arm` target also sets `GOARM=7`, but the CI build step does not. <!-- src: .github/workflows/reproducible.yml:50-62, Makefile:26, .github/workflows/ci.yml:37-38 -->
- `restartSelf` uses `syscall.Exec` in `main.go`, so this tree builds for unix targets only. <!-- src: main.go:733-739 -->
- The README transcript and `docs/trust.md` describe what the agent sends and reaches, read from `main.go`. A change to that behaviour needs those updated, and CI greps the stub relay output for fixed strings. <!-- src: docs/trust.md:3, .github/workflows/ci.yml:49-56 -->
- Dependabot covers GitHub Actions only, because `go.mod` is overwritten from upstream. <!-- src: .github/dependabot.yml:2-6 -->

## Unverified

- How releases are built, signed and published lives in the private monorepo, so it is not documented here beyond what CONTRIBUTING.md and README.md say.
- The installer script is not in this repository. What it does is described in `docs/install.md` as read from the served script, and was not re-checked for this guide.
- `restart_*.go` appears in CONTRIBUTING.md and `.cortex-gate.json`, but no such file is in this tree.

## Generated facts

<!-- auto:start -->
- go module: hotend-agent
- go version: 1.22
- go requires (1): github.com/gorilla/websocket
<!-- auto:end -->
