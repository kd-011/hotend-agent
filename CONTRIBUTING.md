# Contributing

Bug reports and fixes are welcome. This repository works differently from most, so here is how they get handled.

## Where the source of truth is

The agent is developed in a private monorepo together with the Hotend relay and app. This repository is a published copy of the agent. On each release the agent source (`main.go`, `main_test.go`, `go.mod`, `go.sum`, `restart_*.go`, `verify-sig/`, `scripts/verify-agent-release.mjs`) is copied over this repository from the monorepo. Anything changed in those files here would be overwritten by the next publish.

So a pull request that edits agent code can't be merged as it stands. This is what happens instead:

1. You open a pull request or an issue describing the change.
2. The maintainer reads it. If the fix is worth having, they make the same change in the monorepo, with a test, and credit you in the commit message.
3. It reaches this repository in the next signed release. The pull request is then closed with a link to that release.

You can skip the pull request. An issue with the failing behaviour and a suggested fix is just as useful.

Documentation, templates, the issue and pull request forms and the workflows in `.github/` are edited here directly, and pull requests against those merge normally.

## Releases

The maintainer builds and signs releases offline. The signing keys never touch GitHub or the relay. After a release is published, the `reproducible` workflow rebuilds it from the tag on a GitHub runner and fails if the bytes differ from the published binaries. Nobody else can cut a release or move a tag, so please don't open pull requests that bump version strings.

## Building and testing

Requires Go 1.22 or newer.

```sh
make            # host build
go test ./...   # the release guards: signature, version and rollback checks
make test       # vet and test, as CI runs them
```

Keep the build static (`CGO_ENABLED=0`) and keep `-trimpath -buildvcs=false`. Without them the output isn't reproducible and can't be compared with a published release. For byte-identical results, use the official go.dev toolchain, as the README describes.

## Ground rules

- Keep dependencies at the standard library plus `gorilla/websocket`. A new dependency in code that runs on printers needs a good reason.
- A change to a guard (signature check, version check, rollback, local-only request targets) needs a test that fails when the guard is weakened.
- Plain commit messages and plain prose. No em dashes.
- By contributing you agree your change is under the MIT licence in `LICENSE`.

## Dependency updates and CI

Dependabot proposes GitHub Actions updates weekly, and a person reviews and merges them. It doesn't touch `go.mod`, which comes from the monorepo. Go dependency advisories still appear in the Security tab. CI vets, builds the three shipped architectures and runs the tests on every pull request and once a week.

A weekly job checks the links in the docs and compares the claims in `dossiers/hotend-agent.md` with the files they cite. If something is wrong it opens one issue, and it closes the issue when the next run is clean. It never opens pull requests, tags or releases.
