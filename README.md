# hotend-agent

The printer-side agent for [Hotend](https://hotend.app). It runs on the same box
as your printer (a Raspberry Pi, an old mini-PC, anything on the same network),
dials out to the Hotend relay over a single WebSocket, and forwards the traffic
the app asks for to your local Moonraker instance.

This is the whole of what runs on your hardware. It's published here so you can
read exactly what it does before installing it, build it yourself if you'd
rather, and firewall it however you like.

## How it works

The agent makes **one outbound connection** — a `wss://` WebSocket to the relay.
It never opens a listening port to the internet. When you open the app, the relay
asks the agent to make a specific local request (a Moonraker REST call, a webcam
frame, a Moonraker WebSocket subscription), the agent makes it against
`127.0.0.1`, and streams the response back over the same connection.

Because everything is outbound, there is nothing to port-forward and nothing on
your LAN is reachable from outside. If the relay is down, or you stop the agent,
your printer is untouched — Mainsail, Fluidd and anything else on the network keep
working exactly as before.

On first run the agent generates a random printer ID and prints a pairing code
(or links automatically if you pass a claim token). The relay then issues a
per-device secret, which the agent stores locally and presents on every
reconnect, so a leaked printer ID alone can't be used to impersonate your
printer.

## What leaves your network

Only what the app requests, when it requests it:

- Moonraker API traffic — status, temperatures, motion, macros, and uploading
 g-code to start a print
- Webcam frames — when you're watching the stream or the failure detector is on
- Filament-usage totals — for Spoolman-style spool tracking
- The agent's own version, on connect and on each update check, so the fleet's
 update adoption is visible

The agent starts no other outbound traffic. It does not read your filesystem
beyond its own small state file, and it ships no analytics.

## Install

The supported path, which fetches the current release, **verifies its signature**
(see [Self-update](#self-update)) and installs a service:

```sh
curl -fsSL https://app.hotend.app/install | sh
```

First installs are verified **fail-closed**. The installer downloads the tiny
dependency-free checker in [`verify-sig/`](verify-sig/) (served per-architecture
beside the release) and verifies the agent binary's ed25519 signature against the
keys pinned in the install script before anything is installed. A missing
signature, an unfetchable checker, or a signature that doesn't verify all
**refuse the install and change nothing** — there is no silent "couldn't check,
so allow it". The only way past a failed check is the explicit opt-out
`HOTEND_ALLOW_UNVERIFIED_INSTALL=1`. (Earlier installers shelled out to system
openssl for this, which was missing or too old on some printer images and
skipped verification when it was — that gap is gone.)

Or build from source (below) and run it yourself:

```sh
# Install into a directory YOUR user owns — self-update swaps the binary in
# place, so the running user must be able to write it. No root anywhere.
install -Dm755 hotend-agent ~/.hotend-agent/hotend-agent

# minimal run — the app gives you the exact command (with the right relay URL)
# when you add a printer, so you normally won't type this by hand
HOTEND_RELAY_URL=wss://app.hotend.app/ ~/.hotend-agent/hotend-agent
```

If you prefer a root-owned binary in `/usr/local/bin`, that works too — but the
agent (correctly) cannot self-update a file it doesn't own, so set
`HOTEND_AGENT_AUTO_UPDATE=off` and update by reinstalling. **Don't run the agent
as root to make self-update work**; it needs no privileges at all.

On first start it prints a pairing code; enter it in the app under **Add
printer**. To run it unattended, use the systemd unit in
[`systemd/hotend-agent.service`](systemd/hotend-agent.service).

## Configuration

Everything is an environment variable. Only `HOTEND_RELAY_URL` is required.

| Variable | Default | Purpose |
| --- | --- | --- |
| `HOTEND_RELAY_URL` | *(required)* | The relay to dial, e.g. `wss://app.hotend.app/`. Must be `wss://` (or `ws://localhost` for local testing). |
| `HOTEND_LOCAL_URL` | `http://127.0.0.1:7125` | Your Moonraker API. |
| `HOTEND_WEBCAM_URL` | `http://127.0.0.1:8080` | Webcam stream base (mjpg-streamer, crowsnest, …). |
| `HOTEND_CLAIM` | *(none)* | One-time claim token to auto-link on first connect instead of reading a pairing code. |
| `HOTEND_AGENT_STATE` | `~/.config/hotend/agent.json` | Where the printer ID and device secret are stored (mode `0600`). |
| `HOTEND_SPOOLMAN_PORT` | `7913` | Port for the local Spoolman-compatible endpoint Moonraker can point at. |
| `HOTEND_AGENT_AUTO_UPDATE` | `on` | Set to `off` to disable self-update entirely — a local kill switch that works even if you trust nothing the relay says. Cloud access is unaffected. |

### Your printer's identity lives outside the install directory

`~/.config/hotend/agent.json` holds the printer ID and device secret, and it is
deliberately **not** inside the install directory: reinstalling shouldn't cost you
your pairing. Installs from before 2026.07.22 kept it at `~/.hotend-agent/agent.json`
or `~/.hotend-agent.json`; the agent migrates from both automatically and never
writes back to them.

Delete that file and the printer is orphaned — the agent generates a new identity
and needs a fresh pairing code, while the old printer stays in the app as
permanently offline (remove it there). Two things worth knowing if you land in that
state: run the agent as the same user every time, because as root the identity is
`/root/.config/hotend/agent.json` rather than your own, and you can re-link without
touching files by adding the printer in the app and passing the claim token it
gives you:

```sh
HOTEND_CLAIM=<token> hotend-agent      # or re-run the installer, which does this for you
```

If the agent is running as a service, its pairing code is in the log:

```sh
journalctl --user -u hotend-agent -n 30 --no-pager
```

## Self-update

The agent checks the relay for a newer build roughly every 6 hours (with jitter,
so the fleet doesn't arrive at once). What it will accept is deliberately narrow:

- **Signed, or refused.** Every release is signed offline with an ed25519 key whose
 public half is compiled into this source (`updatePubKeysHex` in `main.go`). The
 agent downloads the `.sig` beside the binary and verifies it over the exact bytes
 before anything else. Releases also publish a signed **manifest** that binds the
 version string to each architecture's sha256 — so a compromised relay cannot pair
 an old signed binary with a new version. The signing key never touches the server
 that distributes the binaries. It's a list rather than one constant so a key can
 be rotated: ship a release trusting old and new, wait for adoption, then sign
 with the new one.
- **Run-and-report.** The downloaded binary must execute and report the version it
 claims to be before it replaces anything. This runs *after* the signature and
 manifest checks — the agent never executes bytes that haven't already verified
 against the pinned keys — and the version it reports is cross-checked against
 the signed manifest, not taken on faith.
- **Atomically, then re-exec.** The swap is a rename, and the agent then execs the
 new binary in place. It doesn't depend on a service manager to restart it, so this
 works even when it's running in a plain shell, and there's no window where your
 printer is offline.
- **Reversible.** The replaced binary is kept. If the new one can't reach the relay
 within 15 minutes or 3 restarts, the agent puts the old one back and refuses to
 install that version again.
- **Forward-only.** A version that isn't strictly newer is ignored, so nobody can
 push you *backwards* to an older build. (The flip side: a bad release is fixed by
 publishing a higher version, not by re-publishing an old one.)

To manage updates yourself, set `HOTEND_AGENT_AUTO_UPDATE=off` (a local kill
switch the relay can't override), block the relay's `/agent-bin/` path at your
firewall, or build from source and install by hand. All three leave cloud
access working — the agent just stops updating.

## Verify a release

You don't have to take the above on faith — this checks what the server is
actually serving against the keys in this source:

```sh
node scripts/verify-agent-release.mjs             # what the world is served
node scripts/verify-agent-release.mjs --local     # …and that it matches your build
```

It downloads each architecture's binary and signature, verifies the signature over
the bytes, checks the signed release manifest (version ↔ sha256), and confirms the
advertised version matches this tree.

The installer's first-install check uses the same primitive without needing Node:
[`verify-sig/`](verify-sig/) is a ~60-line static Go binary that does exactly one
thing — verify an ed25519 signature over a file against a list of hex public keys
— so what runs on your printer before the agent exists is just as auditable as
the agent itself. (It is deliberately unsigned: it *is* the thing that checks
signatures, so there is nothing upstream of it to verify it against — trust in it
comes from HTTPS to the relay plus this source.)

## Build from source

Requires Go 1.22+.

```sh
make            # host build
make dist       # linux amd64, armv7, arm64 into ./dist
go test ./...
```

Builds are static (`CGO_ENABLED=0`), stripped of paths (`-trimpath`), and carry no
VCS stamp (`-buildvcs=false`), which is what makes them reproducible — that's why
`--local` above can compare your bytes to the published ones.

One sharp edge for byte-identical reproduction: use the **official go.dev
toolchain** for the exact version a release was built with (currently `go1.22.2`,
pinned in the [reproducible workflow](.github/workflows/reproducible.yml)).
Distro packages that report the same version — Ubuntu's `golang-1.22` is one —
are patched compilers that emit different (equally functional) bytes and will
fail the comparison. Any Go 1.22+ still builds a perfectly working agent; the
exact-toolchain rule only matters when you're checking bytes. The `reproducible`
workflow does this check on a GitHub-hosted runner for every release, so you can
also just look for the green run. Static also matters on its own: a dynamically linked build picks
up the glibc of whatever machine compiled it and won't start on an older printer
image.

## License

MIT — see [LICENSE](LICENSE).
