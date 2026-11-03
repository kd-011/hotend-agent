# Verify a release

Three checks, from quickest to strictest. I ran all three from a clean directory with a throwaway `HOME`.

## 1. Check what the relay serves

```sh
node scripts/verify-agent-release.mjs
```

It reads the trusted keys out of `main.go`, then downloads the version, the signed manifest and each architecture's binary and signature from `https://app.hotend.app/agent-bin`. It checks that the manifest verifies, that its version and sha256 values match what is served, and that each binary's signature verifies against one of the keys. It takes about a minute, mostly downloading three 5 to 6 MB binaries. Pass a base URL as the first argument to check another server. Node 18 or newer is enough, and it needs no packages.

Reading the output:

- `signature verifies` and `bytes match signed manifest hash` are the lines that matter for the served binaries. They say the bytes were signed by a key in this source.
- `main.go says X, relay advertises Y` fails whenever this tree and the fleet are on different versions. Releases are mirrored here by hand, so that happens. It is a version gap and says nothing about signatures. The script's text says `agent-go/main.go` because the same script lives in the private monorepo.
- `--local` also compares the served bytes with a `hotend-agent-linux-<arch>` file built in the repo root. It can only pass when your tree is at the served version.
- `rate limited` means the relay capped downloads from your IP for an hour. Run it later.

## 2. Rebuild a tagged release and compare bytes

Distro Go packages, Ubuntu's `golang-1.22` included, produce different bytes from the official toolchain, so use the tarball from go.dev. The version below is the one the [reproducible workflow](../.github/workflows/reproducible.yml) pins. Replace the tag with the release you want to check.

```sh
tag=v2026.08.02.1
curl -fsSLO https://go.dev/dl/go1.22.2.linux-amd64.tar.gz
echo "5901c52b7a78002aeff14a21f93e0f064f74ce1360fce51c6ee68cd471216a17  go1.22.2.linux-amd64.tar.gz" | sha256sum -c -
mkdir toolchain && tar -C toolchain -xzf go1.22.2.linux-amd64.tar.gz
export PATH="$PWD/toolchain/go/bin:$PATH"

git clone --branch "$tag" https://github.com/kd-011/hotend-agent.git src
mkdir published
for f in hotend-agent-linux-amd64 hotend-agent-linux-arm64 hotend-agent-linux-arm; do
  curl -fsSL -o "published/$f" "https://github.com/kd-011/hotend-agent/releases/download/$tag/$f"
done
(cd src && make dist)
sha256sum src/dist/hotend-agent-linux-* published/hotend-agent-linux-*
```

Each architecture should show the same hash twice. That checksum is also listed for that file on go.dev/dl. For `v2026.08.02.1` all three architectures matched and the `verify-sig` build matched the published one too. The newest tags may not have release assets, so look at the [releases page](https://github.com/kd-011/hotend-agent/releases) and the `reproducible` runs first.

## 3. Check a signature by hand

[`verify-sig`](../verify-sig/main.go) is the same check the installer uses. Run it under bash, because it needs the key list split into separate arguments.

```sh
keys=$(grep -oE '"[0-9a-f]{64}"' src/main.go | tr -d '"' | tr '\n' ' ')
for f in hotend-agent-linux-amd64 hotend-agent-linux-arm64 hotend-agent-linux-arm; do
  curl -fsSL -o "published/$f.sig" "https://github.com/kd-011/hotend-agent/releases/download/$tag/$f.sig"
  src/dist/hotend-verify-sig-linux-amd64 "published/$f" "published/$f.sig" $keys && echo "$f ok"
done
```

Exit status 0 means the signature verifies against one of the keys. Adding a byte to a binary makes it exit 1.

## What the checks do not cover

- Signatures prove the binary was signed by the holder of a key listed in `main.go`. They do not prove the key holder is trustworthy, or that the key is safe. [SECURITY.md](../SECURITY.md) describes how the keys are held.
- A byte match proves a release was built from the tagged source. It does not prove the source is free of bugs. You still have to read it.
- The installer script is not in this repository, so nothing here proves what it will do tomorrow. Read it before you run it.
