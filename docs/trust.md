# What the agent can reach and what it sends

Everything here was read from `main.go` at the version in this tree. A running printer may be on an older release, so check `hotend-agent -version` against what you read. The stand-in relay in [`scripts/stub-relay.mjs`](../scripts/stub-relay.mjs) shows the same behaviour live.

## What it can reach

- Moonraker. The agent sends any path and method the relay names to `HOTEND_LOCAL_URL` (`buildLocalURL`, `doFetch`). It rejects paths that don't start with a single `/`, so the relay can't redirect the request to another host. It strips forwarding headers, `Authorization`, `Origin` and a few more (`stripReq`) so Moonraker treats the call as a local client. What a local client may do is set by your Moonraker configuration, and I have not tested that against a real Moonraker. Calls under `/server/spoolman/` are answered by the agent instead.
- Moonraker over WebSocket, by the same rules (`doWSOpen`).
- The webcam. The relay can set the URL live (the `config` message). The agent does not check it against private ranges, and the response goes back to the relay. The agent also probes a fixed list of webcam paths on the Moonraker host and on port 8080 to detect the camera, and reports what it finds.
- Raw TCP tunnels. The relay names a host and port. The agent resolves the host, requires every address to be private, loopback or link-local, then dials the address it checked, so a second DNS answer can't change the target (`resolvePrivateHost`, `doTCPOpen`). Any port from 1 to 65535 is allowed. Loopback is allowed, so a tunnel can reach any TCP service on the printer host itself, and so can any private address on your LAN.
- Local files. The agent reads and writes its state file. During an update it also writes the new binary next to itself, keeps the old one as `.prev` and swaps them.
- Local ports. One listener, on `127.0.0.1:7913` (`HOTEND_SPOOLMAN_PORT`). I checked with `ss -ltnp` while it ran.

It runs as the user who starts it and uses no privileges. It also asks Moonraker for `/server/config` (to see whether `[spoolman]` is set), subscribes to `/websocket` for print stats when it has to count filament itself, and, when `[spoolman]` is set, rewrites Moonraker's active spool to match the account's about once a minute.

## What it sends

To the relay, on connect: the printer ID in the URL, the agent version in `X-Hotend-Agent-Version`, the device secret as a bearer token once issued, and the claim token in the URL while `HOTEND_CLAIM` is set. The installer's unit sets `HOTEND_CLAIM` permanently, and the token is described as single use. The relay sees your public IP, as any server would.

To the relay, on request: Moonraker responses, webcam frames, TCP tunnel data, and the replies to Spoolman requests the relay itself makes.

To the relay, unprompted:

- The detected webcam URL. When the agent runs on a bridge machine this can include a LAN address.
- Filament totals, when Moonraker has no `[spoolman]` section and the agent counts for itself (`trackFilament`). It subscribes to Moonraker's `print_stats` and sends `PUT /api/v1/_hotend/active/use` with the absolute `filament_used` about every 50 mm during a print, and once more at print end.
- An active spool lookup, `GET /api/v1/_hotend/active`, about once a minute when Moonraker does have `[spoolman]` (`syncMoonrakerSpool`).
- Whatever local clients send to the Spoolman endpoint on `127.0.0.1:7913`. Moonraker does this when `[spoolman]` points at it. The agent forwards each request to the relay (`spoolmanHandler`) and returns the reply.

Version check, at start and then every 6 hours plus up to an hour of random delay: `GET <relay>/agent-bin/version?printer=<id>&have=<version>`. If `HOTEND_AGENT_AUTO_UPDATE=off`, the function returns before any request.

I searched `main.go` for the host name, network interfaces, `/proc`, `/etc` and the process environment. Nothing reads or sends them. The environment is handed only to the updated binary, when it is test-run and when the agent re-executes itself.

## Where to look

The `dispatch` function lists every message type the relay can send: `pairing`, `linked`, `config`, `device-secret`, `smres`, `req`, `body`, `end`, `cancel`, `wsopen`, `wsmsg`, `wsclose`, `tcpopen`, `tcpdata`, `tcpclose`. Nothing else is acted on.
