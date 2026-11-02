## What changed and why

Agent source here is a published copy of a private monorepo, so the maintainer ports code changes there instead of merging them here (see CONTRIBUTING.md). Docs, templates and `.github/` changes merge normally.

## How you checked it

- [ ] `go test ./...` passes
- [ ] A new or changed guard has a test that fails when the guard is weakened
- [ ] No printer IDs, pairing codes or secrets in the description or logs
