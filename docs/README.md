# hotend-agent docs

Index of this repo's documentation. Every doc in this folder is linked here, one line each.

- [AGENTS.md](../AGENTS.md): commands, conventions and traps for coding agents (CLAUDE.md imports it).
- [ARCHITECTURE.md](ARCHITECTURE.md): components, data flow, storage, auth, external services.
- [DECISIONS.md](DECISIONS.md): dated record of why it is built the way it is.
- [README.md](../README.md): what it is and how to use it; stands in for a PRD.
- [configuration.md](configuration.md): every environment variable, and where the printer's identity file lives.
- [install.md](install.md): what the supported installer does, and how to build and run the agent by hand.
- [self-update.md](self-update.md): how the agent accepts an update and how to turn updates off.
- [trust.md](trust.md): what the agent can reach and what it sends, read from `main.go`.
- [uninstall.md](uninstall.md): removing the agent for plain-process, systemd and busybox installs.
- [verifying.md](verifying.md): checking a release by the served files, a rebuild or a hand signature check.
