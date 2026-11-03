# Configuration

Everything is an environment variable. Only `HOTEND_RELAY_URL` is required. The installer writes these into the service unit for you.

| Variable | Default | Purpose |
| --- | --- | --- |
| `HOTEND_RELAY_URL` | none | The relay to dial, for example `wss://app.hotend.app/`. It must be `wss://`, or start with `ws://127.0.0.1` or `ws://localhost` for local testing. |
| `HOTEND_LOCAL_URL` | `http://127.0.0.1:7125` | Your Moonraker API. |
| `HOTEND_WEBCAM_URL` | `http://127.0.0.1:8080` | Webcam stream base (mjpg-streamer, crowsnest and similar). The relay can override it live. |
| `HOTEND_CLAIM` | none | One-time claim token that links the printer on first connect instead of showing a pairing code. |
| `HOTEND_AGENT_STATE` | `~/.config/hotend/agent.json` | Where the printer ID and device secret are kept, with mode `0600`. |
| `HOTEND_SPOOLMAN_PORT` | `7913` | Port of the Spoolman-compatible endpoint on `127.0.0.1` that Moonraker can point at. |
| `HOTEND_AGENT_AUTO_UPDATE` | `on` | `off` disables self-update. The relay cannot override it. Cloud access is unaffected. |

## Where the printer's identity lives

`~/.config/hotend/agent.json` holds the printer ID and the device secret. It sits outside the install directory so that reinstalling does not cost you your pairing. Installs from before 2026.07.22 kept it at `~/.hotend-agent/agent.json` or `~/.hotend-agent.json`. The agent reads from both and never writes back to them.

If you delete the file, the printer is orphaned. The agent makes a new identity and needs a fresh pairing code, and the old printer stays in the app as offline until you remove it. Run the agent as the same user every time: as root the file is `/root/.config/hotend/agent.json`.

To re-link without touching files, add the printer in the app and start the agent with the claim token it gives you:

```sh
HOTEND_CLAIM=<token> hotend-agent
```

When the agent runs as a user service, its pairing code is in the journal:

```sh
journalctl --user -u hotend-agent -n 30 --no-pager
```
