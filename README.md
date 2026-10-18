# brigada

**Talk to your own coding session. It splits the work into tasks, brigada gives each one to the agent with the best measured record on your repo among those with quota left, checks and cross-reviews the result, and learns from the outcome.**

You may pay for more than one AI coding subscription. brigada sits above their official command-line tools (Claude Code, Codex, Kimi, opencode) and uses them as workers:

- a **foreman**, a Claude or Codex session, turns what you ask for into a plan of tasks, with dependencies between them. In the usual setup the foreman is the session you already have open in your repository;
- the **dispatcher** gives each task to the agent with the best measured record for that kind of task on *your* repo, among the agents that still have quota;
- every task's own **check** (tests, lint, any command) runs on the result, so "done" is not taken on trust;
- a **reviewer** from a different model family than the author reads the change;
- the **outcome** of every task is recorded locally, and `brigada stats` shows which agent actually wins where;
- **you** land and push: `brigada land` squashes approved work onto your branch locally and stops before any push.

brigada drives the vendors' **unmodified** CLIs, each signed in with your own account. It never rotates or pools credentials. See [docs/TERMS.md](docs/TERMS.md).

**Alpha.** brigada is local-first: one Go binary and a SQLite store on your machine. Expect rough edges and changing behaviour. Workers run in git worktrees, not containers, and a worker can do what your user can do. Checks run with your credentials: do not point brigada at a repository you do not trust.

## Install

You need Go 1.27 or newer, `git`, a repository with an `origin` remote, and signed-in agent CLIs on `PATH`. Reviews need another model family than the author's, so offer review-capable agents from at least two families.

```
go install github.com/Blackfire-AI/brigada/cmd/brigada@latest
brigada version
```

The main package lives in `cmd/brigada`, so that is the install path. Make sure `$(go env GOPATH)/bin` is on your `PATH`. Prebuilt binaries are attached to tagged releases when a release has been published.

## First run: `brigada init`

Sign in to each CLI with its own login flow, then run this inside your repository:

```
brigada init
```

Init walks through three steps: choose models from menus for installed CLIs (confirming sign-in when it cannot be checked), choose a Claude or Codex foreman when both are usable, and choose a default check suggested from your build files (it never runs the check). Kimi reviews only unless you opt into writing code; explicit `--agent` specs retain the runner's class defaults. Init requires two review-capable model families unless you pass `--accept-limited`. It lists the agents, permissions and files, shows diffs for existing files (`--verbose` also shows new file contents), and writes nothing until you confirm. After you confirm it writes `.brigada/project.yaml` (yours to commit; brigada never commits or pushes it), wires Claude Code for your own session, prints the command to run for Codex, starts the engine and starts a detached runner. With agents supplied through `--agent`, `brigada init --probe` shows the plan without prompts or writes; use `--signed-in` for authentication that probes cannot confirm. Stop the runner with `brigada stop`; add `--engine` to stop the engine too. The details and every flag are in [the guide, section 1](docs/GUIDE.md#1-install-and-brigada-init).

## Your own session is the foreman

After init, open your harness in the repository and talk to it (mode A). It plans through brigada's tools, the runner works each task on its own branch in a worktree, runs the check and pushes the branch, and a reviewer of another family reads it. Only one interactive session holds the foreman at a time, and your session lets go of it at the end of each turn so that work finishing while you are away can wake a separate, brigada-owned headless foreman session. Decisions are handed over through short notes recorded in the engine, not through your chat. See [section 2](docs/GUIDE.md#2-your-own-session-is-the-foreman-mode-a).

Then:

```
brigada status              # engine, jobs, plans, what needs you
brigada diff <task-id>      # what a task changed
brigada land <task-id>...   # squash approved tasks onto this branch; never pushes
```

For several repositories (a coordinator session in the parent folder), the watching commands, configuration and the command reference, read the [guide](docs/GUIDE.md).

`brigada help` lists commands. Use `brigada land --help` (or `brigada help land`) for a command's usage and flags, and `brigada project --help` for its subcommands. Each subcommand has help too, for example `brigada project foreman --help`.

## No telemetry

brigada sends no usage data, analytics or crash reports anywhere. That is a promise about brigada, not about the CLIs it drives: each official CLI sends your prompts and the code it reads to its own model provider, as it does when you run it by hand, under your account and that vendor's terms. The runner also pushes task branches to your repository's git remote with your git credentials, and the engine mirrors plan files (titles, task states, dependencies, open questions) to the `brigada/plans` branch of that remote.

brigada has no API-key setting of its own. Workers inherit your environment (minus brigada's own variables), so a CLI that you have set up with an API key the way its vendor documents is driven the same way. See [docs/TERMS.md](docs/TERMS.md).

## Releasing

Maintainers tag a version and push the tag:

```
git tag v0.1.0
git push origin v0.1.0
```

The `release` workflow builds `brigada-<os>-<arch>` binaries (darwin and linux, amd64 and arm64) with the tag as the version, plus a `.tar.gz` of each and `SHA256SUMS`, and attaches them to a new GitHub release. `brigada version` prints the tag without its leading `v`; a plain `go build` prints `0.0.0-dev`.

## License

Apache-2.0. See [LICENSE](LICENSE).
