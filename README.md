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

Other keys: `state_dir`, `idle_minutes`, `headroom`, `task_auto_retry`, `default_reviewer`, `caps.*`, `cost.<harness>/<model>` and `tools.claude/<model>`. An unknown key is an error. State lives in `~/.local/state/brigada` (or `$XDG_STATE_HOME/brigada`); `--state-dir` and `--config` override both; they go after the command, for example `brigada status --state-dir DIR`.

**3. Log in to the CLIs you already pay for.** Use each CLI's own sign-in flow, as you normally would, so that it works by itself in a terminal. brigada never asks for or stores your credentials.

**4. Name the foreman.** From your repo, choose which CLI and model plays the foreman for it (the repo URL is the one `git remote get-url origin` prints):

```
brigada project foreman --repo <origin-url> --harness claude --model sonnet
```

The harness must be `claude` or `codex`; add `--effort <level>` to choose how hard the foreman reasons (docs/GUIDE.md, section 10). Run the command with `--repo` alone to show the stored choice.

**5. Start a runner.** A runner offers your signed-in CLIs as agents and runs the jobs the engine hands out. Leave it running in its own terminal:

```
brigada runner --agent harness=claude --agent harness=codex
```

Each `--agent` is `harness=<h>[,model=<m>][,classes=<c1>+<c2>][,account=<label>][,effort=<level>][,tools=<p1>+<p2>]`, at most one per harness. By default claude workers run with a Go-only shell scope (`go build`, `go test`, `go vet`, `gofmt`), so for a Node, Python or Rust repo widen it with `tools=`, for example `--agent 'harness=claude,tools=Bash(npm test:*)+Bash(npx jest:*)'` (see the guide, section 5). The engine starts on demand, and stops after 30 idle minutes by default. Check on it any time with `brigada status`.

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

Day two and beyond: [docs/GUIDE.md](docs/GUIDE.md). Working in several repositories (one project and foreman per repo, `brigada project list`): [Several repositories](docs/GUIDE.md#several-repositories).

## How it fits your day

Each repo has one long-lived **foreman** session, a `claude` or `codex` CLI that holds brigada's tools. You do not start it per task; you come and go from it. This is fullest with a `claude` foreman. An attached `codex` session is Codex's own terminal interface without brigada's tools, and a later headless turn may start a fresh Codex thread instead of continuing your chat; see [the guide, section 10](docs/GUIDE.md#10-foreman-on-claude-or-codex).

- **Attach for a conversation.** `brigada` inside the repo puts you in the foreman's own session: think out loud, review a plan, answer its questions. Leave when you are done. With `claude`, the next headless turn continues the same session.
- **Ask for a message.** `brigada ask "..."` drops a request in the foreman's inbox and returns at once, without a conversation.
- **Status for the queue.** `brigada status` shows the jobs, the plans and the **needs you** queue: plans waiting for your approval, tasks parked for judgment, a limited foreman.
- **While you are away,** work continues headless. The runner works each task on its own branch, runs its check and pushes it; a reviewer of another family reads the change. The foreman wakes on events (a plan done, a task parked, a question), acts on them, and leaves a report. While you are attached it does not start headless turns, and what piled up reaches it when you attach.
- **At the end, you merge.** Each task's work is a branch on your remote. brigada does not merge anything; you read the branches and merge the ones you want.
- **The records** live on your machine, in the state directory (`~/.local/state/brigada` by default): the store, transcripts, check logs and the outcome record behind `brigada history` and `brigada stats`. Plan files are mirrored to the `brigada/plans` branch of your remote.

Each repo has its own foreman. More in [the guide](docs/GUIDE.md#2-daily-use).

## From your own harness

`brigada mcp --client` is an MCP server over stdio. Register it in another coding harness, and that harness can talk to the foreman and watch the work without you relaying between them. `brigada` must be on the `PATH` of the harness that launches it (or use the absolute path of the executable in the commands below).

**Claude Code** (the `--` separates Claude Code's options from the brigada command; `--scope user` makes the server available in all your Claude Code projects, where the default `local` scope is limited to the current project):

```
claude mcp add --scope user --transport stdio brigada -- brigada mcp --client
```

**Codex:**

```
codex mcp add brigada -- brigada mcp --client
```

These forms follow `claude mcp add --help` and `codex mcp add --help` as of 2026-10-16. A non-default state directory or config file goes after `mcp`, in the command that is launched: `brigada mcp --client --state-dir /absolute/path`. Put no token in the command; the bridge reads the client token from the state directory.

It offers three tools:

- `ask_foreman`: send the foreman a message as you. It takes `text` and `repo_url`, the project's repo URL (the one `git remote get-url origin` prints). `repo_url` may be left out only when the engine knows a single project; with several repos, always pass it, for example `{"text": "fix the flaky auth test; the check is: go test ./auth/...", "repo_url": "git@github.com:you/app.git"}`. With nobody attached, the foreman wakes up and acts on it; with a chat attached, the message waits for the foreman's next read. The answer says which happened. The same text again within a day counts as a retry of the first call, so reword it to send it anew.
- `status`: the engine's state across every project, as in `brigada status`.
- `history`: the timeline, optionally one `task`, at most `limit` items (1 to 500, default 100).

The engine starts on demand, as for any brigada command. The other harness acts **for you**, with the client token: it can send your message and read status and history. It is never the foreman and cannot do what the foreman does. A runner must still be running for the work to happen.

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
