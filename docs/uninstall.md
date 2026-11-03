# Remove the agent

Pick the section that matches how it was installed. Afterwards, remove the printer in the Hotend app, or it stays listed as offline.

I ran the commands for the plain-process case in a sandbox. The systemd and busybox sections come from reading the installer as served in November 2026 and were not run.

## Plain process (no service manager)

```sh
kill $(pgrep -f "$HOME/.hotend-agent/hotend-agent")
rm -rf ~/.hotend-agent ~/.config/hotend
```

## systemd user service (the usual Raspberry Pi image)

```sh
systemctl --user disable --now hotend-agent
rm ~/.config/systemd/user/hotend-agent.service
systemctl --user daemon-reload
rm -rf ~/.hotend-agent ~/.config/hotend
```

The installer also ran `loginctl enable-linger`. If nothing else of yours needs the user session to outlive your logins, undo it with `loginctl disable-linger $USER`.

## Busybox boards (Creality K1 family and similar)

```sh
/etc/init.d/S99hotend_agent stop
rm /etc/init.d/S99hotend_agent
rm -rf /usr/data/hotend-agent /usr/data/hotend
```

## The Moonraker edit

If the installer added a `[spoolman]` section, it is the last block of `~/printer_data/config/moonraker.conf`:

```ini
[spoolman]
server: http://127.0.0.1:7913
sync_rate: 5
```

Delete those lines, or restore the copy at `moonraker.conf.hotend-bak` if you have made no other edits since. Then restart Moonraker. Left in place, Moonraker will keep trying to reach a port nothing listens on. The installer skips this edit on busybox boards and when Moonraker already had a `[spoolman]` section, so you may have nothing to undo.

Deleting `~/.config/hotend/agent.json` discards the printer's identity. Keep it if you plan to reinstall and keep the same printer in your account.
