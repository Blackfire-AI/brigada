# Contributing

Notes for contributors and their coding agents.

## Build

    go build ./cmd/brigada

The Go version is in `go.mod`.

## Checks

CI runs these; run them before you push:

    test -z "$(gofmt -l .)"
    go run ./internal/tools/guard
    go vet ./...
    go test -count=1 ./...
    CGO_ENABLED=1 go test -race -count=1 ./...

`guard` is the repository's own scanner for private addresses and similar leaks.

Before a release, a person runs `bash scripts/release-check-mode-a.sh` (needs signed-in `claude` and `codex`; CI does not run it).

## Layout

`cmd/brigada` is the single binary: client, engine, runner, foreman and MCP bridge. Everything else is under `internal/`:

- `adapter`: one adapter per agent CLI (claude, codex, kimi, opencode, and a fake for tests)
- `api`: the engine's JSON HTTP API, on a Unix socket locally and TLS remotely
- `caps`: the concurrency-caps model
- `engine`: the mailbox, job creation from ready tasks, and the routine rules
- `foreman`: the foreman's fixed instructions and wake template
- `history`: the per-project timeline of what happened
- `lifecycle`: the engine's life as a process: lock, start on demand, idle exit
- `mcp`: the `brigada mcp` stdio bridge offering brigada's tools to the foreman
- `notify`: local notifications
- `planfile`: the plan file mirrored into the repo
- `projecturl`: canonical repository identity
- `prompt`: the fixed templates worker prompts are composed from
- `runner`: executes jobs for the engine
- `runnerstate`: the local single-runner protocol
- `store`: the engine's SQLite state
- `term`, `logo`: terminal styling and the logo
- `testsupport`: helpers shared by tests
- `tools`: repository tooling, such as `guard`
- `version`: the binary's version

For the design, read `docs/design.md`, `docs/product.md` and `docs/glossary.md`.

## Conventions

- Match the surrounding code: its comment density and naming.
- Put tests beside the code they test.
- Add no new dependency without a reason.

## Commits

Conventional Commits with a scope, as in `git log`: `fix(init): ...`, `feat(engine): ...`, `docs: ...`. The subject describes the change in the log's plain style.

## Review

Every change is reviewed by a model from a different family than its author's: `brigada review --branch <branch>`.

## Public repository

Never commit secrets, tokens, private hostnames or private IP addresses. `guard` enforces part of this.

## Files brigada manages

`brigada init` writes `.brigada/project.yaml`, `.mcp.json` and `.claude/skills/brigada-foreman/SKILL.md`. Do not edit them by hand.

<!-- brigada-managed: router v1; init added a blank line after byte 2579 -->
If your prompt opens with "You are a brigada worker" or
"You are a brigada reviewer", you are a worker or reviewer run by brigada:
follow that prompt; this block and the foreman rules do not apply to you.

Otherwise: in this repository, code changes go through brigada, and you are
its foreman. Read `.brigada/foreman.md` first and follow it, unless your
prompt already gives you brigada's foreman rules. Plan work with brigada's
tools; do not edit code yourself unless the user explicitly asks you to.
If brigada's tools are not available in this session, say so and tell the
user how to enable them (restart the session in this repository and approve
the `brigada` MCP server) instead of doing the work by hand.
<!-- /brigada-managed: router -->
