# Install

## The supported installer

Hotend shows the command, with your claim token, when you add a printer:

```sh
curl -fsSL https://app.hotend.app/install | sh
```

The installer is not in this repository. To read it first:

```sh
curl -fsSL https://app.hotend.app/install -o install.sh && less install.sh && sh install.sh
```

I read the served installer end to end in November 2026. It:

- finds Moonraker on `127.0.0.1:7125`, then a few `.local` names, then scans your /24 for port 7125. It prints a warning before the scan. Set `HOTEND_LOCAL_URL` to skip all of it.
- downloads the agent, its signature and the [`verify-sig`](../verify-sig/) checker from the relay, and checks the signature against two keys. Those are the same two keys as `updatePubKeysHex` in `main.go`. A missing or wrong signature stops the install before anything is installed. `HOTEND_ALLOW_UNVERIFIED_INSTALL=1` is the only way past it.
- on a machine with a user systemd session, appends a `[spoolman]` section to `~/printer_data/config/moonraker.conf` (backup at `moonraker.conf.hotend-bak`) and restarts Moonraker, trying `sudo systemctl` if the user session can't. `HOTEND_SKIP_SPOOLMAN=1` skips it. Busybox boards are not touched.
- writes `~/.config/systemd/user/hotend-agent.service` and turns on lingering so the agent survives logout. Busybox boards (the Creality K1 family and similar) get `/usr/data/hotend-agent` and `/etc/init.d/S99hotend_agent` instead. With no service manager it runs the agent in the background and says it will not survive a reboot.
- if the install fails, sends the error text and your claim token to the relay so the failure shows up in your account.

I ran the installer in a sandbox against a stand-in relay with a throwaway `HOME` and no systemd on `PATH`. A good download installed. A binary with one byte added was refused, and so was a missing signature. In both refusals no binary was installed and the failure report went to the stand-in. I did not run the systemd or busybox branches, and I did not run it against the real relay.

## Build and run it yourself

Install into a directory your user owns, because self-update replaces the binary in place:

```sh
make
install -Dm755 hotend-agent ~/.hotend-agent/hotend-agent
HOTEND_RELAY_URL=wss://app.hotend.app/ ~/.hotend-agent/hotend-agent
```

On first start it prints a pairing code to enter in the app under Add printer. For an unattended run, use the example unit in [`systemd/hotend-agent.service`](../systemd/hotend-agent.service). A root-owned binary in `/usr/local/bin` also works, with `HOTEND_AGENT_AUTO_UPDATE=off`, since the agent can't replace a file it doesn't own. Don't run it as root to get self-update working.

## Building

```sh
make            # host build
make dist       # linux amd64, armv7, arm64 and the verify-sig checker into ./dist
go test ./...
```

Builds are static (`CGO_ENABLED=0`), carry no local paths (`-trimpath`) and no VCS stamp (`-buildvcs=false`). That is what makes them reproducible. Any Go 1.22 or newer builds a working agent, but byte-identical output needs the official go.dev toolchain at the version pinned in the [reproducible workflow](../.github/workflows/reproducible.yml), which is go1.22.2. Distro packages that report the same version are patched and produce different bytes. A static build matters on its own: a dynamically linked one picks up the build machine's glibc and won't start on an older printer image.
