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

The agent starts no other outbound traffic. It does not read your filesystem
beyond its own small state file, and it ships no analytics.

## Install

Grab a prebuilt binary from the [releases](../../releases) page (Linux
`amd64` / `armv7` / `arm64`), or build from source (below).

```sh
# put the binary somewhere on PATH
sudo install -m755 hotend-agent /usr/local/bin/hotend-agent

# minimal run — the app gives you the exact command (with the right relay URL)
# when you add a printer, so you normally won't type this by hand
HOTEND_RELAY_URL=wss://app.hotend.app/ hotend-agent
```

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
| `HOTEND_AGENT_STATE` | `~/.hotend-agent.json` | Where the printer ID and device secret are stored (mode `0600`). |
| `HOTEND_SPOOLMAN_PORT` | `7913` | Port for the local Spoolman-compatible endpoint Moonraker can point at. |

## Build from source

Requires Go 1.22+.

```sh
go build -o hotend-agent .
```

Cross-compile for a Pi:

```sh
make            # builds amd64, armv7 and arm64 into ./dist
```

## Self-update

The agent checks the relay for a newer build every 6 hours. It downloads to a
temp file, runs it once and confirms it reports the expected version before
atomically swapping it in — a bad download can never replace a working binary.
It then exits and lets your service manager (`Restart=always`) relaunch it. If
you'd rather manage updates yourself, run it under a manager that doesn't restart
it, or block the relay's `/agent-bin/` path.

## License

MIT — see [LICENSE](LICENSE).
