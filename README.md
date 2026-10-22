<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/logo/lockup-on-dark.svg">
    <img src="docs/brand/logo/lockup-on-light.svg" alt="brigada by Blackfire AI" width="300">
  </picture>
</h1>

**Talk to the foreman. It splits the work into tasks; brigada gives each task to the agent with the best measured record on your repo among those with quota left, checks and cross-reviews the result, and learns from the outcome.**

You may pay for more than one AI coding subscription. brigada sits above their official command-line tools (Claude Code, Codex, Kimi, opencode; experimental: dsh) and uses them as workers:

- the **foreman**, one of those CLIs, turns what you ask for into a plan of tasks with dependencies between them. It lives in a brigada-owned session: you talk to it with `brigada attach`, and it acts headless while you are away;
- the **dispatcher** gives each task to the agent with the best measured record for that kind of task on *your* repo, among the agents that still have quota;
- every task's own **check** (tests, lint, any command) runs on the result, so "done" is not taken on trust;
- a **reviewer** from a different model family than the author reads the change;
- the **outcome** of every task is recorded locally, and `brigada stats` shows which agent actually wins where;
- **landing is brigada's**: each approved task is squashed onto the target branch, your check reruns on the result, and the push follows your push gate (`auto` by default); `brigada land` stays as the hand-driven form, and gates hold anything you want to see first.

brigada drives the vendors' **unmodified** CLIs, each signed in with your own account. It never rotates or pools credentials. See [docs/TERMS.md](docs/TERMS.md).

> **Before you start.** brigada is local-first: one Go binary and a SQLite store on your machine. Workers run in git worktrees, not containers, and a worker can do what your user can do. Checks run with your credentials: do not point brigada at a repository you do not trust.

## Install

You need:

- Go 1.27 or newer and `git`;
- a repository with an `origin` remote;
- signed-in agent CLIs on `PATH`. Reviews need a model family other than the author's, so offer review-capable agents from at least two families.

```sh
go install github.com/Blackfire-AI/brigada/cmd/brigada@latest
brigada version
```

The main package lives in `cmd/brigada`, so that is the install path. Make sure `$(go env GOPATH)/bin` is on your `PATH`. Once a release is published, prebuilt binaries are attached to its tag.

## First run: `brigada init`

Sign in to each CLI with its own login flow, then run this inside your repository:

```sh
brigada init
```

Init walks you through four steps:

1. Pick the foreman model from the signed-in Claude and Codex CLIs, then how hard it should think. `--foreman claude/sonnet@medium` supplies both.
2. Choose which CLIs' models may run tasks. Opencode offers a menu with multiple picks. Reviews need two model families; Kimi reviews only unless you let it write code.
3. Set tasks at once on this machine (or pass `--parallel N`). Enter keeps the saved limit, else half the cores, at least 1 and at most 4.
4. Confirm the check suggested by your build files, type your own command, or enter `-` for none. Init takes worker tools from that choice and never runs the check.

Init then lists the agents, permissions and files, and shows a diff for every existing file it would change (`--verbose` also shows the contents of new files). Nothing is written or started until you confirm.

After you confirm, init:

- writes `.brigada/project.yaml`, which is yours to commit (brigada never commits or pushes it), and strips the wiring an older init left in the repository;
- starts the engine and a detached runner.

`brigada init --probe` shows the plan and writes nothing. Stop the runner with `brigada stop`; add `--engine` to stop the engine too. Every step and flag is in [the guide, section 1](docs/GUIDE.md#1-install-and-brigada-init).

Rerun `brigada init` to change agents, the foreman model and effort, or tasks at once; use `brigada start` after a reboot and `brigada config` to see every setting and how to change it.

`brigada remove` removes brigada's repository wiring and archives the project, keeping its history; see [the guide](docs/GUIDE.md#removing-brigada-from-a-repository).

## Talk to the foreman

After init, run `brigada attach` inside the repository. The foreman is an agent in a brigada-owned session: your attached chat and the headless turns that wake on events while you are away are one transcript, running in a reading checkout of your repository with your own harness config. It plans through brigada's tools (`--model` and `--effort` override the project's foreman choice for one chat); the runner works each task on its own branch in a worktree, runs the check and pushes the branch; and a reviewer of another family reads it. See [section 2](docs/GUIDE.md#2-talking-to-the-foreman-brigada-attach).

Then:

```sh
brigada status              # engine, jobs, plans, what needs you
brigada diff <task-id>      # what a task changed
brigada land <task-id>...   # land approved tasks by hand (the land job does it by rule)
```

The [guide](docs/GUIDE.md) covers the rest: a coordinator session over several repositories, watching work as it runs, configuration, and the full command reference.

`brigada help` lists the commands. `brigada land --help` (or `brigada help land`) prints a command's usage and flags, and `brigada project --help` lists its subcommands. Each subcommand has help too, for example `brigada project foreman --help`.

## No telemetry

brigada sends no usage data, analytics or crash reports anywhere. That is a promise about brigada, not about the CLIs it drives:

- each official CLI sends your prompts and the code it reads to its own model provider, as it does when you run it by hand, under your account and that vendor's terms;
- the runner pushes task branches to your repository's git remote with your git credentials;
- the engine mirrors plan files (titles, task states, dependencies, branches, open questions, extra repositories) to the `brigada/plans` branch of that remote; `brigada project set plan_files=off` turns that off.

brigada has no API-key setting of its own. Workers inherit your environment (minus brigada's own variables), so a CLI that you have set up with an API key the way its vendor documents is driven the same way. See [docs/TERMS.md](docs/TERMS.md).

## Releasing

Maintainers tag a version and push the tag:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The `release` workflow builds `brigada-<os>-<arch>` binaries (darwin and linux, amd64 and arm64) with the tag as the version, plus a `.tar.gz` of each and `SHA256SUMS`, and attaches them to a new GitHub release. `brigada version` prints the tag without its leading `v`; a plain `go build` prints `0.0.0-dev`.

## License

Apache-2.0. See [LICENSE](LICENSE).

What 1.0 promises until 2.0: [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).

Changes: [CHANGELOG.md](CHANGELOG.md).
