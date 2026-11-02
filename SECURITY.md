# Security

## Reporting a vulnerability

Use GitHub private vulnerability reporting: open the Security tab of this repository and choose "Report a vulnerability". The report stays private until a fix is out. Please don't open a public issue for something exploitable.

Say what you ran, what you expected and what happened. A reproduction against the current release helps most.

The Hotend relay and app aren't in this repository, but report problems with them here too and they will be passed on.

## What is in scope

The agent runs on your printer host, so scope covers what it accepts and what it does on your network:

- Release signing and self-update. The agent verifies an ed25519 signature over the exact bytes of a new binary, and over a signed manifest that binds the version to each architecture's sha256, against public keys compiled into `main.go`. It refuses anything unsigned, runs the new binary only after those checks pass, keeps the old binary, rolls back if the new one cannot reach the relay within 15 minutes or 3 restarts, and ignores any version that is not strictly newer. A way around any of these is a vulnerability.
- The installer's first-install check, done by [`verify-sig/`](verify-sig/). It must fail closed: a missing signature or checker, or a signature that does not verify, refuses the install.
- Request targets. Moonraker requests go to the address the agent is configured with. The webcam address works differently: the relay can set it live, the agent does not check it against private ranges, and webcam requests go to whatever it points at, with the response returned to the relay. The agent can also open raw TCP tunnels the relay asks for, so relay-side printer drivers can reach a printer on your LAN. Those tunnels are limited to hosts whose every resolved address is private, loopback or link-local, and the agent dials the address it checked rather than resolving the name a second time. A compromised relay can therefore reach LAN devices through tunnels and any URL through the webcam path. These are known limits of how far the agent trusts the relay, and other request paths are still in scope. In scope: a tunnel that reaches a public address, a way around that check (DNS rebinding, for example), a listening port exposed to the internet, and any other request path that lets the relay reach a target you did not expect.
- The state file holding the printer ID and device secret, which is written with mode `0600`.

## Checking a release yourself

- `node scripts/verify-agent-release.mjs` downloads what the relay serves for each architecture and checks signatures and the signed manifest against the keys in this source. Add `--local` to compare against your own build.
- The `reproducible` workflow rebuilds every release from its tag on a GitHub-hosted runner with the pinned Go toolchain, fails if the bytes differ from the published assets, and re-verifies the signatures and manifest hashes. A green run for a release means the published binaries match this source.
- Setting `HOTEND_AGENT_AUTO_UPDATE=off` stops self-update entirely, whatever the relay says.

## Signing keys

The maintainer signs releases offline. The public keys are a list in `main.go` so a key can be rotated without stranding printers. If you think a signing key is compromised, report it through the channel above straight away.

## Supported versions

Only the latest release gets fixes. The agent updates itself, and a bad release is fixed by publishing a higher version, because an older version is never installed over a newer one.
