# brigada

**Talk to a foreman. It splits the work into tasks, gives each one to the agent with the best measured record on your repo among those with quota left, checks and cross-reviews the result, and learns from the outcome.**

You may pay for more than one AI coding subscription. brigada sits above their official command-line tools (Claude Code, Codex, Kimi, opencode) and uses them as workers:

- a **foreman**, itself one of those CLIs, turns what you ask for into a plan of tasks, with dependencies between them;
- the **dispatcher** gives each task to the agent with the best measured record for that kind of task on *your* repo, among the agents that still have quota;
- every task's own **check** (tests, lint, any command) runs on the result, so "done" is not taken on trust;
- a **reviewer** from a different model family than the author reads the change;
- the **outcome** of every task is recorded locally, and `brigada stats` shows which agent actually wins where.

brigada drives the vendors' **unmodified** CLIs, each signed in with your own account. It never rotates or pools credentials. See [docs/TERMS.md](docs/TERMS.md).

## Status

**Alpha.** brigada is local-first: one Go binary and a SQLite store on your machine. Expect rough edges and changing behaviour.

What works today:

- **Plans with dependencies.** The foreman submits a plan of tasks; a task starts when the tasks it depends on are done.
- **An approval limit.** A plan with many tasks, an unusual task class or a protected path waits for you: `brigada plan approve <id>` or `brigada plan reject <id>`. Nothing is queued until you approve.
- **Checks.** Each task carries a check command that the runner executes after the worker finishes.
- **Cross-family review.** A change is reviewed by an agent of another family than its author. `brigada review --branch <name>` reviews a branch on its own, without a plan.
- **The outcome record and stats.** `brigada stats` prints attempts, successes, rate and confidence per agent and task class, per repo. On a fresh install there is little data, and the confidence column says so.
- **Retry.** A worker CLI failure (quota, authentication, a lost runner) gets one automatic restart on another ready agent, with a fresh session and branch. Set `task_auto_retry = false` in the config file to park such failures for the foreman instead.
- **Snapshots and recover.** Set `snapshot_dir` in the config file (an absolute directory outside the state directory) and the engine snapshots its store daily, keeping the last seven. With the engine stopped, `brigada recover` restores the newest snapshot into a fresh store and rebuilds newer plans from their plan files, mirrored to `.brigada/plans/<plan-id>.yaml` on the `brigada/plans` branch. Without a snapshot, messages, transcripts and outcomes are not restored.
- **Adapters** for the `claude`, `kimi`, `codex` and `opencode` CLIs as workers. The foreman can run on `claude` or `codex`.

Local mode and its limits:

- Workers run in **git worktrees, not containers**. A worker can do what your user can do.
- **Checks run with your credentials.** A check is a command on your machine, and so is whatever the repo makes it run. Do not point brigada at a repo you do not trust.
- Plan state, transcripts and the outcome record live in the state directory on your machine.

**No telemetry.** brigada sends no usage data, analytics or crash reports anywhere. That is a promise about brigada, not about the CLIs it drives: each official CLI sends your prompts and the code it reads to its own model provider, as it does when you run it by hand, under your account and that vendor's terms. The runner also pushes task branches to your repo's git remote with your git credentials, and the engine mirrors plan files (titles, task states, dependencies, open questions) to the `brigada/plans` branch of that remote.

## Quick start

You need Go 1.27 or newer, `git`, a repository with an `origin` remote, and the CLIs for the agents you will use installed and signed in. The runner example in step 5 offers two families, `claude` and `codex`, so it needs both. A change is reviewed by an agent of a different family than its author, so with a single family the review cannot complete; keep at least two families among your agents (for example `claude` and `kimi`, or `claude` and `opencode`) and edit the `--agent` list to match what you have.

**1. Install.**

```
go install github.com/Blackfire-AI/brigada/cmd/brigada@latest
brigada version
```

The main package lives in `cmd/brigada`, so that is the install path. Make sure `$(go env GOPATH)/bin` is on your `PATH`. Prebuilt binaries are not published yet.

**2. Configure the engine (optional).** The engine runs with defaults and needs no config file. To change them, write `~/.config/brigada/config` (or `$XDG_CONFIG_HOME/brigada/config`), one `key = value` per line, `#` for comments:

```
# desktop notifications use the OS tool when found (macOS osascript, Linux
# notify-send); that is the default. Uncomment to turn them off:
# notify = none
# daily snapshots of the store, last seven kept; an absolute directory
snapshot_dir = /home/you/backups/brigada
```

Other keys: `state_dir`, `idle_minutes`, `headroom`, `task_auto_retry`, `default_reviewer`, `caps.*` and `cost.<harness>/<model>`. An unknown key is an error. State lives in `~/.local/state/brigada` (or `$XDG_STATE_HOME/brigada`); `--state-dir` and `--config` override both; they go after the command, for example `brigada status --state-dir DIR`.

**3. Log in to the CLIs you already pay for.** Use each CLI's own sign-in flow, as you normally would, so that it works by itself in a terminal. brigada never asks for or stores your credentials.

**4. Name the foreman.** From your repo, choose which CLI and model plays the foreman for it (the repo URL is the one `git remote get-url origin` prints):

```
brigada project foreman --repo <origin-url> --harness claude --model sonnet
```

The harness must be `claude` or `codex`. Run the command with `--repo` alone to show the stored choice.

**5. Start a runner.** A runner offers your signed-in CLIs as agents and runs the jobs the engine hands out. Leave it running in its own terminal:

```
brigada runner --agent harness=claude --agent harness=codex
```

Each `--agent` is `harness=<h>[,model=<m>][,classes=<c1>+<c2>][,account=<label>]`, at most one per harness. The engine starts on demand, and stops after 30 idle minutes by default. Check on it any time with `brigada status`.

**6. Ask.** From inside the repo, chat with the foreman:

```
brigada
```

or send one message without attaching, and name the check you trust:

```
brigada ask "fix the flaky auth test; the check is: go test ./auth/..."
```

(`brigada ask` takes `--repo <origin-url>` to pick a project when the engine knows several, and `-` to read the message from stdin.)

The foreman makes a plan, the runner works each task on its own branch in a worktree, runs the check and pushes the branch to your remote, and a reviewer of another family reads it. Follow along and look at results:

```
brigada status        # engine, jobs, plans, what needs you
brigada history       # the timeline
brigada stats         # which agent wins which class on this repo
brigada plan approve <id>   # or: brigada plan reject <id>
brigada cancel <job-id>
```

`brigada help` lists every command.

Day two and beyond: [docs/GUIDE.md](docs/GUIDE.md).

## API keys

brigada has no API-key setting of its own yet. Workers inherit your environment (minus brigada's own variables), so a CLI that you have set up with an API key the way its vendor documents is driven the same way. See [docs/TERMS.md](docs/TERMS.md).

## Releasing

Maintainers tag a version and push the tag:

```
git tag v0.1.0
git push origin v0.1.0
```

The `release` workflow builds `brigada-<os>-<arch>` binaries (darwin and linux, amd64 and arm64) with the tag as the version, plus a `.tar.gz` of each and `SHA256SUMS`, and attaches them to a new GitHub release. Users without Go download the binary for their platform from the releases page; with Go, `go install github.com/Blackfire-AI/brigada/cmd/brigada@latest` works too. `brigada version` prints the tag without its leading `v`; a plain `go build` prints `0.0.0-dev`.

## License

Apache-2.0. See [LICENSE](LICENSE).
