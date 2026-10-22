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
    (cd internal/tools/licenses && go test -count=1 .)

`guard` is the repository's own scanner for private addresses and similar leaks.

`THIRD_PARTY_NOTICES.md` comes from the nested module in `internal/tools/licenses` (its own `go.mod`), which keeps go-licenses out of the main `go.mod` and makes the notices reproducible. Do not fold it into the main module. The generator queries go-licenses once per platform the release workflow builds and unions the results, so the file is identical whatever host regenerates it. Locally, `TestThirdPartyNoticesFresh` skips when the generator cannot run; CI sets `CI=true` and fails instead. To regenerate after a dependency change:

    cd internal/tools/licenses
    go test -count=1 -run TestThirdPartyNoticesGolden -update .

## Layout

`cmd/brigada` is the single binary: client, engine, runner, foreman and MCP bridge. Everything else is under `internal/`:

- `adapter`: one adapter per agent CLI (claude, codex, kimi, opencode, dsh, and a fake for tests)
- `api`: the engine's JSON HTTP API, on a Unix socket (remote TLS is backlog)
- `caps`: the concurrency-caps model
- `engine`: the mailbox, job creation from ready tasks, and the routine rules
- `foreman`: the foreman's fixed instructions and wake template
- `history`: the per-project timeline of what happened
- `landing`: the git side of landing an approved task (§7.1)
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

The docs under `docs/` describe the system as it is on main, in the present tense. A change that alters behaviour updates the section it touches in the same commit. No dates, decision or status markers, issue numbers or history: git log, CHANGELOG.md and docs/COMPATIBILITY.md keep those.

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

`brigada init` writes `.brigada/project.yaml`; do not edit it by hand.

