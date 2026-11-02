# hotend-agent

[![ci](https://github.com/kd-011/hotend-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/kd-011/hotend-agent/actions/workflows/ci.yml)

The printer-side agent for [Hotend](https://hotend.app). It runs on the same box
as your printer (a Raspberry Pi, an old mini-PC, anything on the same network),
dials out to the Hotend relay over a single WebSocket, and forwards the traffic
the app asks for to your local Moonraker instance.

This is all of what runs on your hardware. The source is here so you can read it
before you install, build it yourself, and firewall it however you like.

## How it works

The agent makes one outbound connection, a `wss://` WebSocket to the relay. It
never opens a listening port to the internet. When you open the app, the relay
asks the agent to make a specific local request (a Moonraker REST call, a webcam
frame, a Moonraker WebSocket subscription). The agent makes it against
`127.0.0.1` by default and streams the response back over the same connection.
Two things go beyond that: the relay can set the webcam URL the agent fetches, and
it can ask for a raw TCP tunnel to a host on your LAN (the agent refuses any host
that is not on a private, loopback or link-local address). [SECURITY.md](SECURITY.md)
covers what that means if the relay is compromised.

Everything is outbound, so there is nothing to port-forward and nothing on your
LAN is reachable from outside. If the relay is down or you stop the agent, your
printer is untouched. Mainsail, Fluidd and anything else on the network keep
working as before.

On first run the agent generates a random printer ID and prints a pairing code
(or links automatically if you pass a claim token). The relay then issues a
per-device secret. The agent stores it locally and presents it on every
reconnect, so a leaked printer ID alone can't be used to impersonate your
printer.

## What leaves your network

Only what the app requests, when it requests it:

- Moonraker API traffic: status, temperatures, motion, macros, and uploading
 g-code to start a print
- Webcam frames, when you're watching the stream or the failure detector is on
- Filament-usage totals, for Spoolman-style spool tracking
- The agent's own version, on connect and on each update check, so update adoption
 across the fleet is visible

The agent starts no other outbound traffic. It does not read your filesystem
beyond its own small state file, and it ships no analytics.

## Install

The supported path fetches the current release, verifies its signature
(see [Self-update](#self-update)) and installs a service:

```sh
curl -fsSL https://app.hotend.app/install | sh
```

First installs fail closed. The installer downloads the small dependency-free
checker in [`verify-sig/`](verify-sig/) (served per architecture beside the
release) and verifies the agent binary's ed25519 signature against the keys
pinned in the install script before anything is installed. If the signature is
missing, the checker can't be fetched or the signature doesn't verify, the
installer stops and changes nothing. It never skips the check quietly. The only
way past a failed check is the explicit opt-out
`HOTEND_ALLOW_UNVERIFIED_INSTALL=1`. Earlier installers used the system openssl
for this, which was missing or too old on some printer images, and they skipped
verification when it was. That gap is closed.

Or build from source (below) and run it yourself:

```sh
# Install into a directory YOUR user owns. Self-update swaps the binary in
# place, so the running user must be able to write it. No root anywhere.
install -Dm755 hotend-agent ~/.hotend-agent/hotend-agent

# minimal run. The app gives you the exact command (with the right relay URL)
# when you add a printer, so you normally won't type this by hand
HOTEND_RELAY_URL=wss://app.hotend.app/ ~/.hotend-agent/hotend-agent
```

A root-owned binary in `/usr/local/bin` also works. The agent can't self-update a
file it doesn't own, so set `HOTEND_AGENT_AUTO_UPDATE=off` and update by
reinstalling. Don't run the agent as root to get self-update working. It needs no
privileges at all.

On first start it prints a pairing code. Enter it in the app under **Add
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
| `HOTEND_AGENT_AUTO_UPDATE` | `on` | Set to `off` to disable self-update entirely. This is a local kill switch that works even if you trust nothing the relay says. Cloud access is unaffected. |

### Your printer's identity lives outside the install directory

`~/.config/hotend/agent.json` holds the printer ID and device secret. It sits
outside the install directory so that reinstalling doesn't cost you your pairing.
Installs from before 2026.07.22 kept it at `~/.hotend-agent/agent.json` or
`~/.hotend-agent.json`. The agent migrates from both automatically and never
writes back to them.

If you delete that file the printer is orphaned. The agent generates a new
identity and needs a fresh pairing code, and the old printer stays in the app as
permanently offline (remove it there). Run the agent as the same user every time,
because as root the identity is `/root/.config/hotend/agent.json` rather than your
own. You can re-link without touching files by adding the printer in the app and
passing the claim token it gives you:

```sh
HOTEND_CLAIM=<token> hotend-agent      # or re-run the installer, which does this for you
```

If the agent is running as a service, its pairing code is in the log:

```sh
journalctl --user -u hotend-agent -n 30 --no-pager
```

## Self-update

The agent checks the relay for a newer build roughly every 6 hours, with jitter
so the whole fleet doesn't arrive at once. What it will accept is narrow:

- Signed. Every release is signed offline with an ed25519 key whose public half is
 compiled into this source (`updatePubKeysHex` in `main.go`). The agent downloads
 the `.sig` beside the binary and verifies it over the exact bytes before doing
 anything else. Releases also publish a signed manifest that binds the version
 string to each architecture's sha256, so a compromised relay cannot pair an old
 signed binary with a new version. The signing key never touches the server that
 distributes the binaries. The keys are a list so one can be rotated: ship a
 release that trusts old and new, wait for adoption, then sign with the new one.
- It runs and reports its version. The downloaded binary must execute and report
 the version it claims to be before it replaces anything. This happens after the
 signature and manifest checks, so the agent never executes bytes that haven't
 already verified against the pinned keys. The reported version is cross-checked
 against the signed manifest.
- The swap is a rename, and the agent then execs the new binary in place. It
 doesn't depend on a service manager to restart it, so this works in a plain shell
 too, and your printer is never offline during the update.
- It can be reversed. The replaced binary is kept. If the new one can't reach the
 relay within 15 minutes or 3 restarts, the agent puts the old one back and
 refuses to install that version again.
- It only moves forward. A version that isn't strictly newer is ignored, so nobody
 can push you back to an older build. A bad release is fixed by publishing a
 higher version, not by re-publishing an old one.

To manage updates yourself, set `HOTEND_AGENT_AUTO_UPDATE=off` (a local kill
switch the relay can't override), block the relay's `/agent-bin/` path at your
firewall, or build from source and install by hand. Cloud access keeps working in
all three cases. The agent just stops updating.

## Verify a release

You can check the above yourself. This script compares what the server is
actually serving against the keys in this source:

```sh
node scripts/verify-agent-release.mjs             # what the world is served
node scripts/verify-agent-release.mjs --local     # …and that it matches your build
```

It downloads each architecture's binary and signature, verifies the signature over
the bytes, checks the signed release manifest (version ↔ sha256), and confirms the
advertised version matches this tree.

The installer's first-install check uses the same primitive without needing Node.
[`verify-sig/`](verify-sig/) is a ~60-line static Go binary that verifies an
ed25519 signature over a file against a list of hex public keys and does nothing
else, so what runs on your printer before the agent exists is as easy to audit as
the agent. It is unsigned on purpose: it is the thing that checks signatures, so
there is nothing upstream to verify it against. Trust in it comes from HTTPS to
the relay plus this source.

## Build from source

Requires Go 1.22+.

```sh
make            # host build
make dist       # linux amd64, armv7, arm64 into ./dist
go test ./...
```

Builds are static (`CGO_ENABLED=0`), stripped of paths (`-trimpath`), and carry no
VCS stamp (`-buildvcs=false`). That makes them reproducible, which is why
`--local` above can compare your bytes to the published ones.

For byte-identical builds, use the official go.dev toolchain at the exact version
a release was built with (currently `go1.22.2`, pinned in the
[reproducible workflow](.github/workflows/reproducible.yml)). Distro packages that
report the same version, Ubuntu's `golang-1.22` for one, are patched compilers
that emit different (equally functional) bytes and will fail the comparison. Any
Go 1.22+ builds a working agent. The exact toolchain only matters when you're
checking bytes. The `reproducible` workflow does this check on a GitHub-hosted
runner for every release, so you can also just look for the green run. A static
build also matters on its own: a dynamically linked one picks up the glibc of
whatever machine compiled it and won't start on an older printer image.

## Contributing and security

The agent source here is a published copy of a private monorepo. [CONTRIBUTING.md](CONTRIBUTING.md) explains how pull requests are handled. [SECURITY.md](SECURITY.md) explains how to report a vulnerability privately and how to check a release yourself.

## License

MIT. See [LICENSE](LICENSE).
