# Contributing

Thanks for helping with brigada. Coding agents working on this repository
should also read [AGENTS.md](AGENTS.md), which holds the agent-specific notes.

## Setup

You need Go at the version in `go.mod`, and `git`.

    git clone https://github.com/Blackfire-AI/brigada
    cd brigada
    go build ./cmd/brigada

To only check that it compiles, run `go build -o /dev/null ./cmd/brigada`.

Read these before you change behaviour:

- `docs/design.md`: how brigada works inside;
- `docs/product.md`: what it is for;
- `docs/glossary.md`: the terms used in the code and docs;
- `docs/GUIDE.md`: how to use it.

## Checks

CI runs these. Run them before you push:

    test -z "$(gofmt -l .)"
    go run ./internal/tools/guard
    go vet ./...
    go test -count=1 ./...
    CGO_ENABLED=1 go test -race -count=1 ./...

`guard` scans for private addresses and similar leaks. Never commit
secrets, tokens, private hostnames or private IP addresses.

## Pull requests

- For anything non-trivial, open an issue first and agree on the approach.
- Keep one change per PR.
- Put tests beside the code they test.
- Update the docs in the same PR when behaviour changes.
- Add no new dependency without a reason. Say why in the PR.

## Review

Every change is reviewed by a model from a different family than its
author's, with `brigada review --branch <branch>`. A maintainer also
reviews every change before it merges.

## Commits

Use Conventional Commits with a scope, as in `git log`:
`fix(init): ...`, `feat(engine): ...`, `docs: ...`. The subject says what
the change does, in plain words. Reference the issue in the body with
`Fixes #N`.

## License

brigada is licensed under the Apache License 2.0. Under section 5 of the
license, any contribution you submit for inclusion is under the same
license. You do not need to sign a CLA, and no DCO sign-off is required.

## Security

Do not report security issues in public issues. See
[SECURITY.md](SECURITY.md) for how to report them.
