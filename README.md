# hotend-agent

[![ci](https://github.com/kd-011/hotend-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/kd-011/hotend-agent/actions/workflows/ci.yml)

hotend-agent is the program [Hotend](https://hotend.app) puts on your printer's host. It opens one outbound WebSocket to the Hotend relay and passes the app's requests to your Moonraker. This repository is the source of that program, so you can read what runs there and check a release before it goes near your network.

It needs a Hotend account to do anything useful. If you don't want a third party between you and your printer, see [when not to use it](#when-not-to-use-it).

## Watch it run

You can see everything the agent sends without an account and without a printer. This builds it, starts a stand-in relay and a stand-in Moonraker on loopback, and prints each request. It needs Go 1.22 and Node 18.

```sh
git clone https://github.com/kd-011/hotend-agent && cd hotend-agent
make && node scripts/stub-relay.mjs ./hotend-agent
```

On the machine this README was written on, that took about 10 seconds, most of it the Go build. The output below is from a real run, with one line about filament tracking left out. The relay is the stand-in, and it asks for the two Moonraker paths at 1.5 s and 2 s.

```text
0.007s  stub      relay on 127.0.0.1:36791, moonraker on 127.0.0.1:43883, state in /tmp/hotend-stub-6eVjEC
0.014s  agent     says [agent] hotend-agent 2026.09.06.1 starting
0.014s  agent     says [agent] spoolman endpoint on http://127.0.0.1:36792 (Moonraker [spoolman] server:)
0.016s  relay     WebSocket /__relay/agent?printer=5131b6c53fddc276136a5b942261f8a4
0.016s  relay       header x-hotend-agent-version: 2026.09.06.1
0.016s  relay       header user-agent: Go-http-client/1.1
0.017s  moonraker GET /server/config
 0.02s  agent     says [agent] connected to ws://127.0.0.1:36791 (printer 5131b6c5…), proxying http://127.0.0.1:43883
  0.02s  moonraker GET /server/webcams/list
0.021s  moonraker GET /websocket
1.014s  relay     HTTP GET /agent-bin/version ?printer=5131b6c53fddc276136a5b942261f8a4&have=2026.09.06.1
1.517s  relay     asks the agent: GET /printer/info
1.518s  moonraker GET /printer/info
1.519s  agent     sent reply to request 1: status 200
1.519s  agent     sent body for request 1: {"result":{"state":"ready","hostname":"stub"}}
1.519s  agent     sent end of request 1
2.018s  relay     asks the agent: POST /printer/gcode/script?script=M115
2.018s  moonraker POST /printer/gcode/script?script=M115
2.019s  agent     sent reply to request 2: status 200
2.019s  agent     sent body for request 2: {"result":"ok"}
2.019s  agent     sent end of request 2
4.014s  stub      done, agent stopped
```

[`scripts/stub-relay.mjs`](scripts/stub-relay.mjs) is about 130 lines with no dependencies. It uses a throwaway state file and never contacts the real relay.

## What it does

It dials out to the relay over `wss://` and keeps that connection open. When you use the app, the relay sends a request, the agent makes the matching call to your Moonraker (`http://127.0.0.1:7125` by default) or webcam, and the answer goes back down the same connection. It opens no port to the internet. Its one listener is a Spoolman-compatible endpoint on `127.0.0.1:7913`. If you stop the agent, Mainsail and Fluidd carry on as before.

## What it can reach

The agent has no list of allowed paths. Whatever path and method the relay names, it sends to your Moonraker, so the limit is whatever your Moonraker allows a local client. I have not tested that against a real Moonraker, so check your `trusted_clients` setting. The relay can also point the webcam fetch at any URL and ask for a raw TCP tunnel to any port on a private, loopback or link-local address, which includes services on the printer host itself. The agent runs as your user and needs no root. Details and the code behind each claim are in [docs/trust.md](docs/trust.md), and [SECURITY.md](SECURITY.md) has the scope.

## What it sends

- On connect: the printer ID, the agent version, the device secret in a header, and the one-time claim token while `HOTEND_CLAIM` is set.
- About every 6 hours: the printer ID and running version, in a version check. `HOTEND_AGENT_AUTO_UPDATE=off` stops it.
- Whatever the relay asks for: Moonraker responses, webcam frames, TCP tunnel data.
- Without being asked: filament totals during prints (about every 50 mm, and at print end), a once-a-minute active spool lookup when Moonraker has `[spoolman]`, requests Moonraker makes to the local Spoolman endpoint, and the detected webcam URL, which can hold a LAN address.

I found no code that reads the host name, network interfaces, `/proc` or `/etc`. There is no analytics.

## Check a release yourself

```sh
node scripts/verify-agent-release.mjs
```

That downloads each architecture from the relay and checks its signature and the signed manifest against the keys in `main.go`. It takes about a minute. For a tagged release you can also rebuild with the official Go toolchain and compare bytes, which the `reproducible` workflow does on a GitHub runner. Steps are in [docs/verifying.md](docs/verifying.md).

Releases are signed offline and mirrored here by hand, so the binary the relay serves can be newer than the newest tag here. For that binary you can check the signature but not rebuild it from public source until it is mirrored. The script reports the version difference as a failure.

## Install

Hotend shows you the exact command, with your claim token, when you add a printer. The core of it is:

```sh
curl -fsSL https://app.hotend.app/install | sh
```

The installer is not in this repository. To read it first, save it and open it before running it:

```sh
curl -fsSL https://app.hotend.app/install -o install.sh && less install.sh && sh install.sh
```

It checks the agent's signature against the same two keys as `main.go` and installs nothing if the check fails. It also scans your /24 for Moonraker when it can't find it locally, and edits `moonraker.conf` on systemd machines. [docs/install.md](docs/install.md) lists what it does, as read from the served script, and how to build and run the agent by hand.

## Remove it

Stop and remove the service, delete its files and undo the Moonraker edit, then remove the printer in the app. [docs/uninstall.md](docs/uninstall.md) has the commands for each install type.

## When not to use it

- You don't want a company's server between you and your printer. A VPN you run yourself gives you the real Mainsail with nothing in the middle, and [Mainsail's remote access page](https://docs.mainsail.xyz/faq/remote-access/) recommends it. It also lists Tailscale, with the caveat that you don't control the connection.
- You want the whole stack open. Only this agent is published. The relay and the app are not. [Obico](https://www.obico.io/blog/klipper-remote-access/) describes itself as open source and self-hostable, and its Moonraker plugin is AGPL-3.0.
- You have no Hotend account. Without one the agent connects to nothing.
- Your printer host is not linux on amd64, arm or arm64. This repository builds only those.

## More

[Configuration](docs/configuration.md), [self-update](docs/self-update.md), [CONTRIBUTING](CONTRIBUTING.md) (this repository is a copy of a private monorepo, so pull requests are handled differently), [SECURITY](SECURITY.md). MIT licence.
