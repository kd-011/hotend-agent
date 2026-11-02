---
slug: hotend-agent
name: hotend-agent
repo: .
status: active
last_verified: 2026-11-02
tags: [go, agent, 3d-printing]
---

The printer-side agent for Hotend. It dials out to a relay over one WebSocket and forwards the requests the app asks for to the local Moonraker. These claims are the README's main promises, each tied to the code that keeps it.

## Features

- Refuses to start unless the relay URL is `wss://`, with `ws://` allowed only for `127.0.0.1` and `localhost`. <!-- src: main.go:285-288 -->
- Talks to Moonraker at `http://127.0.0.1:7125` and the webcam at `http://127.0.0.1:8080` unless the environment says otherwise. <!-- src: main.go:73-74 -->
- Stores the printer ID and device secret in `agent.json` under a `hotend` config directory, written with mode `0600`. <!-- src: main.go:82-89, main.go:236-242 -->
- The Spoolman-compatible endpoint listens on `127.0.0.1` only, on port 7913 by default. <!-- src: main.go:136, main.go:1097 -->
- Self-update can be switched off locally with `HOTEND_AGENT_AUTO_UPDATE` set to `off`, `0`, `false` or `no`. <!-- src: main.go:595-598 -->
- Checks for a newer build every 6 hours plus up to an hour of random jitter. <!-- src: main.go:353 -->
- Accepts a release only if its signature verifies under one of the public keys compiled into the source. <!-- src: main.go:50, main.go:745-755 -->
- Ignores any release version that is not strictly newer than the running one. <!-- src: main.go:496-498 -->
- Rolls a new binary back if it has not reached the relay within 15 minutes or 3 boots. <!-- src: main.go:326-331 -->
- The first-install checker `verify-sig` takes a file, a hex signature file and one or more hex public keys, and exits 1 unless one key verifies. <!-- src: verify-sig/main.go:27-31, verify-sig/main.go:50-57 -->

## Architecture

- One Go module with a single dependency, `gorilla/websocket`, on Go 1.22. <!-- src: go.mod -->
- Builds are static (`CGO_ENABLED=0`) and use `-trimpath -buildvcs=false`, which keeps them reproducible. <!-- src: Makefile:12-13 -->

## Deploy

- CI vets, builds the amd64, arm64 and arm binaries, rejects any that is dynamically linked, and runs the tests on every pull request and weekly. <!-- src: .github/workflows/ci.yml:6-10, .github/workflows/ci.yml:30-31, .github/workflows/ci.yml:35-47 -->
- Every published release is rebuilt on a GitHub runner with Go 1.22.2 and the job fails if the bytes differ from the published binaries. <!-- src: .github/workflows/reproducible.yml:54, .github/workflows/reproducible.yml:72-82 -->

<!-- auto:start -->
- go module: hotend-agent
- go version: 1.22
- go requires (1): github.com/gorilla/websocket
<!-- auto:end -->
