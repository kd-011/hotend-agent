# Self-update

The agent asks the relay for a newer build when it starts and then about every 6 hours, with up to an hour of random delay so a whole fleet does not arrive at once. It accepts an update only when all of this holds:

- The binary is signed. Releases are signed offline with an ed25519 key. The public keys are the list `updatePubKeysHex` in `main.go`, and the agent checks the signature over the exact bytes it downloaded. A signed manifest also ties the version string to each architecture's sha256, so a compromised relay cannot pair an old signed binary with a new version number. The signing key never touches the server that distributes binaries. There are two keys so one can be rotated without stranding printers.
- It runs and reports its version. The new binary has to start with `-version` and print the version it claims. This happens after the signature and manifest checks, so the agent never executes bytes that have not verified. The reported version is compared with the signed manifest.
- The swap is a rename, followed by an exec of the new binary in place. It does not depend on a service manager, and the printer connection drops only for the moment of the exec.
- It can be undone. The replaced binary is kept. If the new one cannot reach the relay within 15 minutes or 3 restarts, the old one goes back and that version is never installed again on this printer.
- It only moves forward. A version that is not strictly newer is ignored, so nobody can push you back to an older build. A bad release is fixed by publishing a higher version.

There is one fallback in the code to know about. If the relay has no signed manifest for a version (a 404), the agent falls back to the per-binary signature alone and prints a line saying so. A manifest that is present but wrong is refused.

## Turning it off

Any of these stops updates, and cloud access keeps working:

- Set `HOTEND_AGENT_AUTO_UPDATE=off`. This is a local switch the relay cannot override. With it set, the agent does not even make the version request.
- Block the relay's `/agent-bin/` path at your firewall.
- Build from source and install by hand.

A root-owned binary in `/usr/local/bin` cannot be updated by the agent, since it does not own the file. Set `HOTEND_AGENT_AUTO_UPDATE=off` there and update by reinstalling. Do not run the agent as root to get around that. It needs no privileges.
