# brigada: user guide

This guide follows how an engineer uses brigada: install and run `brigada init` in a repository, talk to the foreman with `brigada attach`, add a coordinator over several repositories, watch the work, land it, and configure the project. The [README](../README.md) is the short version. For the terms of use, see [TERMS.md](TERMS.md).

Contents:

1. [Install and `brigada init`](#1-install-and-brigada-init)
2. [Talking to the foreman: `brigada attach`](#2-talking-to-the-foreman-brigada-attach)
3. [Several repositories](#3-several-repositories)
4. [While work runs](#4-while-work-runs)
5. [Finishing: the land job](#5-finishing-the-land-job)
6. [Configuring a project](#6-configuring-a-project)
7. [Plans, checks and reviews](#7-plans-checks-and-reviews)
8. [Agents, quota and health](#8-agents-quota-and-health)
9. [The record and stats](#9-the-record-and-stats)
10. [Backups and recovery](#10-backups-and-recovery)
11. [The foreman's harness: claude or codex](#11-the-foremans-harness-claude-or-codex)
12. [From another harness](#12-from-another-harness)
13. [Configuration reference](#13-configuration-reference)
14. [Command reference](#14-command-reference)
15. [Known limits](#15-known-limits)

## 1. Install and `brigada init`

You need Go 1.27 or newer, `git`, a repository with an `origin` remote, and signed-in agent CLIs on `PATH`.

```sh
go install github.com/Blackfire-AI/brigada/cmd/brigada@latest
brigada version
```

Make sure `$(go env GOPATH)/bin` is on your `PATH`.

### The parts

Four parts, one binary:

- The **foreman** is an agent session (a `claude` or `codex` CLI) that has brigada's tools. It turns your asks into plans and decides what to do when a task needs judgment. You talk to it with `brigada attach` ([section 2](#2-talking-to-the-foreman-brigada-attach)); while nobody is attached, the engine wakes it headless.
- The **engine** holds the store (one SQLite file), the rules and the dispatcher, and serves everything on one Unix socket. It runs no model. It starts on demand when any command needs it and exits after 30 idle minutes (no live jobs, no attached chat, no open requests).
- The **runner** is the process you leave running. It offers your signed-in CLIs as agents, leases jobs from the engine, runs each worker in a git worktree, runs the task's check, and pushes the task's branch.
- The **repo remote** is where the branches land, along with the plan files on the `brigada/plans` branch.

### What init does

Run `brigada init` inside a repository. (At the top of a folder that holds several repositories, init does something else: see [section 3](#3-several-repositories).) The repository must have an `origin` remote, and all its URLs must name one repository. Init prints the canonical repository identity, then walks you through four steps. Nothing is written or started until you confirm the plan at the end.

Before step 1, init checks the installed CLIs in order (`claude`, `codex`, `kimi`, `opencode`, and [`dsh`](#dsh-experimental)) and prints one `Signed in:` line naming the confirmed ones, with the probe's detail in parentheses where there is one (opencode: its provider count and the command that counted them), then one line for each installed CLI whose sign-in it cannot check, such as `kimi: init can't check sign-in.`. CLIs that are not installed are not named. With `--yes` or `--probe` the same facts print as the `Agent CLIs found:` list.

- For Claude and Codex, init runs `claude auth status` and `codex login status` if their installed help documents them. If neither the probe nor `--signed-in` confirms authentication, it asks `Are you signed in to claude? [y/N]` before step 1. Answering no leaves that CLI out, models and all.
- Kimi has no verified sign-in check. Init tells you to run `kimi login` first if needed, then asks `Use kimi? [y/N]:` in step 2.
- For opencode, init runs `opencode providers list` if its help documents `providers`, or `opencode auth list` on older versions. Stored provider credentials confirm sign-in. If sign-in remains unconfirmed, it asks `Use opencode? [y/N]:`. When the check is unavailable, it tells you to run `opencode auth login` first if needed. Keys may also come from your environment.
- dsh is installed when `dsh` is on `PATH`; init reads only `dsh --version` and makes no model call. It cannot check sign-in, so it asks `Use dsh? [y/N]:` in step 2 and offers `deepseek-flash` and `deepseek-v4-pro`. When `DEEPSEEK_API_KEY` is not set, a one-line note says dsh needs it exported before `brigada start`, or stored through dsh's own credential store; the note never blocks init and the key is never printed.
- `--signed-in` confirms authentication without a question (for Kimi, opencode and dsh, that also answers the use question). With a distinct `account=LABEL` on `--agent`, a sign-in or use question names that account. Status output is read in memory, never printed or stored. Init never runs a login command or a model.

1. **The foreman.** The foreman plans the work and decides what happens when a task needs judgment. It runs whenever work finishes, so it uses that agent's quota. Only Claude and Codex can be the foreman ([section 11](#11-the-foremans-harness-claude-or-codex) states why). Init asks `Which model should the foreman run?` and lists every model of the signed-in Claude and Codex CLIs, Claude first, as `harness/model` with a note:

   The foreman menu uses the Claude and Codex rows below. Workers use every known-family model in each CLI's list; opencode offers individual picks across providers.

   | CLI | Model source |
   |---|---|
   | `claude` | The aliases `sonnet`, `opus`, `haiku` (Anthropic), plus any other models Claude Code's own cache lists. |
   | `codex` | The CLI default first (shown by the top-level `model` of `$CODEX_HOME/config.toml`, or `~/.codex/config.toml` when `CODEX_HOME` is unset; its note names that config path, and it is stored as `default`), then the catalog models from `codex debug models`. If the config cannot be read, the entry says `CLI default`. |
   | `kimi` | The CLI default and configured models from `~/.kimi-code/config.toml`; worker-only. |
   | `opencode` | The `provider/model` list from `opencode models`; worker-only, with multiple picks in its menu. |

   Models whose family init cannot tell are left out; Codex's `default` counts as OpenAI. `Pick 1-N, or type harness/model [D]:` takes a number or a typed `harness/model` that names a signed-in Claude or Codex model of a known family. `D` is the foreman saved by the last run, else the first Claude model, else the first Codex model; the saved one is marked `current`. After three bad answers init stops and names `--foreman HARNESS/MODEL`. With only one candidate, init prints `Foreman: HARNESS/MODEL (the only model that can plan)` and skips the model question.

   Then it asks once, for the foreman only, `How hard should the foreman think? Higher means better plans and decisions, and slower turns that use more of the plan's limits.` For Claude the levels are `medium` (marked as Claude Code's default), `low`, `high`, `xhigh`, `max`. For Codex they are the model's catalog levels, its default first (tagged `your codex default` when it comes from `model_reasoning_effort` in your config, else `codex's default`), the rest in catalog order. `Pick 1-N or type a level [1]:` takes a number or a listed level; Enter records the first entry's level, or the saved one on a rerun. A model with fewer than two levels is not asked. Workers get no effort question: for each task the foreman says how hard a model should think.

   `--foreman HARNESS/MODEL[@EFFORT]` answers both questions; without `@EFFORT` an interactive run still asks the effort. With `--yes` or `--probe`, the foreman must be among the usable `--agent` models, and its agent effort is used unless `@EFFORT` overrides it. The step ends with how to change the choice later: `brigada project foreman --harness H --model M --effort E`, or run `brigada init` again.

2. **Workers.** Each agent CLI can run tasks, and init offers every model the CLI knows. For each task the foreman says how heavy a model it needs (light, standard or heavy) and how hard it should think, and brigada picks the model of that tier with the best record on this repo. The tier comes from the model's name, unless the local config sets `tier.HARNESS/MODEL`. For each installed CLI, in order, init prints its sign-in line and then asks:

   - Claude and Codex: `Use claude's models for tasks (opus, sonnet, haiku)? [Y/n]:`. The parenthesis lists the models heaviest tier first when there are at most three, else the first model and `and N more`. Enter is yes; a rerun defaults to whether that CLI had saved agents. Yes makes every model one agent.
   - Kimi: the unchecked sign-in text and `Use kimi? [y/N]:`, then the warning that Kimi has no sandbox (when writing code it can run any command on this machine without asking; as a reviewer it only reads, [section 8](#8-agents-quota-and-health)) and `Let kimi write code too, not only review? [y/N]:`. Enter or no gives every configured Kimi model `classes=review`; yes also allows implementation and answers. A rerun defaults to the saved classes. Kimi's configured effort stays in `~/.kimi-code/config.toml`.
   - Opencode: `Which models should opencode run?`, with the `provider/model` entries from `opencode models` (only models with a recognised family are numbered, and init reports how many it left out), a `-  don't use opencode` row, and `Pick numbers (1,3) or -, or type provider/model [-]:`. It takes several comma-separated numbers, one or more typed `provider/model` names (a bare `glm-5.3` is refused), or `-`; a bad number asks again. A rerun defaults to the saved picks.
   - Every chosen model is one agent with the classes `implement+review+answer`, except Kimi's. A saved agent's other settings (account, tools, effort) are kept. The step ends with how to change this later: run `brigada init` again, or edit your own defaults in the CLIs' configs.

   `--agent` once per model replaces these questions: use `effort=LEVEL` for an explicit agent effort, and set `classes=review` explicitly to keep Kimi reviewing only; the flag's class default is `implement+review+answer`. The foreman questions are still asked, over every model of the signed-in Claude and Codex CLIs, and a foreman model that is not among the agents is registered as an agent with `classes=none`.

   A reviewer must come from a different model family than the author, so you need at least two families (for example Claude and Codex). Init counts distinct families among usable agents whose classes include `review`; two CLIs serving the same family count once. With fewer than two, init prints `LIMITED SETUP`, stops, and writes and starts nothing, unless you pass `--accept-limited`. A limited setup still runs, but reviews that need another family wait; `brigada status` shows them.

   A foreman whose CLI you decline to use for tasks (`Use codex's models…` answered no) is still registered as an agent with `classes=none`, so its turns have somewhere to run. The Plan shows it as `foreman only`.

3. **Tasks at once.** Init names this machine's core count and asks `Tasks at once [N]:` (1–64). Enter keeps the saved choice, or defaults to half the cores, at least 1 and at most 4. `--parallel N` supplies the answer; `--yes` and `--probe` use it, else the saved choice, else that default. On apply, init writes `caps.global = N`, `caps.plan = 3` and per-account caps (Claude 3, Codex 2, Kimi 1, OpenCode 2, each no higher than N) to the local brigada config, preserving other settings. A cap key the config already has keeps its value, and the plan names each value it keeps and each one an explicit answer or `--overwrite-config` changes; `caps.global` still follows an explicit answer (`--parallel N` or a typed number), and `--overwrite-config` lets the run replace every existing cap. The runner uses `--max-parallel N`. The engine reads caps when it starts. `brigada config` shows the config file to edit; after changing caps, run `brigada stop --engine`, then `brigada start` to start the engine and runner again. If the engine is already running when init changes caps, init prints the restart instructions.

4. **The check.** Init explains: `A command that must succeed (exit 0) before a task's work is accepted.` It names the build files that supplied candidates: `Found uv.lock, so the suggestion is:`, for example. With several candidates it says `so the suggestions are:` and lists one line per candidate, best first, with its note. It asks `Check command [<best>]:`; Enter takes that command. The sources are:

   | Source | Candidates |
   |---|---|
   | `Makefile`, `justfile` | targets named `check`, `ci`, `test`, `lint`, `typecheck` (as `make T` or `just T`), aggregates first; the note shows the target's prerequisites or first recipe line |
   | `package.json` scripts | `npm run check`, `npm test`, `npm run lint`, `npm run typecheck` |
   | `go.mod` | `go test ./...`, plus the Go tool scope |
   | `Cargo.toml` | `cargo test` |
   | `uv.lock` | `uv run pytest` |
   | `poetry.lock` | `poetry run pytest` |
   | `pyproject.toml` or `pytest.ini` | `python -m pytest` |

   Type the command itself to replace the suggestion, or `-` for no check (work goes straight to review; add a check later). Bare numbers are refused and asked again, up to three attempts. With no candidates, init prints `Found nothing brigada recognises.` and asks `Check command, or - for none [-]:`; Enter picks no check. The worker tools follow the choice: a suggested command's tools, or `Bash(COMMAND:*)` for a typed command (file tools only if the command cannot be represented as a tool scope). No check gives Claude file tools only (`none`), or the Go default scope (`go build`, `go test`, `go vet`, `gofmt`) in a repository with `go.mod`. `--tools` and an agent's own `tools=` still win. The step ends with how to change the check later: edit `default_check` in `.brigada/project.yaml`, then run `brigada project import`. No check leaves that key out, removing it on a rerun if needed. Init never executes the check. The tool scope applies to Claude workers (their tool list) and opencode workers (the mapped bash commands) ([section 8](#8-agents-quota-and-health)).

   On a rerun with an existing `.brigada/project.yaml`, the step is asked again: the prompt defaults to the current `default_check`, and Enter keeps it with its tools. That command is marked `current`; when it is also a suggestion it is listed only once. If the project has no check, `no check` is marked `current` and `Check command [-]:` keeps it even when candidates are found. `--check` answers the step without a question; `--check none` and `--check -` both pick no check. With `--yes` or `--probe` init asks nothing and keeps the project file's choice, else the best candidate, else no check.

   Steps 1 and 2 each end with a line saying how to change that choice later.

The `Plan` block then shows:

- `Foreman:`, which names the chosen model and its effort, as `claude/sonnet, effort medium` (no effort suffix when none is recorded). Configured defaults use their resolved names when available, otherwise `CLI default`.
- `Agents (every model of each CLI; the foreman picks the tier per task):`, with each harness, model (and `@high`, for example, when an `--agent` set an effort), family, tier and role. The tier is the model's name-based tier unless a `tier.` config key overrides it. The role is `writes code, reviews`, `reviews only` or `foreman only`; a long role wraps within its column. Family labels include `Anthropic`, `OpenAI`, `Moonshot` and `Zhipu (GLM)`.
- `Reviews:`, which counts the review-capable model families and describes cross-family review. In a limited setup, reviews that need another family still wait.
- `At once:`, with the machine limit and each usable account's cap.
- `Check:`, which names the check and reminds you that init does not run it and that you can edit `.brigada/project.yaml`, or shows `none (work goes straight to review)`.
- `What workers may do:`, with a row for each harness that implements. Claude gets `edit files; run only: …`, or `edit files; no commands` for `none`. Codex can edit and run commands inside its sandbox. Kimi, if allowed to implement, can edit and run any command without asking. Opencode gets `edit files; run only the commands of the worker tools scope`.
- `Files in this repository`, which lists each file's state first (`new`, `changes`, `unchanged`, or `left alone`, with its reason after the path), then its path and its purpose. On a first run the list is `.brigada/project.yaml` alone; on a rerun over an older init's wiring it also names the files it strips (below). Changes to existing files show diffs and keep other settings and comments; new files show full contents only with `--verbose`. Init never commits these files.
- `In the background:`, with the persistent runner, its log, `brigada stop`, and the engine's exit after 30 idle minutes.
- `Next:`, the closing text: talk to the foreman with `brigada attach`, or send `brigada ask "what you want built"` without a conversation, then watch `brigada status`.

Then, after you confirm (`y` at the prompt, or `--yes`), init:

- writes **`.brigada/project.yaml`** ([section 6](#6-configuring-a-project)). The file is yours to commit; brigada never commits or pushes anything. The write is refused if the file is a symlink or names another repository, and a file that changed after the plan was shown is not overwritten;
- strips brigada's wiring from the repository, unless `--no-wiring`: the `brigada` server in `.mcp.json`, the hooks in private `.claude/settings.local.json` and the legacy brigada hooks in shared `.claude/settings.json`, the `brigada` table in `.codex/config.toml`, `.brigada/foreman.md` and its block in `AGENTS.md` with `CLAUDE.md`'s import section, the old foreman skill, and the `/.claude/settings.local.json` line in git's `info/exclude`. Init writes no harness wiring; only brigada's own entries change, and a file you have not edited comes back byte for byte;
- starts the engine on demand, imports the project file (recording where each value came from, [section 6](#6-configuring-a-project)), registers the foreman, saves the agent and concurrency choices and starts one detached runner. The answers replace what they cover — an interactive run's questions replace every saved line of each CLI asked about, so declining a CLI removes its lines, while `--agent` replaces the harness and model pairs it names — and every other saved agent line stays in the agents file and the runner serves it, unless `--overwrite-config` is passed. If its agents or concurrency changed, init restarts the runner with the new choices; otherwise it keeps the running one. The restart waits out the runner's running jobs — no job is cut — and `--now` restarts at once instead, cutting them. The runner's log is `runner.log` in the state directory.

### Flags

| Flag | Meaning |
|---|---|
| `--probe` | Print the plan; write nothing and start nothing. |
| `--verbose` | Also print the full contents of new repository setup files. Diffs for existing files are always shown. |
| `--yes` | Apply the displayed plan without prompting. It does not confirm authentication. `--yes` and `--probe` together are a usage error. |
| `--now` | Restart a running runner at once when its agents or concurrency changed, cutting its running jobs; without it the restart waits out running jobs. |
| `--agent SPEC` | Repeat once per model. The same syntax and defaults as `brigada runner --agent` ([section 8](#8-agents-quota-and-health)): `harness=<h>[,model=<m>][,classes=…][,account=<label>][,agent-file=<path>][,effort=<level>][,tools=…]`. Replaces the worker models questions and Kimi's writing question; the foreman model and effort are still chosen in step 1. Use `classes=review` to keep Kimi reviewing only. |
| `--signed-in HARNESS` | Confirm an installed harness is signed in (repeatable or comma-separated). Local only. |
| `--foreman HARNESS/MODEL[@EFFORT]` | Choose a signed-in Claude or Codex foreman model and effort, for example `claude/sonnet@high`. `H/M` without `@E` skips the model question but still asks for effort in an interactive run. |
| `--parallel N` | Tasks at once (1–64); answers step 3 without a question. |
| `--overwrite-config` | Replace config caps and saved agent lines that already hold another value. Without it init adds only missing cap keys, reports each value it keeps and each one it changes, and keeps the saved agent lines its answers do not replace (those of CLIs an interactive run does not ask about, and harness and model pairs `--agent` does not name); the runner serves those too. |
| `--check CMD` | The default check; answers step 4 without a question. |
| `--tools SCOPE` | Worker tools in runner `tools=` syntax (`Bash(npm test:*)+Bash(npx jest:*)`, or `none`). An explicit agent `tools=` wins; a `tools.claude/<model>` line in your config file is kept unless you pass `--tools`. |
| `--accept-limited` | Go ahead with fewer than two review families. |
| `--no-wiring` | In a repository, a no-op with a note: init writes no harness wiring and, on a rerun, strips nothing; it still writes `.brigada/operator.md`, which is not wiring. In a parent folder, skip the coordinator's wiring. |
| `--all` | Only in a folder that is not a repository ([section 3](#3-several-repositories)). |
| `--state-dir DIR`, `--config PATH` | As for every command. |

### Without a terminal

Init still reads answers from standard input, one line per question, and prints each prompt on its own line. Supply answers only for questions that are asked, in this order:

1. Any unconfirmed Claude or Codex sign-in answers, before step 1.
2. The foreman model (a number or `harness/model`), then its effort when offered. `--foreman H/M@E` supplies both; without `@E`, effort is still asked. A single usable model needs no model answer.
3. Workers, in CLI order: yes or no to using Claude's and Codex's models; Kimi's use answer when unconfirmed, then whether it may write code; opencode's use answer when unconfirmed, then its comma-separated model picks.
4. Tasks at once, if `--parallel` was not given.
5. The check command, if `--check` was not given.
6. The final confirmation.

With explicit `--agent` flags, init first asks any needed authentication or use questions for those agents in CLI order, then asks the foreman model and effort; the worker model and Kimi writing questions are skipped.

On a first run, Enter picks the first foreman model and its default effort, uses all offered Claude and Codex models, leaves Kimi out at `Use kimi?`, and keeps Kimi reviewing only at its writing question. Opencode defaults to `-`, so Enter leaves it out. With `--agent`, the worker models questions and Kimi writing question are skipped; the foreman questions and any unconfirmed sign-in or use questions may still be asked.

With `--yes` or `--probe` init asks nothing, so the agents must come from `--agent`, sign-in from the status command or `--signed-in`, tasks at once from the saved choice or `--parallel`, and the check and tools from the project file, the first check candidate, or `--check` and `--tools`. If a question is needed and input is at end of file, init fails and names the flag that supplies the answer.

The final prompt says what will happen, for example `Write 4 files and start the runner? [y/N]:`. Answering no exits 1 and starts nothing. Declining one harness's sign-in only skips that harness.

A probe without prompts or writes:

```sh
brigada init --probe --agent harness=claude,model=sonnet --agent harness=codex,model=gpt-5 --signed-in claude,codex
```

If init fails after writing `project.yaml`, it says `partial init` and names the step that failed; fix the error and rerun it. Init is safe to rerun.

### Changing your setup later

Run `brigada init` again to change the foreman, its effort or the agents. Its questions default to the saved choices: the saved foreman and its effort (marked `current`), whether each CLI had agents, whether Kimi may write code and which OpenCode models were picked. The tasks-at-once step defaults to the saved limit. The check prompt defaults to the existing `.brigada/project.yaml` check, and Enter keeps it. A rerun adds config caps only where the key is absent and keeps the saved agent lines its answers do not replace — the runner keeps serving them — reporting what it keeps; declining a CLI's question removes its saved lines, and `--overwrite-config` replaces them. Confirm the plan to apply changes. If the agents or task limit changed, init stops the detached runner and starts it with the new choices.

To change only the foreman, use `brigada project foreman --harness H --model M [--effort E]`. To change the check, edit `.brigada/project.yaml`, then run `brigada project import`.

After a reboot, run `brigada start`. It starts the engine and detached runner using the saved agents and concurrency, without the init questions. It succeeds if the runner is already running; if there are no saved agents, it asks you to run `brigada init` first.

`brigada config` shows the saved agents and runner state, this repository's foreman and every project setting with its provenance, and all machine config-file keys and defaults. Each section says how to change it. Outside a registered repository it still shows agents and machine settings, with a hint to run inside a repository brigada knows. Project reads start the engine on demand. Use `--state-dir DIR` and `--config PATH` to inspect another setup.

### Stopping

`brigada stop` stops the detached runner. It applies to the whole state directory, not just the current repository. `brigada stop --engine` also stops the engine.

Stop checks the lock before signalling, sends SIGTERM, and waits up to 10 seconds for each process to release its lock; it reports failure if a process is still running. It clears stale runner records without signalling their old pids, and succeeds when nothing is running. Neither form starts an engine. Other commands start the engine again on demand; `brigada start` starts the runner again with the saved agents. `brigada config` shows whether it is running. Stop keeps the saved agent choices.

### Removing brigada from a repository

Run `brigada remove` inside the repository. It prints a removal plan, then asks `Remove brigada from this repository? [y/N]:`. Answering no leaves everything in place and prints `Nothing removed.`. The repository is identified by its `origin`, as with init.

The repository cleanup covers brigada's wiring:

- the `brigada` server in `.mcp.json`;
- the `Stop` hook (`brigada hold yield`) in `.claude/settings.local.json`, and the legacy brigada hooks in `.claude/settings.json`;
- the `brigada` table in `.codex/config.toml`;
- `.brigada/foreman.md`, and any `.claude/skills/brigada-foreman/SKILL.md` an older init left, each only when it has brigada's marker;
- brigada's managed block in `AGENTS.md` and its import section in `CLAUDE.md`: only the bytes between the markers go, with the separators init itself added, so a file you have not edited comes back byte for byte and a file init created is deleted;
- the `/.claude/settings.local.json` line in git's `info/exclude`;
- `.brigada/operator.md`, the operator guide init writes, when it still has init's generated header.

Other settings and hooks stay. A JSON file is deleted only if removing brigada's entries leaves nothing else in it. `.brigada/project.yaml` stays unless you pass `--delete-config`.

| Flag | Meaning |
|---|---|
| `--yes` | Apply the displayed plan and stop an unused runner without prompting. |
| `--cancel` | Cancel live jobs and unfinished plans when removing the project. Without it, live work makes removal fail: wait for it to finish or pass `--cancel`. |
| `--delete-config` | Also delete this repository's `.brigada/project.yaml`. |
| `--state-dir DIR`, `--config PATH` | As for every command. |

Removal archives the project, keeping its plans, tasks, stats and history ([section 6](#6-configuring-a-project)). On success it prints `brigada removed; project history kept.`. If other projects use the runner, it keeps running. Otherwise, if the runner is running, remove asks `stop the runner? [Y/n]`; Enter stops it. `--yes` accepts that too.

Remote `brigada/task-*` branches and `brigada/plans` are left alone. Remove lists them and prints `git push origin --delete` commands for you to run yourself; if the remote cannot be checked, it says so. When Codex is installed, it also prints `codex mcp remove brigada-foreman` for the user-level registration that only the pre-0.78 fallback creates. Remove does not run that command.

### By hand

The manual equivalents of what init applies (repeat `--agent` for each model):

```sh
brigada project foreman --harness claude --model sonnet --effort medium
brigada project import               # if you wrote .brigada/project.yaml
brigada runner --agent harness=claude,model=sonnet --agent harness=claude,model=opus --agent harness=codex,model=gpt-5 --max-parallel 4
```

A runner started by hand stays in its terminal.

### Where state lives

State lives in the state directory: `~/.local/state/brigada`, or `$XDG_STATE_HOME/brigada`, or what `state_dir` or `--state-dir` says. It holds:

- `store.db`: the store;
- `brigada.sock`: the socket;
- `engine.lock`, `engine.log` and `runner.log`;
- `artifacts/`: transcripts and check logs, one file each (the store only points to them);
- `tokens`: the credentials the roles use to talk to the engine, readable by you only; keep it that way;
- `runner/`: the runner's `mirrors/` of your repos, and a `worktrees/` directory per running job.

The socket path is limited to 104 bytes; a state directory that is too deep is refused with a message that says so.

The config file is `~/.config/brigada/config`, or `$XDG_CONFIG_HOME/brigada/config`, or what `--config` names. It is optional ([section 13](#13-configuration-reference)). The flags `--state-dir` and `--config` go after the command: `brigada status --state-dir DIR`.

## 2. Talking to the foreman: `brigada attach`

The foreman is an agent in a brigada-owned session: while nobody is attached, the engine wakes it headless on events, and `brigada attach` is the conversation with it. The chat and the headless turns are one transcript: your next chat sees everything the foreman did while you were away.

### Attach

**Attach** is a conversation with the foreman. Run `brigada` (or `brigada attach`) inside the repo: the repo's `origin` URL names the project, and you are put into the foreman's own session in your terminal, in the foreman's reading checkout of your repository — a clone brigada keeps under its state directory ([section 11](#11-the-foremans-harness-claude-or-codex)), where the repository's own `CLAUDE.md`, `AGENTS.md` and `.claude/` load as in any clone.

- **One at a time.** Only one chat can be attached at a time. If the foreman is in the middle of a headless turn, attach waits for it to finish; `brigada attach --force` cancels that turn instead.
- **`--model <model>`** runs this chat on another model than the project's foreman model; the session is the same, and the next headless turn uses the project's setting again.
- **`--effort <level>`** does the same for the reasoning effort, checked against the foreman's harness.
- **While attached**, the engine starts no headless turns.
- **What the foreman sees.** Its opening prompt carries the events that piled up while you were away (finished plans, parked tasks, questions) and any waiting messages; it reads new ones through its `events` tool as they arrive.
- **When you leave**, the engine records the session id the chat ended on, so the next turn continues it, and anything the foreman did not read is looked at again for a wake-up.
- **No foreman yet.** If the project has none, attach tells you the `brigada project foreman` command to run first.

With `claude`, an attached foreman is the same session the headless turns resume, so the next turn continues your chat. The first attach shows Claude Code's own "trust this folder" prompt for the foreman's working directory under the state directory: choose yes. Your own user-scope MCP servers load as in any session of yours; a repository whose `.mcp.json` still carries an older init's `brigada` entry must have it removed — rerun `brigada init` or run `brigada remove` — or the two `brigada` servers clash.

**The Codex limits.** An attached `codex` session is Codex's own terminal interface. It does not carry brigada's tools, and Codex does not expose the thread in a form brigada can write back, so a later headless turn may start a fresh Codex thread instead of continuing the chat.

### The operator flow

The usual way in is through your own coding agent, the operator. `brigada init` writes `.brigada/operator.md`, a generated, harness-agnostic guide (init overwrites it on a rerun, `--no-wiring` does not skip it, `brigada remove` deletes it). Open your agent in the repository, have it read that file, and tell it, for example, "ask brigada to fix #12". The operator:

- **asks** with `brigada ask -`, pointing at issues and document sections, the build order, tier hints and cooling accounts;
- **watches** with `brigada status`, `brigada history --task N`, `brigada history --job N` (a job's full verdict) and `brigada diff --stat N`; you watch the same work live in `brigada top`;
- **follows a landing**: merges the staging pull request with a merge commit once CI is green, and restarts the engine and runner when their own code landed and nothing pending needs the old behaviour;
- **recovers** after an outage by asking again: an ask re-wakes the foreman.

The operator has no approval authority. Plan approval, land approval, push approval, `--and-stop-asking`, tags and releases, and rule changes stay yours; the operator tells you what waits. `brigada attach` below is the other way in: your own conversation with the foreman.

### What the foreman can and cannot do

The foreman's tools are brigada's — `plan_create`, `plan_amend`, `plan_accept`, `answer`, `task_restart`, `task_cancel`, `ask_user`, `status`, `history`, `events`, `capacity`, `stats` — plus read-only file tools: on `claude` it may read and search files, with the shell and every editing tool denied; on `codex` it runs in a read-only sandbox ([section 11](#11-the-foremans-harness-claude-or-codex)). There is deliberately no tool that approves or rejects a plan, or that passes a gate: those decisions are yours ([section 7](#gates-and-rules)).

brigada enforces its own side whatever the foreman believes:

- the foreman's credential is limited to the one project, and cannot approve or reject plans or touch another project;
- plans over the approval limit wait for you;
- checks and reviews run whatever the foreman believes.

An attached chat is not charged to the foreman's wake budget ([section 4](#4-while-work-runs)); headless wakes are. If the foreman's account hits its limit, that is the flow of [section 8](#at-a-vendor-limit).

### Ask

**Ask** is a message. `brigada ask "fix the flaky auth test; the check is: go test ./auth/..."` leaves your words in the foreman's inbox and returns at once with one line that says what happened: the foreman was woken, or why the message waits (you are attached, a turn is already running so it waits for the next one, the project has no foreman, the foreman is limited). The daily wake budget does not hold back a message: a wake that carries it goes ahead. Use it for a request you do not need to discuss.

**From your own harness.** A plain Claude session can reach the foreman without you leaving it, through the coordinator bridge, `brigada mcp --coordinator`. Add it to your own MCP configuration at user scope, and only if you want it: it is opt-in, and it is not repository wiring. `brigada init` does not add it to a repository's `.mcp.json` or to your settings. For Claude Code:

```sh
claude mcp add --scope user --transport stdio brigada-coordinator -- brigada mcp --coordinator
```

or the same entry by hand in `~/.claude.json` under `mcpServers` (the `brigada` command must be on `PATH`):

```json
{
  "mcpServers": {
    "brigada-coordinator": {
      "type": "stdio",
      "command": "brigada",
      "args": ["mcp", "--coordinator"]
    }
  }
}
```

It bears the state directory's client token, so it reaches every project of that engine, and it carries no foreman credential: it cannot plan or edit, only ask, read and carry your plan decisions. Its `ask_foreman` tool takes a `repo_url` (as `list_projects` shows it) and the `text`. Two replies to a parked task work through it as plain messages: `answer task N: <answer>` and `restart task N with this note: <note>`. The foreman receives your words as they are and acts on them with its own tools; the reply of `ask_foreman` is one line only: the foreman was woken (`waking up`), the message is queued for the next turn because one is running, or it waits, and why (an attached chat, no foreman, the foreman limited). The daily wake budget bounds the foreman's own headless wakes; it does not block a message you send, so a wake that carries your message goes ahead even when the budget is spent. It does not carry the foreman's report. Read that with `status` and `history` after the foreman's turn, which is when the answer or restart shows up in the timeline.

With `-` as the only argument, ask reads the message from standard input:

```sh
git diff main | brigada ask -
```

- `--repo <origin-url>` names the project. Without it, ask takes the project that the working directory's origin names. Outside a repository, or in one with no origin, it takes the engine's only project; when the engine has several and none is named, it refuses, and `brigada project list` shows the URLs.
- A message you send is not charged against the foreman's daily wake budget ([section 4](#4-while-work-runs)): a wake that carries your message goes ahead even when that limit is spent.

## 3. Several repositories

A **project** is one repository URL (the canonical form of `origin`). Each project has its own foreman session, plans, history and stats, all served by one engine and the same runners. A monorepo is one project.

To work across several, run `brigada init` in the parent folder, not in a repository:

```sh
cd ~/work          # holds api/, web/, infra/ …
brigada init
```

In a parent folder, init:

- lists the direct children only: hidden directories are skipped, symlinks are not followed, and repositories are not descended into;
- marks each child new, known (the engine already has the project), or skipped with the reason (not a repository, no origin, ambiguous origin, a duplicate of another child);
- lets you pick which new ones to register, by number, `all`, or blank for none; `--all` registers every new one without asking;
- gives each registered project a foreman: `--foreman harness/model` if given, otherwise the foreman of an existing project; with neither, init stops;
- writes nothing into the child repositories;
- unless `--no-wiring`, writes into the parent folder `.mcp.json` (the `brigada-coordinator` server, `brigada mcp --coordinator`) and `.claude/skills/brigada-coordinator/SKILL.md` (brigada-owned, marked), and prints the Codex command to run yourself:

  ```sh
  codex mcp add brigada-coordinator -- brigada mcp --coordinator
  ```

The flags that apply here are `--all`, `--foreman`, `--yes`, `--probe`, `--no-wiring`, `--state-dir` and `--config`. `--check`, `--tools`, `--agent`, `--signed-in` and `--accept-limited` describe one repository and are a usage error here.

Registering a project this way gives it a foreman but no runner agents: run `brigada init` inside each repository to wire it and get its runner.

### The coordinator session

Open your harness in the parent folder. The coordinator session has six tools over every project the engine knows, authorised as you (the client token), not as a foreman:

- `list_projects`: every project by repository URL, with its foreman, active plans and how many things need you;
- `ask_foreman`: send one project's foreman a message as you (`repo_url`, `text`);
- `status` and `history`: across every project, or one with `repo_url`; `status` shows each plan awaiting approval with its id, version and why it waits, its tasks in the plans list (which trims finished plans like `brigada status`, `all=true` for every one);
- `plan_approve` and `plan_reject`: your decision on a plan over the approval limit, naming the project, the plan id and the **plan version you were shown**. A plan amended since is refused, and the coordinator must read `status` and show you the plan again. `plan_approve` also takes `and_stop_asking`, which changes the rule that held the plan the way `brigada project set` would ([section 7](#gates-and-rules)). These tools act for you: the coordinator is told to call them only after you said an explicit yes or no in the chat.

The coordinator never plans and never edits a task graph: it has no `plan_create`, `plan_amend` or task tool. Each project's own foreman plans.

Out of scope: a plan whose tasks span repositories (a plan belongs to one project, and each task runs in that repository), and a foreman that knows another repository's work. For a change that touches two repositories, the coordinator sends each project its own request, and you carry the coordination, such as merging in order.

`brigada project list` prints one line per project.

## 4. While work runs

`brigada status` prints:

- the engine (pid, uptime) and the job counts by status;
- the jobs a runner holds right now, with the worker's last move;
- `runner restarting after N running jobs` while a restart waits out those jobs (`--now` on init or `agents add` cuts them instead);
- any agents cooling down;
- the plans with their tasks, and the foreman's latest report;
- how many messages wait for the foreman;
- the **needs you** queue;
- one hint line when your CLIs offer models no agent registers ([section 8](#models-added-later)).

The plans list keeps every live plan — active or awaiting approval — and the three most recently finished ones (done or cancelled); older finished plans are one count line, `N older finished plans hidden; pass all=true`. `brigada status --all` lists them all. The foreman's `status` tool does the same by default; its `all` parameter is `--all`'s twin. The needs-you queue and the running jobs are never trimmed.

`brigada status --line` prints one short line for a terminal status bar; it never starts the engine.

The needs-you queue lists what only you (or the foreman) can unblock:

| Entry | What it wants |
|---|---|
| `plan #N pending_approval: <why>` | Your decision: `brigada plan approve N` or `brigada plan reject N` ([section 7](#gates-and-rules)), or the coordinator's tools ([section 3](#3-several-repositories)); `--and-stop-asking` also changes the rule that held it. |
| `task #N done: held by rule <key>: <reason>` | Your decision on a landing held at the land gate. The line carries the command that releases the hold, `brigada land approve N`, and the one that also changes the rule, `brigada land approve --and-stop-asking N`; `brigada land reject N` cancels the landing ([section 5](#5-finishing-the-land-job)). |
| `task #N done: waits to push: <branch>@<sha>` | The push gate holds it; `brigada push approve` releases every waiting push of the project ([section 5](#5-finishing-the-land-job)). |
| `task #N needs_foreman: <reason>` | Judgment: the worker asked a question, the task spent its rounds or failed after tier escalation was exhausted, or a run stopped. The foreman is woken for these; the entry is there so you can see it and step in. |
| `task #N needs_user: <reason>` | An answer from you, for a task parked for you directly. |
| `foreman #N limited: <until>` | A choice: wait, switch the foreman, or let approved plans run without it ([section 8](#8-agents-quota-and-health)). |
| `foreman #N budget_spent: wake budget spent (20/20 today, resets <UTC time>); <n> events waiting` | Raise the day's wake budget with `brigada budget --wakes N`, or send `brigada ask`: a wake that carries your message is not charged. |
| `agent <harness>/<model> broken: <reason>` | A fix, then `brigada agent reset <harness>/<model>`; repeated failures also recover by themselves ([section 8](#8-agents-quota-and-health)). For an `auth` break on a harness that reads its credential from the environment (dsh: `DEEPSEEK_API_KEY`), the reason names the variable and when the runner started, so you can see a runner that started without it; a runner that has not reported its start says so instead of a time. |

### The wake budget

The foreman is woken headless on events: a plan done, a task parked, a question. Those wakes are limited per project per UTC day, 20 by default, so a loop cannot spend your subscription unseen.

- `brigada budget` shows the budget, today's use, the reset time and how many events wait.
- `brigada budget --wakes N` (1 to 1000) sets the budget for the project, and at once wakes whatever the spent budget had parked.
- `--repo URL` names the project; without it, the engine's only project is used.
- A message you send with `brigada ask` goes ahead even when the budget is spent.
- The budget limits headless wakes only. While you are attached, the needs-you entry is not shown and the chat reads waiting events with its `events` tool.

The default for projects that have not set a budget comes from `foreman_wakes_per_day` in the config file ([section 13](#13-configuration-reference)).

When `notify` is `auto` (the default), the engine also raises a desktop notification for the same kinds of moments: a plan done, a plan waiting for approval, a task parked, the foreman's report, a limited foreman, a spent wake budget, a broken agent.

`notify.webhook` in the config file carries the same moments to one URL: every notification is also POSTed there as a small plain-JSON document — project, kind, the one-line text, the task and plan ids, the time — fit for ntfy, Slack or a Telegram bot relay, with no per-service adapters. The URL must be https (plain http is accepted only for localhost and other loopback addresses), and a redirect is never followed. For a failed check the text says only that the check failed, not what it printed. The URL may carry a token, so it lives in the config file alone, never in a project's files. Delivery is fire-and-forget: a short timeout bounds each attempt, one retry follows a failure, and a failing webhook is logged without the URL and never slows the engine.

### History, diffs and logs

`brigada history` prints the timeline, oldest first: events and messages together. `--task N` and `--job N` narrow it to one task or one job. `--limit N` (1 to 500, default 100) sets how many entries are fetched per request; the command fetches every page, so the output is always the whole timeline.

`brigada diff <task-id> [--stat] [--repo URL]` prints what a task changed, as `git diff <base>...<branch>` between the task's base ref and its latest result branch (`-r<n>` included).

- Run it inside the project's repository. The working directory's origin must be the task's repo, or diff refuses and names both; `--repo` says which URL the directory stands for when its origin is a mirror or fork.
- It fetches the two refs from origin, with no checkout, and prints to stdout, so it pipes to a pager.
- A task with no result branch yet says so.

`brigada logs <job-id>` prints what a job's worker did, from its transcript, as readable lines: messages, tool starts and ends, errors.

- `--check` prints the job's check log instead.
- `--tail N` keeps the last N lines.
- `--task N` picks that task's latest job instead of a job id (`brigada history --task N` lists them all).
- `--follow` polls a running job's latest move every two seconds until the job ends, then prints the transcript tail.

### The path a task walks

A task moves through these states, as `brigada status` shows them next to the task:

1. `pending`: waiting for the tasks it depends on. Status adds `(waiting on …)` with their names.
2. `ready`: queued for an agent. A task can also wait here because every cap that applies is full ([section 8](#8-agents-quota-and-health)); it starts when a slot frees.
3. `running`: a worker has the job. The runner's check also happens while the task is `running`; there is no separate `checking` state in the output.
4. `reviewing`: for `implement` tasks whose check passed, a reviewer of another family reads the change ([section 7](#7-plans-checks-and-reviews)). A review that asks for changes sends the task back to `running` for a fix round.
5. `done`. A `done` task whose landing is recorded — by the land job, or by a `Brigada-Task: <id>` trailer detected on the default branch ([section 5](#5-finishing-the-land-job)) — is shown as `landed` beside its state, so you can tell it from an approved task that has not merged yet.

Off this path are `needs_foreman` and `needs_user` (parked, see the queue above), `failed`, and `cancelled`.

Jobs have their own statuses, the ones in the `jobs:` line of status: `queued`, `leased`, `running`, `done`, `failed`, `cancelled`, `stalled`. A worker that makes no progress for ten minutes is killed and its job ends `stalled`; the task goes to the foreman. A worker that repeats itself also goes to the foreman, with a `task_loop` event naming the loop: the same check failure across consecutive fix rounds, or the same tool call 8 times in 10 minutes. Nothing is killed for a loop; the foreman decides.

### Cancelling

`brigada cancel <job-id>` stops one live job: a worker, a review, or a foreman turn. The runner learns of it on its next heartbeat.

It cancels that job only: not the task, not the plan, and it is not an approval of anything. To drop a whole plan that is waiting for approval, use `plan reject`. To drop tasks of a running plan, ask the foreman (it has `task_cancel`).

Job ids are not task ids. `status` shows a running job as `task #N …`; find its job id with `brigada history --task N`.

## 5. Finishing: the land job

Landing is brigada's. When a task is `done` and approved (an `approve` verdict on its last review) and every dependency of class `implement` has landed, the engine queues a **land job** for it. A runner with git for your repo — no worker, no model — squashes the task's own diff onto the target branch as one commit, reruns the project's default check on the result, and then pushes by the push gate. The commit message is built from the task's title, spec and the worker's summary, with `Brigada-Task`, `Brigada-Branch` and `Co-Authored-By` trailers. When the worker's result block carries a `commit_message`, written in the repo's own commit style (the one its AGENTS.md asks for and its git log shows), its subject and body become the message instead; the trailers stay brigada's.

Landing is per task, not per plan: a stack lands in dependency order as its tasks finish, and the target moves with the plan. It happens in a clone under the runner's state directory, never in your working tree; the runner pushes the target with its own credentials, which never reach a worker ([section 8](#8-agents-quota-and-health)). Where approved work lands is `land_target` ([section 6](#6-configuring-a-project)): the project's default branch, or a staging branch such as `brigada/landed`. A new task's work starts from there too: its worktree is built on the land target, and the land job's diff is taken against that base. A land target that does not exist on the remote yet is not an error: the worktree is built on the default branch until the first landing creates it.

Whether you land by hand or the land job does it, the engine learns the task landed: whenever the runner refreshes the project's default branch — a headless foreman turn, or `brigada attach` — it scans the branch for `Brigada-Task: <id>` trailers and records each task it finds as landed with its commit. So `brigada status` and the foreman show `landed` for a task you merged by hand, and a plan whose tasks all landed says so in its done report. A task parked for a land conflict or a rejected push, or whose push waits for approval, that you then landed by hand returns to `done` and leaves the needs-you queue.

### Gates and holds

The loop — plan, work, check, review, land, push — runs on its own; you stop it with gates and rules ([section 7](#gates-and-rules)). Two matter here:

- The **land gate** holds a landing before the squash. `gate_land: ask` holds every landing; a rule holds the one task that trips it — a tier among `approval_tiers`, a changed path matching `approval_protected_paths`, or a spent restart when `approval_restarted` is `true`. A held task stays `done`, and only its dependents' landings wait on it: work on the rest of the plan goes on. The needs-you entry names the rule and the commands that answer it, as `task #N done: held by rule approval_tiers, approval_protected_paths: heavy; touches infra/**; release: brigada land approve N; change the rule: brigada land approve --and-stop-asking N`, in `brigada status` and `brigada top` alike. A task that lands while held, by its trailer on the default branch or by its land job, leaves the queue.
- The **push gate** (`gate_push`) decides what follows a successful landing:
  - `auto` (the default): the runner pushes the target at once.
  - `ask`: the landing is recorded as waiting, and the queue shows `task #N done: waits to push: <branch>@<sha>`. The commit waits on the remote under `refs/brigada/land/<target>`, a ref outside your branches that only ever moves forward, so it outlives the runner that made it. Waiting landings stack: the next landing is squashed onto the newest waiting one, so a dependent sits on top of its dependency. `brigada push approve` releases every waiting landing of the project at once: each gets its push, no new squash, on whichever runner takes it, and the stack reaches the target in order. If the target moved outside brigada while landings waited, their push is refused as a non-fast-forward and the task parks for the foreman. Switching `gate_push` from `ask` to `auto` pushes nothing that waits: a waiting landing still waits for `brigada push approve`, and one brigada re-makes because its waiting ref went stale (the target moved past it) waits at `ask` again rather than pushing under `auto`.
  - `never`: brigada never pushes your branches. The landing's commit waits on the remote under `refs/brigada/land/<target>` — the same durable ref a waiting landing uses — and the queue shows `task #N done: push by hand: <command>`: fetch that ref and push it to the target, from your own repository, when you choose. The entry stays in the queue until the project is archived: brigada cannot see a push you ran by hand.

```sh
brigada land approve 45   # release one held landing: prints the land job it queued, or the live job already on the task
brigada land reject 45    # cancel it: the branch is left as it is, nothing recorded
brigada push approve      # release the push gate for every waiting landing
```

Only you can say these: the foreman has no tool to pass a gate.

### Conflicts and failed checks

A landing lands the head the review approved: a branch that gained a commit after its approving review is refused, nothing squashed or pushed, and the task parks at `needs_foreman` naming both heads, for the foreman to restart it so the new head is checked and reviewed.

A landing never resolves a conflict and never picks a side. When the task's diff no longer applies onto the target, or the check fails on the landed result, nothing is pushed: the task parks at `needs_foreman` with the conflicting files or the check log, and the foreman restarts it with a note (rebase onto the target's new head, resolve). The check and the review run again, and the task lands on its next approval.

### `after_land`

`after_land`, when set, is a repo-relative command the runner runs after a successful land: after the push, or after the landing when the push gate is `never`. Its failure is reported — in the landing record and `brigada history` — and never undoes the landing.

### `land_ready`

`land_ready`, when set, is a repo-relative command path inside the repository — for example `.brigada/land-ready.sh` — that a land job runs in its landing clone before anything lands, and the push phase runs again before it pushes: the gate the landing must pass. It runs under the environment of a check, bounded by the check timeout. Exit `0` lands; exit `1` holds the landing with the command's first output line as the reason — nothing squashed, pushed or recorded — the needs-you line reading `held by rule land_ready: <reason>`, released by `brigada land approve`, which re-queues the landing without re-running the command; any other exit, a command that could not start, or a timeout is `not ready` — nothing lands, and the landing is requeued on a growing backoff (5 s, then 15 s, 45 s and 2 minutes, capped there), so a gate that says "not yet" (a release in flight, a CI window closed) is asked again rather than parking the task. `brigada status` shows the task's landing target state as `ready`, `held by rule land_ready: <reason>`, or `waiting (retry at <time>, last exit <n>)`.

### By hand: `brigada land`

`brigada land` is the hand-driven form of the same squash, sharing the code and the commit message with the job: for `gate_push = never`, for a branch you want on your own machine, and for landing a task by hand. It works in your own repository and stops before the push:

```sh
brigada land <task-id>... [--onto BRANCH] [--repo URL] [--landed ID]... [--no-check]
```

Land puts approved work into your own repository, on the checked-out branch: the project's default branch, or the one named with `--onto`. It never switches branches.

- Each task becomes one commit holding only its own diff against its own base, so a stacked task lands as just its work.
- Tasks go in dependency order. A dependency must be in the same call, already landed (found by its `Brigada-Task:` trailer), or named with `--landed`.
- Then the project's default check runs once on the result.
- Land prints `git push origin <branch>` and stops: it never pushes and never touches the remote's branches.

Land refuses, changing nothing:

- a dirty tree;
- a task that is not approved;
- a local branch behind its remote;
- a directory whose origin is not the task's project;
- a dependency that is not landed;
- a missing default check (unless `--no-check`).

Run it inside the repository; `--repo` says which URL the directory stands for when its origin is a mirror or fork.

On a conflict, land stops, names the files and leaves the tree for you to resolve; commit with the printed `git commit -F` and rerun land for the rest. A check that fails leaves the commits in place and prints the `git reset --hard` that undoes them.

One limit: the runner does not record the head commit the reviewer approved, so land cannot compare it with the branch's head. It prints a warning per task saying the head was not checked against what was reviewed. Look at `brigada diff <task-id>` before you push.

## 6. Configuring a project

A project's settings come from three places:

- the built-in defaults;
- the committed file **`.brigada/project.yaml`**, which `brigada init` writes and `brigada project import` reads;
- **local settings** you set with `brigada project set`, kept in this engine's store.

`brigada project show [--repo URL]` prints every key with its effective value and where it comes from: `(default)`, `(set)` for a local setting, or `(imported <hash>)` for a value the last import set. `--repo` defaults to the origin of the repository you are in.

`brigada remove` archives the project: plans, tasks, stats and history stay in the store, and `brigada history` still shows its history. The project disappears from `brigada project list` and `brigada status`. Run `brigada init` inside the repository to bring it back. See [section 1](#removing-brigada-from-a-repository) for removal's flags and the wiring it removes.

### `.brigada/project.yaml`, schema 1

The file is flat: `key: value` lines, no nesting, one document, at most 64 KiB. Lists are written as a `[a, b]` flow list or as `- item` lines. `schema` and `repo` are required; every other key is optional, and leaving one out keeps its default. An unknown key is an error that lists the valid keys.

| Key | Value | Default | Meaning |
|---|---|---|---|
| `schema` | `1` | required | The file format version. |
| `repo` | canonical URL, e.g. `github.com/org/app` | required | The repository this file belongs to, in its canonical form (`host[:port]/path`, or `file:<absolute path>` for a file remote). Import refuses a file that names another repository. |
| `default_check` | command | none | The check a task runs when its spec names none; the land job reruns it on the landed result, and the hand-driven `brigada land` on the squashed tree. A task whose check is another command waits for you at the plan gate ([Gates and rules](#gates-and-rules)). |
| `worker_tools` | `+`-separated Claude Code tool patterns, or `none` | the agent's own scope | The shell scope of opencode workers on this project (claude workers take their scope from the agent, not from this setting) ([section 8](#8-agents-quota-and-health)). |
| `approval_max_tasks` | integer | `0` | A plan with more tasks than this waits for you. `0`, the default, turns the count rule off, and so does a negative value; the other approval rules still apply. |
| `approval_budget` | a whole number of tokens, a `$` amount, or both joined with `+` | none | The plan budget, the approval rule on by default: a plan whose recorded spend passes it holds its new jobs mid-run ([Gates and rules](#gates-and-rules)). |
| `approval_classes` | list of class names | `implement`, `review`, `answer` | A task of a class outside the list holds the plan. |
| `approval_protected_paths` | list of repo-relative globs | none | A task whose spec or check mentions a matching path holds the plan; at the land gate, a task whose recorded changed paths match holds the landing ([section 5](#5-finishing-the-land-job)). |
| `gate_plan` | `auto` or `ask` | `auto` | `ask` holds every plan at the plan gate, whatever the approval rules say. |
| `gate_land` | `auto` or `ask` | `auto` | `ask` holds every landing at the land gate; the land job runs only after you release it. |
| `gate_push` | `auto`, `ask` or `never` | `auto` | What follows a successful landing: `auto` pushes the target, `ask` waits for `brigada push approve`, `never` leaves the push to you and records the exact command. |
| `approval_tiers` | list of `light`, `standard`, `heavy` | none | Tiers whose tasks wait at the land gate. |
| `approval_restarted` | `true` or `false` | `false` | `true`: a task that spent a restart or a tier bump waits at the land gate. |
| `land_target` | branch name | the project's default branch | Where approved work lands: the default branch, or a staging branch such as `brigada/landed`. A new task's work builds on it too ([section 5](#5-finishing-the-land-job)). |
| `after_land` | command | none | A repo-relative command the runner runs after a successful land; its failure is reported, never undone. |
| `setup` | command | none | The environment step run before the worker and the check ([section 7](#7-plans-checks-and-reviews)). |
| `setup_cache_key_files` | list of repo-relative files | none (no cache) | Files whose contents key the setup cache. |
| `setup_timeout_minutes` | positive integer | `15` | How long `setup` may run. |
| `extra_repos` | list of `"name repo ref"` strings | none | Other repositories checked out read-only beside every task ([section 7](#7-plans-checks-and-reviews)). |
| `foreman_rotate_tokens` | non-negative integer | `400000` | Input tokens on one foreman turn past which the next turn starts a fresh session; `0` never rotates ([section 11](#session-size-and-rotation)). |
| `plan_files` | `on` or `off` | `on` | `off` stops the plan-file mirror: no plan file is written or pushed ([The plan file](#the-plan-file)). |
| `instructions_file` | repo-relative path | none | A text file whose contents become the project's instructions for every worker and reviewer, at most 8192 bytes (see [Project instructions](#project-instructions)). |

An example:

```yaml
schema: 1
repo: github.com/org/app
default_check: go test ./...
worker_tools: Bash(go build:*)+Bash(go test:*)
approval_max_tasks: 8
approval_protected_paths:
  - infra/**
  - db/*.sql
setup: npm ci --cache "$BRIGADA_CACHE_DIR"
setup_cache_key_files: [package-lock.json]
instructions_file: .brigada/instructions.md
```

### Local-only settings

Some keys belong to you and this engine, not to the team. The shared file refuses them, with a message naming the command to use:

| Key | Default | Meaning |
|---|---|---|
| `foreman_wakes_per_day` | `20`, or the config file's value | The daily wake budget, 1 to 1000 ([section 4](#4-while-work-runs); also `brigada budget --wakes N`). |
| `instructions` | none | A local override of the instruction text. |
| `plan_review` | `true` | The plan review before a plan's (or an active plan's amendment's) tasks queue: a `plan_review` job, an agent of a family other than the foreman's, whose verdict starts the plan or sends findings back to the foreman ([section 7](#gates-and-rules)). |
| `plan_review.tier` | `standard` | The plan reviewer's tier: `light`, `standard` or `heavy`. |
| `plan_review.effort` | `medium` | The plan reviewer's effort: `low`, `medium`, `high` or `max`. |

```sh
brigada project set [--repo URL] key=value ...
brigada project show [--repo URL]
```

- `project set` accepts every key in the two tables above except `schema`, `repo` and `instructions_file`, so a local value can also override an imported one.
- Flags go before the `key=value` pairs.
- On the command line, list values are joined with `+`: `approval_classes=implement+review`, `approval_protected_paths=infra/**+db/*.sql`, `approval_tiers=light+heavy`.
- An empty value (`approval_max_tasks=`) puts the key back to its default.
- An unknown key, or a value the key does not take, is refused with a usage error that names the valid keys, and nothing is stored.
- `project set` creates the project if the repository has none yet, and prints the same listing as `show`.

Only you can run these commands; the foreman has no tool for them.

### Importing, and provenance

`brigada project import [--repo URL] [--file PATH] [--yes]` reads `.brigada/project.yaml` (or the file named by `--file`) from the repository you are in and sends its content to the engine; the engine never reads your disk. Every value it sets is remembered as imported from that file's hash.

- On the **first** import over values you had set by hand, it prints each `hand-set … -> file …` replacement and asks; `--yes` accepts.
- On a **later** import, a changed value in the file replaces an imported value. A value you have since overridden with `project set` is **kept**, and import prints a conflict with both values; to take the file's value, run `project set key=` and import again.
- `brigada status` tells you when the file on disk has changed since the last import (or has not been imported yet).

`brigada init` imports for you. After you edit the file by hand or pull a change, run `brigada project import`.

### Project instructions

A project has one instruction text that brigada puts into every worker and reviewer prompt, whatever the harness. It goes after the worker contract (result block, no pushing, tool scope), which always wins, and before the task spec.

- Set it with `instructions_file: .brigada/instructions.md` in the committed file. Import reads the file and sends its text; the file must stay inside the repository.
- Or set a local override with `brigada project set instructions=@path` (`@-` reads standard input).
- It is at most 8192 bytes of UTF-8; a longer text is refused, never cut.
- `project show` and `status` tell you the project has instructions (their size and a digest, not the text).
- A task copies the text when it is created, so changing it later does not alter existing tasks.
- The foreman's own turns do not get the text injected; the foreman reads it through `status` before it plans.

### The foreman, effort and tools on agents

The foreman's model and effort are chosen in `brigada init`, or with `brigada project foreman [--repo URL] [--harness H --model M [--effort E]]` ([section 11](#11-the-foremans-harness-claude-or-codex)). The foreman chooses each task's effort separately. An agent's `effort` and, for Claude, `tools` are set on the runner's `--agent` ([section 8](#8-agents-quota-and-health)), with `tools.claude/<model>` in the config file as the per-model default ([section 13](#13-configuration-reference)).

## 7. Plans, checks and reviews

You ask; the foreman turns the ask into a **plan**: a set of tasks, each with a spec for the worker, a check command, a class (`implement` by default, also `review` and `answer`), and a list of the tasks it depends on.

- A task with no dependencies is queued at once.
- A task with dependencies waits (`pending`) until all of them are done. If one of them fails or is cancelled, the dependents keep waiting until the foreman re-points them to another task or drops the dependency.
- A dependency may instead name, by id, a landed task of another plan of the project — one whose work is already on the land target, done and pushed there. It counts as done from the start; nothing waits on it. One that landed on another branch, or has not landed, is refused, and so is one belonging to another project.
- The foreman can amend a live plan: add tasks, cancel tasks, re-point dependencies, change a waiting or parked task's tier and effort, or replace or append to a pending task's spec (`respec`).
- A plan is `done` when no live work is left. That is not the same as everything succeeding: the plan's done report names the tasks that failed, that were cancelled without your approval, and that never ran, and what landed and what waits at a gate ([section 5](#5-finishing-the-land-job)).

Each task also has these fields, set by the foreman in `plan_create` or `plan_amend`:

| Field | Values | Default | Meaning |
|---|---|---|---|
| `tier` | `light`, `standard`, `heavy` | `standard` | How heavy a model the task needs; the dispatcher selects an agent of that tier, with fallback when needed ([section 8](#tiers)). |
| `effort` | `low`, `medium`, `high`, `max` | the agent's effort, else `medium` | How hard the model should think on this task, mapped to the chosen harness ([section 8](#effort)). |
| `review_tier` | `light`, `standard`, `heavy` | the config's `review.tier`: one tier below the task's | The tier of the model that reviews this task, taken as given, not as a step from the task's tier ([Reviews](#reviews)). |
| `review_effort` | `low`, `medium`, `high`, `max` | the config's `review.effort`, `medium` | How hard the reviewer should think, whatever effort the task itself runs at ([Reviews](#reviews)). |
| `caps` | an object with any of `fix_rounds`, `review_rounds`, `restart`, each an integer from 0 to 10 | the engine's defaults, per key | This task's own round budgets ([Rounds](#rounds)). An absent key keeps the default; an unknown key or a value out of range refuses the whole call. |
| `continue` | the name of one of the task's `deps` | none | Which dependency's worker starts this task: when that dep is done, this task's first work job resumes the dep author's session on its own agent, so it starts with the files it already read instead of paying the cold start a new session pays. The continued dep is the task's base: `base` defaults to it, and a `base` naming another dep refuses the whole call, as a `continue` that is not one of the task's deps does. When the session or its agent is not available — the session id is unknown, the agent is gone, broken, capped at 0, cooling for more than an hour, or outside the task's tier — the task starts fresh as usual, and a `task_restart` of it always starts fresh. `brigada status` shows `continues <dep>` beside the task. |

None of the fields triggers approval. The plan shown for approval includes tier and effort, and `brigada status` shows them beside tasks: `heavy @high`, for example. A task with non-default caps also shows them there, as `(restarts 4)` or `(fix 3, review 1, restarts 4)`. A task's own review tier and review effort show as `review:heavy` and `review@high`; the config defaults are left unsaid. An unchanged standard tier is omitted; `standard->heavy` shows an automatic bump from the foreman's tier to the effective tier, and `@low->medium` an automatic effort step (`@default->high` when the task set no effort).

The foreman can use `plan_amend`'s `retier` list, for example `[{"task":"build","tier":"light","effort":"low"}]`, to change a `pending`, `ready` or `needs_foreman` task; the entry may carry `review_tier` and `review_effort` too, which change only how the task is reviewed. Omitted fields keep their values; changing the tier resets both the foreman's tier and the effective tier, and resets the effective effort to the task's effort; changing the effort sets the effective effort too. A task at `needs_foreman` still needs `task_restart` to run again. A `retier` entry may also carry `caps`, for example `[{"task":"build","caps":{"restart":4}}]`: it merges key by key into the task's caps, which is how the foreman gives a parked task more restarts before `task_restart`.

### Gates and rules

The loop — plan, work, check, review, land, push — is brigada's; you stop it with gates and rules, all keys of `.brigada/project.yaml` ([section 6](#6-configuring-a-project)). The engine enforces every stop; the foreman has no tool to pass one. A gate set to `ask` holds in the needs-you queue, and a rule turns `auto` into `ask` for the one plan or task that trips it, naming the rule.

**The plan review** (local setting `plan_review`, on by default) runs before the plan gate: a new plan — everything but one task of the `review` class — and each amendment of an active plan that adds tasks or edits a pending task's spec is first reviewed by an agent of a family other than the foreman's (`plan_review.tier`, default `standard`; `plan_review.effort`, default `medium`). The plan waits in `reviewing` (`brigada status` shows `plan 12 reviewing (round 1)`), no work queued; the reviewer's `approve` verdict then runs the plan gate's rules below, and a `changes` verdict parks the plan at `needs_foreman` and sends the findings to the foreman as a `plan_reviewed` event — whole, up to 16 KB, so a long verdict stays readable, while the status's short lines cut and point at `brigada history --job <N>`. The foreman answers by amending the plan (allowed from the parked state, the added and edited tasks going to round 2) or with `plan_accept`, which accepts the plan as reviewed with a recorded reason and lets it proceed through the same rules — at most two rounds, the second `changes` verdict proceeding through the same rules with the findings recorded. A reviewer that runs and gives no verdict is retried once on another reviewer; when that gives none either, the plan stays `reviewing` and the foreman is told why, and answers by amending the plan: added or edited tasks start another round, an amendment that only cancels or retiers lets the plan proceed. A plan with no reviewer to be had, or whose review never runs, proceeds through the rules with the reason recorded. An amendment of a plan still awaiting your yes is not reviewed: the plan gate's rules judge the amended plan whole, and your yes covers it. The user rules always have the last word: review first, rules after.

**What you see while a plan reviews.** `brigada status` lists the plan as `#12 reviewing (round 1)` with its title until the review concludes. A new plan has no work queued while it reviews. An amendment of an active plan turns that plan `reviewing` while its earlier tasks keep running; only the added and edited tasks wait. The history carries `plan_review_queued` when a round (or its one retry on another reviewer) is asked, and `plan_reviewed` when a round ends, with its status and reason. A `changes` verdict with a round left parks the plan at `needs_foreman` — `#12 needs_foreman (round 1)` in the plans list, with its findings as the needs-you entry's reason — where it waits for the foreman's `plan_amend` or `plan_accept` and nothing retries on its own; the findings show in `brigada status` and in the foreman's inbox, cut to a line with a pointer to the history that holds them whole. A plan whose review gave no verdict on its retry stays `reviewing` with the reason, and the foreman's amendment answers it. A plan with no reviewer to be had proceeds at once, the reason in its history. Plans that are one `review`-class task (`brigada review --branch`) are not reviewed.

**Turning it off.** `plan_review` is a local setting, on by default: `brigada project set plan_review=false` sends new plans and amendments straight to the plan gate's rules. Set it back to `true` to review again. `plan_review.tier` and `plan_review.effort` set the reviewer; the settings table in [section 13](#13-configuration-reference) lists all three.

**The plan gate.** A plan starts on its own unless it trips one of the per-task rules, or `gate_plan: ask` holds every plan:

- more tasks than `approval_max_tasks`, when you set one (default `0`: the count rule is off);
- a task class outside `approval_classes` (default `implement`, `review`, `answer`);
- a task whose spec or check mentions a path that matches one of `approval_protected_paths` (default none);
- a task naming an extra repository outside the project's default `extra_repos` ([section 7](#7-plans-checks-and-reviews));
- a task whose check is not the project's `default_check` (the rule `check_command`): the runner runs a check as itself, in your account and without your forge and cloud credentials, so a command you did not set waits for your yes. A task that names no check takes the default, so it never trips the rule; with no default, only a task that names a check does. Tasks have no setup of their own — they record the project's — so a setup never differs.

A held plan waits in `pending_approval`, and nothing is queued. The needs-you entry gives the reasons, for example `7 tasks, over the limit of 5`, `gate_plan is ask` or `check_command: check "make release" differs from default_check "go test ./..."`, with how to release or change it. An amendment of an already-active plan is judged by what it adds: each added task is checked on its own against the per-task rules — its class, the protected paths its spec or check mentions, its extra repositories, its check against `default_check` — and one that trips a rule is held alone, parked with a needs-you line naming the rule, while the plan stays active and the rest runs on. The task count and `gate_plan` never hold an amendment of a running plan, and cancelling, re-pointing or retiering never holds. A `respec` edit is judged on the text it adds and the task's check, with the same per-task hold. You decide:

```sh
brigada plan approve 7    # queues the tasks; prints each task's job
brigada plan reject 7     # cancels the plan's tasks
```

or through the coordinator's `plan_approve` and `plan_reject` ([section 3](#3-several-repositories)), which name the plan version you were shown.

**Stop asking.** Add `--and-stop-asking` to an approval — `brigada plan approve --and-stop-asking 7`, `brigada land approve --and-stop-asking <task>`; the coordinator's `plan_approve` takes `and_stop_asking: true` — and the approval also changes the rule that held the plan or the landing, through the same code path as `brigada project set` and recorded in the history, so the same question is not asked twice. Every hold reason maps to exactly one setting: a class joins `approval_classes`, a protected path leaves `approval_protected_paths`, an extra repository joins the project's `extra_repos`, the task count raises `approval_max_tasks`, a tier leaves `approval_tiers`, a spent restart sets `approval_restarted` to `false`, and a gate held at `ask` returns to `auto`. The check rule is the exception: the option leaves `default_check` alone, since moving it to one task's command would change every task's check. Each rule moves just far enough that the hold it answers no longer trips: a count rises to the plan's own task count, nothing wider. Without the option the approval only releases the hold, as it always has.

**The plan budget** (`approval_budget`, none by default) is the cost rule that stands in the task count's place: a whole number of tokens, a `$` amount, or both joined with `+`, for example `2000000+$5`. A plan's spend is the usage the engine records per job — input plus output tokens, and dollars when the CLI reported them — summed over the plan's jobs, unknown counters left out. When the spend passes the budget, the engine holds the plan's new jobs: every task that would newly start, a freed dependent or an amendment's addition alike, parks at the plan gate with a needs-you line naming the rule and the spend, and no job is queued; work already running goes on, with its fix rounds, reviews and landings. Only the claude CLI reports dollars; codex, opencode, kimi and dsh report tokens, so on a subscription harness without a price the budget counts tokens and a `$` side never trips. Raise the budget (`brigada project set approval_budget=...`) and have the foreman `task_restart` the held tasks to release them.

**The land gate** holds an approved task before it is squashed onto the target: `gate_land: ask`, a tier among `approval_tiers`, a changed path matching `approval_protected_paths`, or a spent restart when `approval_restarted` is `true`. You release one held landing with `brigada land approve <task>` or cancel it with `brigada land reject <task>`; `--and-stop-asking` also changes the rules that held it, as above.

**The push gate** (`gate_push`, `auto` by default) decides what follows a successful landing: `auto` pushes the target, `ask` holds the push until `brigada push approve` releases every waiting landing of the project, and `never` leaves the push to you. Both gates, their needs-you entries and `after_land` are described in [section 5](#5-finishing-the-land-job). A held task blocks nothing but its dependents' landings: work on the rest of the plan goes on.

Only you can decide: no foreman tool approves a plan or passes a gate.

### The plan file

Every change to a plan is mirrored to `.brigada/plans/<plan-id>.yaml` and pushed to the `brigada/plans` branch of your remote, not to your default branch. The file holds the graph (deps, base, and a `continue` where one is set), the task states, the branches, the open questions and the foreman's task tier and effort, but no transcripts; the setting `plan_files: off` stops the mirror, and then nothing is written or pushed. It carries the foreman's tier rather than the effective tier raised by escalation; a standard tier and an unset effort are omitted.

The store is the authority, and the file is a copy that may lag: a push that fails is retried on the next change. The file exists so that `brigada recover` can rebuild plans after a lost store ([section 10](#10-backups-and-recovery)), and so you can see a plan from another machine.

### Checks

A task's spec names a check command. After the worker reports done, the runner runs the check in the task's worktree. The exit code and the tail of the output are the result; the worker's own claim that tests pass is not used. A check that runs longer than 10 minutes is killed and counts as failed. A task may carry no check at all, and then nothing is run.

If the check of an `implement` task fails and fix rounds are left, the worker gets the check's output and fixes on its own session. A failed check on a task of any other class gets no fix round: the task parks at `needs_foreman`.

### An environment for checks and reviews

A worktree is a fresh clone, with no `node_modules` or virtualenv. Set a `setup` command in `.brigada/project.yaml` (or with `brigada project set`), and the runner runs it in the worktree before the worker starts and again before the check. It runs as the runner itself, not under the worker's tool scope, with your environment less credentials such as `GH_TOKEN`, `SSH_AUTH_SOCK` and the cloud providers' keys, and without brigada's own variables; checks and the project's `after_land` command run the same way. A review job gets the same environment before the reviewer starts.

The keys are flat in schema 1:

- `setup`: the command;
- `setup_cache_key_files`: repo-relative files such as `package-lock.json` or `go.sum`;
- `setup_timeout_minutes`: default 15.

**The cache.** The runner sets `BRIGADA_CACHE_DIR` for the command to fill with downloads (`npm ci --cache "$BRIGADA_CACHE_DIR"`).

- The cache is keyed by the contents of the named files, the command, and the runner's OS and architecture. With no key files there is no cache.
- It lives under the runner's state directory, per repository, never shared across repositories or runners.
- Each job works on its own copy. Only the run before the worker of a work job may store it, never a fix or review job, whose worktree already holds a worker's branch; so a worker cannot spoil the next job's cache.
- Entries unused for 30 days, or beyond the three most recent of a repository, are removed.

**Rules for the command.** It must be idempotent: it runs again before the check, whatever the worker did. A setup that exits non-zero or times out is an infrastructure failure, not a failed check: the task is retried up to twice, then parks at `needs_foreman`, and the setup log is an artifact of the job (`setup_log_id`). Setup output is data, never instructions.

### Another repository to read

A task may need to read a second repository: a shared schema, a generated client's source.

**Naming it.** Give the task `extra_repos` in `plan_create` or `plan_amend`, as `[{name, repo, ref}]`:

- `name` is `[a-z][a-z0-9_-]*`;
- `ref` is a branch, tag or commit, and defaults to the repository's default branch; only a full commit SHA is reproducible.

Or set a default for every task in `.brigada/project.yaml` or with `brigada project set`, one string per entry: `extra_repos="schema git@example.com:org/schema.git v2.3.0"`. Several entries are a YAML list, or joined by `+` on the command line. A task that omits the field inherits the default; a list, even `[]`, replaces it.

**What the runner does.** It clones each repository with its own git credentials beside the task's worktree, with no remote and read-only files. It sets `BRIGADA_REPO_<NAME>` (the name uppercased, `-` as `_`) to the checkout's absolute path for the setup command, the worker and the check, so a check can say `go test ./... -schema "$BRIGADA_REPO_SCHEMA"` and stay repo-relative. Two names that give one variable are refused.

**Limits.**

- The task still changes only its own repository, and the runner pushes only its branch.
- The read-only checkout is a convention, not a sandbox: a worker with a shell runs as the same user.
- A task that names a repository outside the project default holds the plan for your approval, and the reason lists the repository with its ref.
- A repository the runner cannot fetch is an infrastructure failure, retried like a failed setup.
- The commit each one resolved to is in the work job's result (`extra_repos`).

### Reviews

After an `implement` task's check passes, a reviewer reads the diff, the spec and the check result, never the author's summary (a reviewer coming back after a fix round is the one exception, below). The reviewer is an agent whose model family differs from the author's, so with a single family among your agents no review can run ([section 1](#1-install-and-brigada-init), step 2).

A review is dispatched on its own tier and effort, not the task's, because reading a diff usually takes less than writing it. By default it asks for one tier below the task's effective tier (a heavy task is reviewed at standard, a standard task at light, a light task stays at light) at medium effort, even when the escalation ladder has pushed the task to high effort. Set `review.tier = same` and `review.effort` in the config file to change the defaults for every task, or give one task a `review_tier` and `review_effort`, which win over them. The usual rules apply on top of the tier asked: the reviewer is still of another family, and when that tier has no available other-family reviewer the review climbs to the next tier up, as a work job does ([section 8](#tiers)). The history's `job_agent_picked` event records the tier asked and the tier served. Retries and re-reviews use the same review tier and effort; fix rounds and work are unchanged.

An approve counts only with substantive findings text (80 characters or more). A thinner approve, or a review that ends without a verdict, is retried once with another reviewer, and then the task waits for the foreman. The retry skips the first reviewer only while another qualifying reviewer is available — ready, or cooling for at most an hour; when none is, the first reviewer takes its own retry, and a second no-verdict from it waits for the foreman. Tasks of other classes finish when the run does, without a review.

After a review asked for changes and a fix round answered them, the next review goes back to the reviewer that asked: brigada resumes that reviewer's session on the same agent, so it does not read the repository and the whole diff again. It is shown the fix round's summary and the fix round's diff alone, and its verdict still covers the whole change; the verdict block is unchanged. This saves tokens, and it is only a preference: when that reviewer's session or agent is not available (the agent is gone, broken or cooling for more than an hour, or no longer passes the family rule), or the task was restarted, the review is a fresh one by any qualifying reviewer, and the `task_reviewing` event in the history says why. A retry after an unusable review is always fresh. A resumed review shows in the history as the pick `resumed`.

### Rounds

A review that asks for changes, or a failed check, starts a fix round on the author's session with the findings as feedback. Fix rounds for failed checks and for review changes draw on one shared budget: by default two fix rounds in all per attempt, plus three restarts (the escalation steps below each spend one, so the third leaves the foreman a restart; set `rounds.fix`, `rounds.review` and `rounds.restart` in the config file to change the defaults, and a task's own caps still override them). The foreman can set a task's own budgets with its `caps` (`fix_rounds` for failed checks, `review_rounds` for review changes, `restart`), each 0 to 10. A failed check that repeats itself — the same failing tests, or the same normalised check output, across consecutive fix rounds — parks the task at `needs_foreman` as a loop instead of queueing another round, and counts the attempt. When check or review rounds are spent, brigada first retries on the same tier with the effort one step higher (low → medium → high), then raises the task’s effective tier one step (light → standard → heavy) at the task's own effort; each step starts a fresh attempt on the next `-r<n>` branch, carrying the feedback and spending a restart ([section 8](#tiers)). A worker reporting `failed` gets the same escalation; a missing result block gets the effort steps but never a higher tier. At heavy and high effort, or with no restarts left, the task parks at `needs_foreman` and the entry says why; the foreman decides whether to re-plan, switch agent, split the task or ask you. A stalled worker always goes straight to the foreman. The foreman can change the tier or effort through `plan_amend` as described above.

### A branch with no plan

`brigada review --branch <name> [--repo <origin-url>]` queues a review of a branch on its own and prints the job id (or says one is already queued or running).

- The author's family is read from the branch's last work job; failing that, from the `Co-Authored-By` trailers on its commits; failing that, `default_reviewer` in the config file (a harness name) takes the review. With none of the three, the review is refused.
- There are no fix rounds, and a review that ends without a usable verdict is not retried: run it again.
- The verdict (approve, changes or no verdict, with the findings) is written to the history. It raises no desktop notification and no needs-you entry, so read it with `brigada history`.

## 8. Agents, quota and health

### What the runner offers

`brigada runner` takes `--agent` once per harness and model; several models of one harness are separate agents:

```sh
brigada runner --agent harness=claude,model=sonnet --agent harness=codex,account=work
```

The fields of an `--agent` value:

| Field | Meaning |
|---|---|
| `harness` | `claude`, `kimi`, `codex`, `opencode` or [`dsh`](#dsh-experimental) (experimental; see also [COMPATIBILITY](COMPATIBILITY.md)). |
| `model` | Defaults to `sonnet` for claude, `deepseek-flash` for dsh, and to the CLI's own default for the others. For opencode, a model is `provider/model`. |
| `classes` | A `+`-separated list; default `implement+review+answer`. `classes=none` advertises no task classes (foreman only). |
| `account` | The login whose quota the agent spends; defaults to the harness name (see [Accounts](#accounts)). |
| `agent-file=<path>` | kimi only. Starts work and fix sessions under your own agent definition. Reviews always use the runner's read-only definition. |
| `effort=<level>` | Validated per harness (see [Effort](#effort)). |
| `tools=<p1>+<p2>` | claude only (see [Worker tools (claude)](#worker-tools-claude)). |

Each agent serves one tier, derived from its model name:

| Tier | Model-name patterns |
|---|---|
| `heavy` | `opus`, `fable` segment prefixes, or whole `pro`, `max`, `ultra`, `astra` segments (`-pro`, `-max`, `-ultra`, `-astra`). |
| `light` | `haiku` segment prefix, or whole `mini`, `nano`, `flash`, `lite`, `highspeed`, `free`, `luna` segments (`-mini`, `-nano`, `-flash`, `-lite`, `-highspeed`, `-free`, `-luna`). |
| `standard` | Everything else, including agents configured as `model=default`. |

Segments are separated by hyphens; provider prefixes and context suffixes such as `[1m]` are removed, and matching ignores case. Heavy matches win. `tier.<harness>/<model>` in the config file overrides the tier for every agent of that harness/model. The runner's startup line shows the name-based tier; stats and capacity include the engine's override.

Without `--agent`, the shorthand flags `--harness`, `--model`, `--classes` and `--agent-file` describe one agent; there `--classes` is comma-separated (`implement,review`). The two forms cannot be mixed. Other flags:

- `--labels`: the labels the runner offers, comma-separated; `git` is always added when the list lacks it (see below), so a land job can always be leased;
- `--max-parallel N`: how many concurrent jobs this runner runs; unset (`0`) means one at a time. Init sets it from tasks at once, and `brigada start` passes the saved number;
- `--no-start`: fail instead of starting the engine.

A `land` job (see [section 5](#5-finishing-the-land-job)) carries the single label `git` and no agent. Every runner offers the `git` label — its own git does the landing, and the label is the promise that those git credentials work for the repo — so any runner may take it, wherever it runs. The landing happens in a clone under the runner's state directory, never in your working tree; the runner pushes the target with its own credentials, which never reach a worker. A landing that waits at the push gate — or lands under `gate_push: never`, whose push is yours — is kept on the remote (`refs/brigada/land/<target>`), not in that clone, so its push may run on another runner, or on your machine.

`kimi`, `opencode` and `dsh` are worker-only; the foreman can be `claude` or `codex`. [Section 11](#11-the-foremans-harness-claude-or-codex) lists what a foreman harness must do and where each harness stands.

#### dsh (experimental)

`dsh` is DeepSeek Harness (`@deepseek-ai/dsh`), a developer preview whose maintainers have announced breaking changes; the adapter was checked against 0.2.1-rc.2 only and may break on a newer release. `brigada init` offers it, marked experimental, when `dsh` is on `PATH`.

- Install: `npm i -g @deepseek-ai/dsh`, so that `dsh` is on the runner's `PATH`.
- Key: set `env.dsh.DEEPSEEK_API_KEY = keychain:<item>` or `env.dsh.DEEPSEEK_API_KEY = file:<path>` in the config file. The runner resolves it at every start, whoever starts it, and passes it to dsh jobs only; a file must be mode 0600, else the start is refused. Without that key, `export DEEPSEEK_API_KEY=...` in the environment you run `brigada start` from (the runner passes it to dsh) still works, and so does dsh's own credential store. A missing or invalid key fails the job as an authentication error.
- Run it: `brigada runner --agent harness=dsh,model=deepseek-flash`. The models are `deepseek-flash` (dsh's default, light tier) and `deepseek-v4-pro` (heavy tier).
- Each job runs `dsh --profile headless --json` with the prompt on stdin, always with `DSH_TELEMETRY_MODE=DISABLED`, and `DSH_PERMISSION_MODE=read-only` for a review or `workspace-write` for other jobs. brigada never sets `danger-full-access`. `tools=` is refused.
- The model and effort ride a per-job patch file in a fresh directory under the OS temp directory, outside the worktree. brigada does not remove it when the job ends.

**Kimi reviews only by default in interactive init.** After `Use kimi?`, init asks whether it may write code too. Enter or no keeps `classes=review`; yes enables `implement+review+answer`. With `--agent` there is no such question: set `classes=review` explicitly for a review-only Kimi agent. Omitting `classes` keeps the runner's default of all three classes.

The runner writes a read-only Kimi agent definition to `<state-dir>/runner/kimi-readonly.md`. It allows only `Read`, `Grep` and `Glob`, with no `Bash`. Kimi reviews always use this definition; a custom `agent-file=` covers work and fix jobs only. A resumed session keeps the definition it started with.

Letting Kimi write code is an opt-in. Init warns that Kimi has no sandbox: when writing code it can run any command on this machine without asking. Work and fix jobs use Kimi's normal agent, or your `agent-file=`, rather than the runner's read-only definition.

**Keep the runner running.** A job nobody leases within ten minutes fails. The ten minutes do not count while every runner that could take the job is full, or while every agent that qualifies for it is cooling after a usage limit (a fix round is pinned to its author's agent). `brigada status` shows such a job as held: waiting for a runner slot, or `waiting: <harness>/<model> cooling until <time>`. A job pinned to a broken agent still fails after ten minutes, and the failure names the agent and its state.

### Models added later

The agents file (`runner/agents` in the state directory) is written by `brigada init`, one `--agent` line per agent. A model a CLI starts offering afterwards is not registered, and nothing registers it for you: a new model can spend a paid account.

`brigada agents` is read-only. It runs the same cheap model listings init uses (the claude, codex and opencode CLIs; none makes a model call) and compares them with the agents file. It prints the models offered but not registered, each with the tier its name gives and the account it would share, and the registered agents whose model the CLI no longer lists. A model is reported as gone only when the CLI's list is whole: claude's aliases are not a complete list, so claude agents are never reported as gone, and a codex or opencode list that left out a model of unknown family reports none. The tier printed is a guess from the name: correct a wrong one with `tier.<harness>/<model>` in the config file (for example a `-highspeed` model is a faster serving of the same model, not a light one).

`brigada agents add <harness>/<model> [--classes CLASSES] [--effort LEVEL] [--now]` appends one line to the agents file and keeps every other line, comments included. For opencode, the model is `provider/model`, so `brigada agents add opencode/zai-coding-plan/glm-5.3-flash` names the harness before the first slash. `--classes` is comma-separated (default `implement,review,answer`; `none` for a foreman-only agent). The line carries the account the harness's registered agents use when that is not the harness name, so the agent shares the account the report named. The line is checked as the runner checks it at startup, so an unknown harness, an unsupported effort or a duplicate is refused and the file stays as it was. A model already registered is refused. A running runner is handed a restart request it honours between jobs: it finishes its running jobs, exits, and the fresh runner serves the new agent — no job is cut. `--now` cuts the running jobs instead, and with no runner running `brigada start` serves the line.

`brigada status` and `brigada top` show one hint line when the last listing found unregistered models. The listing is cached in `runner/models-offered.json` in the state directory with its time; `status` lists again only when the cache is missing or older than 12 hours, and `top` reads the cache on each redraw and refreshes a stale one once, in the background. Nothing runs the CLIs when no agents file exists, and `brigada agents` always lists afresh.

### Tiers

The dispatcher picks among the eligible agents in the task's effective tier, among the polling runner's agents with room under the caps. Each agent draws a plausible success rate p from its record on this repo for the task's class (a thin record draws widely, so a newer agent still gets tried), and the agents rank by `weight / p`, lowest first, where weight is the agent's `cost.<harness>/<model>` ([section 13](#13-configuration-reference)). The first agent in that order with room under the caps takes the job. A weight of 0 ranks before every paid agent. The pick is random, so two jobs in a row can go to different agents; the decision record (`why this agent`) shows each candidate's weight and draw. Sampling stays within the served tier. A tier whose eligible agents are cap-blocked waits for room, and one whose agents are cooling for at most an hour waits for the reset time; it does not fall back just because another tier is free. An agent cooling for more than an hour, such as under a limit of days, counts like a broken one. An agent whose model cap or account cap is exactly 0 can never run, so it does not count: a tier whose agents are all capped at 0 falls back like a tier with no agent. A cap that is merely full still waits.

If no qualifying agent is registered in the tier, or all are broken or cooling for more than an hour, brigada tries higher tiers in order. If none can serve it, it steps down from heavy toward light. This applies to reviews too, within the other-family rule, so a review does not wait days for a limited reviewer while another family's agent of a neighboring tier is ready. If every tier is cooling, the job waits for the earliest reset. Selection considers agents across runners, and the decision records both the tier asked for and the tier served. If no tier can serve the task, it parks for the foreman after the queue timeout, with the reason in status and a notification when enabled. Fix rounds stay pinned to the author's session and agent.

Spent check or review rounds, or a worker result of `failed`, climb an escalation ladder, one step per fresh attempt, each within the task's restart cap. First the effective effort goes up one step on the same tier: low → medium → high, starting from the effort the task ran at (the task's effort, else the agent's, else medium); the ladder never steps to max. History records `task_effort_bumped`. Only at high effort does the effective tier go up one step (light → standard → heavy), with the effort back at the task's own; history records `task_tier_bumped`. So a failed standard task at medium retries at standard/high, then at heavy at its own effort, then heavy/high. At heavy and high effort, or with the restart cap spent, the task goes to the foreman. A run that ended without a readable result block may be brigada's own fault, so it gets the effort steps but never a higher tier: such a run at high effort goes to the foreman with the reason. Transient CLI failures use the automatic retry rules below and never enter the ladder; stalls and loops always go straight to the foreman. The foreman can lower a tier or effort through `plan_amend`'s `retier` before restarting the task; a tier change starts the effort over at the task's own.

### Effort

`effort=<level>` on an agent sets how hard its CLI reasons; leave it out for the CLI's own default. brigada accepts these levels per harness and passes each one through one fixed flag, never a raw argument:

| Harness | Accepted levels | Passed as |
|---|---|---|
| `claude` | `low`, `medium`, `high`, `xhigh`, `max` | `--effort <level>` (fresh, resume and attach) |
| `codex` | `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra` | `-c model_reasoning_effort="<level>"` |
| `opencode` | provider-specific variant matching `[a-z0-9-]{1,16}` | `--variant <level>` |
| `dsh` | `off`, `low`, `high`, `max` (no `medium`) | `reasoningEffort` in the job's patch file |
| `kimi` | none: its CLI documents no effort option, so `effort=` is refused | — |

The lists are brigada's accepted values, not a promise that every model supports every one: the CLI judges that. For codex the key is model-dependent. For opencode, variant availability depends on the provider and model. A level outside the accepted list or variant syntax is a usage error at startup.

A task's `effort` uses brigada's four levels and is mapped for each job:

| Task effort | Claude | Codex | OpenCode | dsh | Kimi |
|---|---|---|---|---|---|
| `low` | `low` | `low` | `minimal` | `low` | none |
| `medium` | `medium` | `medium` | no variant | no key | none |
| `high` | `high` | `high` | `high` | `high` | none |
| `max` | `max` | `max` | `max` | `max` | none |

A nonempty mapped task level overrides the agent's effort. When mapping yields no level (including OpenCode and dsh `medium`), the agent's effort still applies; Kimi has no effort control. A task with no effort of its own and an agent with no `effort=` runs at `medium`, resolved in the four levels above before the mapping, so Claude and Codex get `medium`, and OpenCode, dsh and Kimi get nothing and use the CLI or provider default. Each job record stores the resolved level. `brigada logs` renders the CLI transcript and `brigada history` renders events and messages; neither adds a field for that stored effort level.

Effort is not part of an agent's identity: the dispatcher, `agent reset` and the outcome record still key by `harness/model`. `status` and `stats` print `claude/opus@high` only for an agent that has an effort set. The foreman's effort is chosen separately ([section 11](#11-the-foremans-harness-claude-or-codex)).

### Worker tools (claude)

A claude worker edits files in its worktree, and by default its shell is limited to `go build`, `go test`, `go vet` and `gofmt`. That default is Go only, so in a Node, Python or Rust repo the worker cannot run its own tests (the runner's check still runs on the finished tree). This is why `brigada init` sets the project's `worker_tools` from your build files ([section 1](#1-install-and-brigada-init)).

`tools=` replaces the default for one agent with a `+`-separated list of Claude Code tool patterns:

```sh
brigada runner --agent 'harness=claude,tools=Bash(npm test:*)+Bash(npx jest:*)'
```

- `tools=none` means the file tools only, no shell at all.
- The config key `tools.claude/<model>` (same syntax) sets the default for that model's agent. `tools=` on the `--agent` wins over it, and an agent with neither keeps the Go default.
- A pattern cannot contain a comma: the `--agent` value splits on commas.
- The scope applies to workers only. The foreman's own tools (read-only file tools and brigada's MCP tools, no shell and no edits) never change, whatever `tools=` says.
- `codex`, `kimi`, `opencode` and `dsh` refuse `tools=`. Dsh workers run under `DSH_PERMISSION_MODE=workspace-write` (reviews: `read-only`), an OS-level scope. Codex workers run in the `workspace-write` sandbox, codex reviews in `read-only`. A claude review gets only the file-reading tools and git's read commands, whatever the agent's scope or permission mode says, and an opencode review also denies edit and webfetch. Headless claude runs ignore the repository's own `.claude/settings.json` and `.mcp.json`, and opencode runs ignore its `opencode.json`, `.opencode/` and the project's own instruction files (`AGENTS.md`, `CLAUDE.md`); brigada's prompt carries the instructions. Kimi reviews always use the read-only agent definition above; an `agent-file=` applies to work and fix jobs, which then may also run commands without asking. What each harness's worker may run: claude, the agent's `tools=` scope (or `tools.claude/<model>`), else the Go default; the project's `worker_tools` is not read by claude; opencode, bash commands mapped from the project's `worker_tools` (the Go default scope when unset): `Bash(X:*)` allows `X` and `X *`, any pattern that does not map stays denied, and a compound command runs only if every part is allowed; a review runs none; dsh, bash sandboxed to writing the worktree and the temp directories, with `GOCACHE` set to a brigada directory under the temp directory and `GIT_OPTIONAL_LOCKS=0` for work and fix jobs. Every opencode worker also runs with `OPENCODE_PERMISSION` denying `external_directory`: a tool call that reaches outside its worktree fails with a permission error the model sees and can answer, instead of the ask a headless run auto-rejects. Should such an auto-reject still end a run without a word, the adapter reads the CLI's stderr notice and records it as the failure's reason.
- `brigada runner` prints each claude agent's scope at startup, and the foreman's capacity read shows it as `tools=` on the agent.

### Scope per harness

What a worker or reviewer may do is set by its CLI's own permission scope, not by a container. Work and fix jobs and reviews differ:

| Harness | Work and fix jobs | Reviews | Foreman |
|---|---|---|---|
| claude | `acceptEdits` with a tool allowlist: file tools and `go build`, `go test`, `go vet`, `gofmt` by default; `tools=` replaces the shell allowlist | file-reading tools and git's read commands; editing tools denied | `Read`, `Grep`, `Glob` and brigada's MCP tools; `Bash`, `Edit`, `NotebookEdit`, `Write` denied |
| codex | `workspace-write` sandbox | `read-only` sandbox | `read-only` sandbox, with MCP approvals decided by a model |
| kimi | no scope from brigada; the CLI's own permissions apply. `agent-file=` replaces the agent definition | the runner's read-only agent file, which disallows `Bash` | not available |
| opencode | `OPENCODE_PERMISSION` rules, with external directories denied unless a checkout is named | also denies edit and webfetch | not available |
| dsh | OS-level `workspace-write` (`DSH_PERMISSION_MODE`) | OS-level `read-only` | not available |

Every harness runs in a git worktree on your machine. Workers inherit your environment minus the credential scrub ([design §5.7](design.md#57-an-environment-for-checks-and-reviews)), so none of these scopes keeps a check command, a test run or a kimi worker away from your remaining credentials or the network.

### Accounts

An agent's account is the login whose quota it spends. The account label defaults to the harness name, which is right when each CLI has one login. If you have a second login for a CLI, offer it as another runner's agent with `account=<label>`, so that the two logins keep separate quotas, caps and cooling.

### At a vendor limit

**Workers.** A worker that hits its subscription limit cools the account until the reset time the CLI reported, or for 30 minutes when it gave none. Codex names the time in the limit message itself (`… or try again at Oct 24th, 2026 11:42 PM`, the same without a year, a bare clock time like `try again at 11:42 PM`, or a span like `try again in 2h 15m`), read in the machine's local time zone. Opencode's limit message names the time as `… will reset at 2026-10-20 21:32:04` (seconds optional, a trailing `Z`, `UTC` or offset such as `+08:00` honoured); without a zone it is read in the machine's local time zone, a guess that, if early, the next limit error corrects by cooling again. A time that does not parse leaves the 30-minute default. A reset more than eight days away is treated as a parse error and capped at eight days of cooling. Cooling shows on the `cooling:` line of status, not as a queue entry: `cooling: <harness>/<model> until <time> (account <label>)`. Meanwhile the dispatcher skips every agent on that account, and the task that hit the limit gets the [automatic retry](#automatic-retry) below. Cooling clears when the time passes, or at once with `brigada agent reset <harness>/<model>` if you know the limit is lifted.

**The foreman** is separate. Only a limit error on a foreman's own turn marks it limited, shown as `foreman #N limited: …` in the queue, and no new turns are started on it. When the error gave a reset time, the mark clears itself then. When it gave none (`limited, reset time unknown`), waiting never clears it: only changing the foreman does. You choose:

- wait for the reset (only when a time was given);
- switch: `brigada project foreman --repo <origin-url> --harness <h> --model <m>` (a different harness or model than the current pair), which clears the limit and wakes the new foreman for what waited;
- or do nothing: approved plans keep running without it, since the routine rules (dependencies, fix rounds, retries, reviews) do not need the foreman, but anything that needs judgment waits.

### Automatic retry

When a worker's CLI fails for a transient reason (a quota error, an authentication error, a rejected model, a server error, a lost runner), a task's first work job gets one automatic restart on another ready agent, with a fresh session and a new branch, carrying the failed job's last progress. A fix round that fails the same way is retried once instead of restarted: the author's session resumes on the author's agent when it can still take it, else the round runs fresh on another agent of the tier with the round's feedback (the check output or the review findings) in its prompt and the task's branch under it. A second failure, or a failure that is not transient, parks the task for the foreman. Set `task_auto_retry = false` to park transient failures at once.

### Broken agents

An agent is taken out of dispatch as broken when:

1. its CLI reports an authentication failure;
2. its provider rejects the agent's model, saying the account cannot use it or that it does not exist;
3. its last two jobs both failed (a limit hit is quota, not a failure, and does not count); or
4. its runner has not been heard from for 15 minutes.

The auth failure stays broken until you log the CLI in again and run `brigada agent reset <harness>/<model>`; it is never probed. A rejected model stays out the same way, until you reset the agent or remove it: its needs-you line reads `model rejected: <the provider's words>` and always points you at `brigada agents`, where you see whether the CLI still offers the model. The failure streak comes back by itself: a few minutes after it was marked broken the runner probes the CLI, a passing probe clears it, and a failing one waits longer (a growing delay) before the next probe — `brigada agent reset <harness>/<model>` still clears it at once when you know the CLI is fixed. The fourth is not a verdict on the agent: the agent comes back by itself as soon as its runner polls again.

Broken agents are listed in the needs-you queue with the reason, and raise a notification. `agent reset` prints how many agents matched. The name is the `harness/model` pair that `status` and `stats` print, and the reset applies to every runner's copy of that agent.

### Capacity

Caps bound how many jobs run at once, in layers: this runner, the account, the model, the plan and the whole engine (`caps.*` keys, [section 13](#13-configuration-reference)). A job starts only when every layer that applies has room. A task blocked only by full caps waits in the queue (it is not parked and nobody is notified) and starts when a slot frees.

A foreman turn is never held back by the plan or model caps, and goes ahead of queued task jobs. While it runs it still counts as use of its account, runner, global and model caps, so it can leave a worker on the same model waiting.

`brigada capacity` prints the same snapshot as two tables. AGENTS lists each agent with its tier, effort, cost weight, running jobs against its cap (with the layer that sets it) and quota: `ready`, `cooling until` a local time, or `broken`. LAYERS lists each cap layer with its key, cap, use and queue. It shows the project of the repository you are in; `--project URL` names another, and outside a repository it shows every project's.

`brigada top` is a live view of the engine: every `--interval` (default 2s) it redraws the agents table, the running jobs, the queue and held jobs, the active plans with their stuck tasks, the needs-you queue and the newest events. `q` quits and `p` pauses the refresh. When the engine cannot be reached the view keeps the last data and shows the error on a line while it retries; `--no-start` stops it starting an engine. When the output is not a terminal it prints one plain snapshot and exits. The same hint line as `status` sits under the header when your CLIs offer unregistered models; `top` only reads the cached listing on each redraw ([section 8](#models-added-later)).

The foreman can look at the capacity it has when it plans (per agent: the tier, the configured effort or `default`, the cap, the running jobs, the quota state and the cost weight), but the engine enforces the caps whatever the foreman believes. `cost.<harness>/<model>` is a relative weight, default 1, that the foreman sees when it decides which tasks can wait for a cheaper agent. It also steers dispatch: agents of a tier rank by `weight / p` ([section 8](#tiers)), so a dearer agent is picked less often unless its record is clearly better.

### Headroom

With `headroom = true` (the default), task workers on the account that serves your foreman's harness see that account's cap lowered by one (never below zero), so a slot stays free for the foreman. This is about parallel slots; it does not stop a vendor quota from running out. It acts only where a `caps.account.<label>` cap is set; with a cap of 1 on that account, task workers do not run there at all. Set `headroom = false` to let workers use every slot.

## 9. The record and stats

`brigada stats` prints one table per repo:

```text
AGENT  TIER  CLASS  ATTEMPTS  SUCCESSES  RATE  CONFIDENCE  INPUT  OUTPUT  CACHED  REASONING  TOKENS  CALLS  CALLS/JOB  CALLS/SUCCESS  USD  WALL(s)  COST/SUCCESS
```

- **Tier** is the model-name table label (`light`, `standard` or `heavy`), corrected by any `tier.<harness>/<model>` config override, and determines which task tier the agent serves. The record stays keyed by repo, class and agent; changing the tier label keeps the agent's existing outcomes.
- **Attempts and successes** are counted when a task ends, for the agent that wrote the final change, in the task's class.
  - A success is a task whose check passed and, for `implement` tasks, whose review approved within the allowed rounds. Tasks of other classes succeed when the run is done with its check passed, with no review.
  - A task that runs out of rounds counts an attempt without a success.
  - A reviewer's substantive approve counts as an attempt and a success in the `review` class for the reviewer's agent.
  - When the foreman's plan finishes, it is counted in the class `plan` for the foreman's agent, as a success when every task was done (or cancelled with your approval). This is measured only and does not steer dispatch.
- **Rate** is the score the dispatcher ranks by: `(successes + 0.5) / (attempts + 1)`. The prior is one pseudo-trial at 50%, so an agent with no record scores 0.50 and a single success reads 0.75, not 1.00. More attempts pull the rate toward what actually happened.
- **Confidence** is a word for the attempts behind the score: `none` for 0, `starting` for 1 to 4, `enough` from 5 on.
- **Input, output, cached and reasoning** sum CLI-reported tokens for the agent and class. **Input** is all input, cache reads included, for every harness; **cached** is the cached part of it and reasoning is a breakdown of output, so **tokens** (input plus output) compares across agents. **Calls** counts the model requests the CLI reported; **calls/job** and **calls/success** divide it by the jobs that reported calls and by successes. Calls, not tokens, is the unit for plans that limit requests per time window. **USD** sums the dollar costs the CLI supplied; **wall(s)** sums job wall time in seconds. Work, fix and review jobs all count toward the agent that ran each job (reviews in class `review`, foreman jobs in `plan`). Each job, including a resumed session, records its own reported totals; nothing is carried between jobs.
- **Cost/success** divides summed dollars by successes where dollars are known, otherwise summed tokens by successes. No successes or no reported usage displays `-`. Missing counters display `-`, distinct from reported zero. Sums include only known values, so mixed old and new jobs can yield partial totals. Usage does not steer dispatch.
- An agent whose runner has not been seen lately is marked `(stale)`; a registered agent with no record yet shows at 0 attempts.

Read it by class. For each task, the dispatcher picks the agent with the best score in the served tier among those that are ready, have quota and have room under the caps, and sends about one task in ten to the next best, so that an agent with a thin record can be measured.

Two limits:

- the ranking is among the agents of the runner that is asking for work;
- ordinary work and fix jobs are ranked on the `implement` score whatever the task's class, so scores in other classes do not steer how those tasks are dispatched (reviews are ranked on `review`).

Scores are per repo and per class: an agent that wins `implement` on one repo says nothing about another repo or about `review`. On a fresh install the table is mostly priors; trust it as the `enough` rows appear.

The record stays in the local store; nothing leaves the machine for it. For what does leave the machine, see the README's ["No telemetry"](../README.md#no-telemetry).

## 10. Backups and recovery

Set `snapshot_dir` in the config file to an absolute directory outside the state directory. A running engine then copies the whole store there through SQLite's backup interface, as `store-<UTC time>.db`, whenever there is no snapshot or the newest one is more than a day old, and keeps the last seven. The check looks at the directory, so an engine that idles out and restarts still snapshots daily. A failed snapshot is retried an hour later.

If the store is lost or damaged, stop the engine, then:

```sh
brigada recover
```

- `--snapshot-dir DIR` names a directory other than the config's.
- `--repo <origin-url>`, repeatable, names a repo whose plan files to apply beyond those of the snapshot's projects. With no snapshot, at least one `--repo` is required: the bare command has nothing to rebuild from.

`recover` moves the old store aside, restores the newest snapshot into a fresh store, and applies the plan files from the repos' `brigada/plans` branches, with the newer of snapshot and file winning. It refuses while the engine runs.

**Restored:** everything in the snapshot, as of the moment it was taken, and, from the plan files after that, the plans, their task states, branches and open questions.

**Not restored:**

- what happened after the snapshot, other than plans rebuilt from their files;
- transcripts and check logs, if their files are lost: the snapshot copies the SQLite store only, and those live in `artifacts/` in the state directory;
- task specs and check commands for tasks rebuilt from a plan file, which carries neither. A task that was mid-flight (running, checking or reviewing) has no job any more and waits at `needs_foreman` for the foreman.

With no snapshot at all, the store starts empty and is rebuilt from plan files alone, so messages, transcripts and outcomes are gone and the dispatcher's scores start again from the prior.

Recovery also raises the *fence epoch*, a counter that makes every lease from before the recovery invalid, so a runner that was running through it cannot write into the rebuilt store. Dispatch is held for one lease window, so such runners stop their workers first. The tokens are not in the store, so clients, runners and the foreman keep working, and runners register again on their next poll.

## 11. The foreman's harness: claude or codex

Only `claude` and `codex` can be a foreman; `kimi`, `opencode` and `dsh` are worker-only, for the reasons in [what a foreman harness must do](#what-a-foreman-harness-must-do). Init chooses one ([section 1](#1-install-and-brigada-init)). To change it, run `brigada project foreman --repo <origin-url> --harness <h> --model <m>`; inside the repo, `--repo` can be left out. Without `--harness` and `--model`, the command shows the current choice.

This section is about the foreman session itself: the brigada-owned session that wakes headless and that `brigada attach` joins ([section 2](#2-talking-to-the-foreman-brigada-attach)).

### What a foreman harness must do

A foreman harness must meet four requirements, and the runner and `brigada attach` depend on each:

- **(a)** resume one long-lived session headless, so every wake continues the same conversation;
- **(b)** take brigada's MCP server per run from a config the runner writes, in a strict mode so the repository's own MCP servers do not load;
- **(c)** let `brigada attach` join that same session interactively;
- **(d)** accept a read-only tool scope with no shell and no edits, since the foreman plans and never writes code.

Setup checks (a), and that the harness can carry the bridge, through `ResumeCapableHarnesses` in `internal/adapter/adapter.go`, and refuses any other harness. The strictness in (b) is not checked. Status per harness, as the code has it:

- **claude** meets all four. A headless turn runs `--mcp-config` with `--strict-mcp-config`; attach resumes the same session with the bridge added but not strict, so the user's own user-scope servers load as in any session; the scope is the foreman tool set in [scope per harness](#scope-per-harness).
- **codex** has (a). It has (b) only in part: a headless turn carries the bridge as `-c` overrides, but Codex has no strict switch, so brigada does not keep the repository's own MCP servers out (Codex reads `.codex/config.toml` from the project). It has (d)'s no-edits part, through the `read-only` sandbox, but Codex's shell tools stay available there, so the no-shell part is unmet. It lacks (c) in practice: `brigada attach` resumes the thread by id, but detach writes back the id brigada minted, which is not a Codex thread, so the next resume starts fresh; and an attached Codex session gets no MCP config, so brigada's tools are missing from it ([section 15](#15-known-limits)).
- **kimi** lacks (b), since the CLI has no MCP-config flag and the adapter refuses such jobs, and (c), since it has no attach command. It has (a), and a read-only agent file for reviews gives it (d) there only.
- **opencode** lacks (b), since its run path has no MCP-config flag and the adapter refuses such jobs, and (c), since it has no attach command. It has (a), and its review scope denies edit, bash and webfetch.
- **dsh** lacks (b), since no MCP-config flag is known for its headless profile and the adapter refuses such jobs, and (c), since it has no attach command. It has (a) (`--session-id`). Its read-only mode is OS-level and denies writes, bash writes included, so it has the no-edits part of (d), but bash still runs there, so the no-shell part is unmet.

These are current limits of the CLIs, not design choices. A harness that offers (a) to (d) could be a foreman. DeepSeek's dsh, for one, could qualify once its CLI carries an MCP config, an attachable session and a read-only mode with no shell.

### Effort and model

**Effort.** Choose it in init, or add `--effort <level>` to `brigada project foreman` to choose the foreman's reasoning effort: for `claude`, `low`, `medium`, `high`, `xhigh`, `max`; for `codex`, `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra` (the levels are explained in [section 8](#effort)). It goes with `--harness` and `--model`, and choosing again without `--effort` clears it. `project foreman --repo` shows it as `effort=<level>`. It applies to headless turns and to attach, and overrides the effort of the runner agent that serves the turn; workers and reviewers use the task's effort when set, otherwise their agent's effort.

**The model.** A headless turn runs on the harness and model you chose. The engine leases it only to a runner agent that offers exactly that `--harness`/`--model` pair, and the turn runs on that agent's adapter and model (without `--effort`, at that agent's own effort).

- If no registered agent offers the pair, the turn is not handed to another model. It waits, `brigada status` shows `foreman turn waiting: no runner offers <harness>/<model>`, and it does not expire in the queue while it waits.
- `brigada project foreman` warns when you choose a pair no registered agent currently offers, and prints the runner flag that would serve it (`--agent harness=<harness>,model=<model>`). Registering the pair releases the waiting turn.
- An agent that offers the pair but is busy, cooling or broken is a different wait, and does not trigger the warning.
- A turn already queued when you switch keeps the pair it was queued with.

### Reading the code

Headless turns and attach refresh `<state>/runner/foreman/<project>/repo` from the project's repo URL at its configured default branch. The opening prompt names the checkout's absolute path, branch and commit. The foreman reads the code and its CLAUDE.md/AGENTS.md before planning; the snapshot may lag the remote by one fetch. A clone or fetch failure fails the turn (or attach) with the git reason. The session's working directory is the checkout itself, so the repository's own instructions and project memory load as in any clone; a session recorded against another directory resumes as unknown, and a resume the CLI does not know starts a fresh session instead of failing the turn.

### What differs between claude and codex

- **claude:** the session runs in the checkout, so its CLAUDE.md and `.claude/` load as in any clone. The foreman may read files (`Read`, `Grep`, `Glob`) and call brigada's tools; the shell and every editing tool (`Bash`, `Edit`, `NotebookEdit`, `Write`) remain denied.
- **codex:** the checkout is named in the prompt, and the foreman runs with `-s read-only`. Independent config overrides keep automatic review for MCP approvals and reject sandbox and permission escalations. The `--approve-for-me` alias cannot be used here: it selects `workspace-write`, which would make the checkout's repo writable. Codex's filesystem sandbox, not instructions, is what protects the checkout from writes. Unlike claude with its explicit tool denials, codex still has shell tools.

### Codex exec approvals

For your own `codex exec` session, the approval settings are user-level Codex configuration, not entries in `.brigada/project.yaml`, and init does not write them. The headless foreman's adapter supplies exactly these settings as `-c` overrides:

```toml
approvals_reviewer = "auto_review"
approval_policy = { granular = { sandbox_approval = false, rules = false, mcp_elicitations = true, request_permissions = false, skill_approval = false } }
```

These settings auto-approve brigada's own tool calls in `codex exec`; they do not approve a brigada plan on your behalf.

Under `mcp_servers.<name>` the adapter emits only `command`, `args` and `env`; it does not set `mcp_servers.<name>.default_tools_approval_mode`. The headless foreman's read-only sandbox is set separately from these approval settings.

### Switching

Running `project foreman` again with another harness starts a fresh foreman session: a session belongs to one CLI, so the old one is dropped, and the new foreman rebuilds what it needs from the engine (the plans, the history, the pending events). A change of model or of effort on the same harness keeps the session. Switching also clears a foreman limit, as in [section 8](#at-a-vendor-limit). Nothing about plans or tasks is lost.

### Session size and rotation

Every headless turn and every attach resumes the same session, so its context only grows, and each turn rereads all of it. When the last finished turn of the session read more input tokens (input plus cached, as the adapter reports them) than the project's `foreman_rotate_tokens`, brigada rotates before the next turn or attach: it starts a fresh session, exactly as a harness switch does, and logs a `foreman_session_rotated` event with the token count and the threshold. It never rotates while a turn is running or you are attached, and never when the adapter reported no usage. The fresh session's first prompt carries a short digest the engine writes: the active plans and those awaiting approval with their tasks' states, what needs you and why, and the last ten history events; the foreman calls `status` and `history` for more. Later turns of the same session do not repeat it.

- The default is `400000`. Tune it with `brigada project set foreman_rotate_tokens=<n>` or in `.brigada/project.yaml`; `0` never rotates. `brigada project show` prints the value in effect.
- Switching the model or effort on the same harness keeps the session; only a harness switch or a rotation starts a new one.
- A cheap everyday setting is `brigada project foreman --harness claude --model sonnet` for the headless turns, with `brigada attach --model <model>` for a heavy chat: the override applies to that chat alone.

## 12. From another harness

The conversation with the foreman — `brigada attach` and `brigada ask` — is [section 2](#2-talking-to-the-foreman-brigada-attach). This section is about another harness acting for you without hosting the foreman.

`brigada mcp --client` is an MCP server over stdio for exactly that. It offers `ask_foreman` (`text`, and `repo_url` when the engine knows several projects), `status` and `history`. Register it with, for example:

```sh
claude mcp add --scope user --transport stdio brigada -- brigada mcp --client
codex mcp add brigada -- brigada mcp --client
```

A non-default state directory or config file goes after `mcp` in the launched command. That harness acts with the client token as you, never as a foreman. `brigada` must be on its `PATH`.

## 13. Configuration reference

The config file is optional. Its format:

- one `key = value` per line;
- a line starting with `#` is a comment (comments go on their own line, not after a value);
- blank lines are fine;
- an unknown key is an error, and so is a value of the wrong kind.

| Key | Value | Default | Meaning |
|---|---|---|---|
| `state_dir` | path | `$XDG_STATE_HOME/brigada`, else `~/.local/state/brigada` | The state directory. `--state-dir` overrides it. |
| `idle_minutes` | number, 0 or more | `30` | The engine exits after this many minutes with no live jobs, no attached chat and no open requests. `0` means the default. `brigada engine --idle-minutes` overrides it. |
| `notify` | `auto` or `none` | `auto` | `auto` raises desktop notifications with the OS tool when found; `none` turns them off. |
| `notify.webhook` | URL | none (off) | One https URL (http only on a loopback host) that receives every notification as a JSON POST ([section 4](#4-while-work-runs)). It may carry a token, so it lives in this file alone. |
| `snapshot_dir` | absolute path | none (no snapshots) | Where the daily store snapshots go; the last seven are kept ([section 10](#10-backups-and-recovery)). |
| `headroom` | `true` or `false` | `true` | Keep one slot free for the foreman on its account ([section 8](#headroom)). |
| `task_auto_retry` | `true` or `false` | `true` | One automatic restart on another agent after a transient worker failure ([section 8](#automatic-retry)). |
| `foreman_wakes_per_day` | integer, 1 to 1000 | `20` | Default daily foreman wake-ups per project ([section 4](#the-wake-budget)). `brigada budget --wakes N` sets it per project in the store. |
| `default_reviewer` | harness name | none | The harness that takes a standalone review when the branch's author family cannot be told ([section 7](#a-branch-with-no-plan)). |
| `rounds.fix` | integer, 0 or more | `2` | Default fix rounds per attempt for failed checks ([section 7](#7-plans-checks-and-reviews)). A task's own caps override it. |
| `rounds.review` | integer, 0 or more | `2` | Default fix rounds per attempt that a review's requested changes ask for. |
| `rounds.restart` | integer, 0 or more | `3` | Default restarts per task, escalation steps included. |
| `review.tier` | `below` or `same` | `below` | The tier a task's reviews ask for: `below` is one tier under the task's effective tier (heavy → standard, standard → light, light stays light), `same` is the task's own. A task's `review_tier` overrides it ([section 7](#reviews)). |
| `review.effort` | `low`, `medium`, `high` or `max` | `medium` | The effort reviews run at, whatever effort the task itself runs at. A task's `review_effort` overrides it. |
| `caps.global` | integer, 0 or more | no cap | Most parallel jobs in the whole engine. |
| `caps.plan` | integer, 0 or more | no cap | Most parallel tasks per plan. |
| `caps.runner` | integer, 0 or more | no cap | Most parallel jobs per runner. A runner's own `--max-parallel` also applies, and the smaller wins. |
| `caps.account.<label>` | integer, 0 or more | no cap | Most parallel jobs on one account label ([section 8](#capacity)). |
| `caps.model.<model>` | integer, 0 or more | no cap | Most parallel jobs on one model. |
| `cost.<harness>/<model>` | integer, 0 or more | `1` | Relative cost weight of an agent, shown to the foreman. Dispatch ranks the agents of a tier by weight / p, p being a draw from the agent's success record, so a higher weight is picked less often. `0` means free: ranked before every agent with a positive weight. |
| `tier.<harness>/<model>` | `light`, `standard` or `heavy` | from the model name | Override the tier for every agent of this harness/model, shown in stats and capacity. |
| `tools.claude/<model>` | `+`-separated Claude Code tool patterns, or `none` | the Go scope | Default shell scope of the claude worker agent on that model; `--agent ...,tools=` overrides it ([section 8](#worker-tools-claude)). Other harnesses are refused. |
| `env.<harness>.<VAR>` | `keychain:<item>` or `file:<path>` | the ambient variable | A variable for that harness's jobs only, resolved when the runner starts: `keychain:` reads the macOS keychain item through `security find-generic-password -w -s <item>`, `file:` reads a file that must be mode 0600. The config holds the reference, never the value, and the value never reaches a log, the store, status or an error. `PWD`, `BRIGADA_*`, `DSH_PERMISSION_MODE`, `DSH_TELEMETRY_MODE`, `OPENCODE_PERMISSION` and `OPENCODE_DISABLE_PROJECT_CONFIG` are set by the runner and refused as `<VAR>`. See [dsh](#dsh-experimental). |

A cap of `0` caps the layer at nothing, which is different from leaving it out. Init writes `caps.global = N` from tasks at once, `caps.plan = 3` and `caps.account.<label>` for each usable account: Claude 3, Codex 2, Kimi 1 and OpenCode 2, each clamped to N. If harnesses share a label, the smaller cap wins. It preserves other config entries and comments, adds a cap key only when it is absent, and groups newly added keys under `# set by brigada init`. An existing cap keeps its value — the plan names each value it keeps and each one it changes — unless the tasks-at-once answer was explicit (`--parallel N` or a typed number, which sets `caps.global`) or `--overwrite-config` is passed. The engine reads the config at startup; restart it after changing caps or tier overrides.

Project settings (gates and approval rules, `default_check`, `setup` and the rest) are not config-file keys, and the config file refuses them as unknown; they are project settings ([section 6](#6-configuring-a-project)). Effort is not a config-file key either: it is set per agent with `brigada runner --agent …,effort=<level>` ([section 8](#effort)) and per project foreman with `brigada project foreman --effort <level>` ([section 11](#11-the-foremans-harness-claude-or-codex)). `cost.*` and `tier.*` keys name `harness/model` without an effort, including a provider prefix when the model has one.

An example:

```text
notify = none
notify.webhook = https://ntfy.example.com/brigada
snapshot_dir = /home/you/backups/brigada
# set by brigada init
caps.global = 6
caps.plan = 3
caps.account.claude = 3
caps.account.codex = 2
cost.claude/opus = 2
tier.codex/gpt-5 = heavy
```

### Project settings

Every key that `brigada project set` and `project show` know, with each default as `project show` prints it:

| Key | Default | In `project.yaml`? |
|---|---|---|
| `approval_max_tasks` | `0` | yes |
| `approval_budget` | `none` | yes |
| `approval_classes` | `implement+review+answer` | yes |
| `approval_protected_paths` | `none` | yes |
| `gate_plan` | `auto` | yes |
| `gate_land` | `auto` | yes |
| `gate_push` | `auto` | yes |
| `plan_review` | `true` | no, local only |
| `plan_review.tier` | `standard` | no, local only |
| `plan_review.effort` | `medium` | no, local only |
| `approval_tiers` | `none` | yes |
| `approval_restarted` | `false` | yes |
| `land_target` | the project's default branch | yes |
| `after_land` | `none` | yes |
| `land_ready` | `none` | yes |
| `default_check` | `none` | yes |
| `worker_tools` | `agent default` | yes |
| `setup` | `none` | yes |
| `setup_cache_key_files` | `none` | yes |
| `setup_timeout_minutes` | `15` | yes |
| `extra_repos` | `none` | yes |
| `foreman_rotate_tokens` | `400000` | yes |
| `foreman_wakes_per_day` | `20`, or the config file's value | no, local only |
| `plan_files` | `on` | yes |
| `instructions` | `none` | no, local only (the file's counterpart is `instructions_file`) |

## 14. Command reference

`brigada help` prints the command list. `brigada <command> --help` (also `-h` or `-help`) prints a command's usage, description and flags; `brigada help <command> [<subcommand>]` prints the same help. For the two-level commands `project`, `agent`, `plan` and `task`, `--help` or `help` lists the subcommands; use, for example, `brigada project foreman --help` for a subcommand's flags. Help exits 0. A usage error names the relevant help command on stderr.

A command's flags go after its name and before its positional arguments (`diff`, `logs` and `land` also accept flags after their ids). Every executable command also takes `--state-dir DIR` and `--config PATH`, and most client commands take `--no-start` (fail instead of starting the engine); the synopses below leave these out. Exit codes: 0 ok, 1 error, 2 usage. Init's setup refusals exit 1. `brigada hold yield` and `brigada hold confirm` are no-ops that always exit 0; every other `hold` subcommand is an unknown command.

### Starting

- `brigada init [--probe | --yes] [--verbose] [--agent SPEC]... [--signed-in HARNESS]... [--foreman HARNESS/MODEL[@EFFORT]] [--parallel N] [--check CMD] [--tools SCOPE] [--accept-limited] [--no-wiring] [--all]`: set up one repository, or, in a folder that is not a repository, the projects under it. See [sections 1](#1-install-and-brigada-init) and [3](#3-several-repositories).
- `brigada start`: start the engine and detached runner with the agents and tasks-at-once limit saved by init; succeeds when the runner is already running. Use it after a reboot or `brigada stop`. See [section 1](#changing-your-setup-later).
- `brigada runner [--agent SPEC]... [--harness HARNESS] [--model MODEL] [--classes CLASSES] [--agent-file PATH] [--worker-script PATH] [--labels LABELS] [--max-parallel N]`, where SPEC is `harness=<h>[,model=<m>][,classes=<c1>+<c2>|none][,account=<label>][,agent-file=<path>][,effort=<level>][,tools=<p1>+<p2>|none]`: run a runner that leases jobs, runs workers and checks, and pushes branches. The shorthand agent flags cannot be mixed with `--agent`; `--worker-script` is for the `fake` harness used by tests. See [section 8](#8-agents-quota-and-health).
- `brigada stop [--engine]`: stop the detached runner in the selected state directory; `--engine` also stops the engine. Sends SIGTERM and waits up to 10 seconds per process; succeeds if nothing is running, and never starts the engine. See [section 1](#stopping).
- `brigada remove [--yes] [--cancel] [--delete-config]`: remove brigada from this repository, keeping its project history. `--yes` applies the displayed plan and stops an unused runner without prompting; `--cancel` cancels live jobs and unfinished plans; `--delete-config` also deletes `.brigada/project.yaml`. See [section 1](#removing-brigada-from-a-repository).
- `brigada mcp (--coordinator | --client | --socket PATH)`: serve brigada's tools over stdio. `--coordinator` is the coordinator bridge ([section 3](#3-several-repositories)), `--client` the tools of [section 12](#12-from-another-harness). Only one selector at a time. `--socket PATH` is the bridge a headless foreman session and an attached chat use; you do not run that by hand.

### Talking

- `brigada`, `brigada attach [--force] [--model M] [--effort E]`: chat with this repo's foreman. See [section 2](#attach).
- `brigada ask [--repo URL] <message...>` (or `-` for standard input): send the foreman a message without attaching. See [section 2](#ask).

### Watching

- `brigada config`: show agents, runner state, foreman, project settings with provenance, and machine config-file values and defaults, with their locations and commands to change them. See [section 1](#changing-your-setup-later).
- `brigada status [--line] [--all]`: the engine, jobs, plans and the needs-you queue, plus a hint when your CLIs offer unregistered models. See [section 4](#4-while-work-runs).
- `brigada agents`: the models your CLIs offer that no agent registers, with their name-based tier and account, and registered models a CLI no longer lists; read-only. See [section 8](#models-added-later).
- `brigada agents add <harness>/<model> [--classes CLASSES] [--effort LEVEL] [--now]`: register one model by appending its line to the agents file; restart a running runner to serve it, waiting out its running jobs, or cutting them with `--now`. See [section 8](#models-added-later).
- `brigada history [--task N] [--job N] [--limit N]`: the timeline. See [section 4](#history-diffs-and-logs).
- `brigada diff <task-id> [--stat] [--repo URL]`: what a task changed, as `git diff base...branch`; run inside the repo. See [section 4](#history-diffs-and-logs).
- `brigada logs <job-id> [--check] [--tail N] [--follow]`, or `brigada logs --task N`: a job's transcript as readable lines, or its check log. See [section 4](#history-diffs-and-logs).
- `brigada stats`: tier, attempts, successes, rate and confidence per agent, class and repo. See [section 9](#9-the-record-and-stats).
- `brigada top [--interval D] [--project URL] [--no-start]`: the engine full-screen, with the same unregistered-models hint as `status`; one plain snapshot when piped. See [section 8](#capacity).
- `brigada capacity [--project URL]`: each agent's tier, effort, cost, running jobs against its cap, and quota; each cap layer's cap, use and queue. See [section 8](#capacity).

### Finishing

- `brigada land <task-id>... [--onto BRANCH] [--repo URL] [--landed ID]... [--no-check]`: the hand-driven squash of approved tasks onto the checked-out branch, one commit each, in dependency order; rerun the default check; print the push command and stop. See [section 5](#5-finishing-the-land-job).
- `brigada land approve <task>`, `brigada land reject <task>`: your decision on a landing held at the land gate; approve queues its land job, reject cancels the landing and leaves the branch; `--and-stop-asking` also changes the rule that held it. See [section 5](#gates-and-holds).
- `brigada push approve [--repo URL]`: release the push gate; every waiting landing of the project gets its push queued. See [section 5](#gates-and-holds).

### Deciding

- `brigada plan approve <id>`, `brigada plan reject <id>`: your answer on a plan held at the plan gate; `--and-stop-asking` also changes the rule that held it. See [section 7](#gates-and-rules).
- `brigada budget [--repo URL] [--wakes N]`: show the project's foreman wake budget, with today's use and the UTC reset time; `--wakes N` (1 to 1000) sets it. See [section 4](#the-wake-budget).
- `brigada agent reset <harness>/<model>`: clear a broken or cooling agent. See [section 8](#broken-agents).
- `brigada cancel <job-id>`: stop one live job. See [section 4](#cancelling).
- `brigada review --branch <name> [--repo URL]`: review a branch with an agent of another family, with no plan. See [section 7](#a-branch-with-no-plan).
- `brigada task submit --repo URL --spec FILE|- [--base REF] [--check CMD] [--title T] [--harness H]`: submit one task directly, without the foreman. A debugging path; the base ref defaults to `main`.

### Projects

- `brigada project foreman [--repo URL] [--harness H --model M [--effort E]]`: show the repo's foreman (`claude` or `codex`) and its effort, or set them with `--harness` and `--model`; `--repo` defaults to the origin of the working directory. See [section 11](#11-the-foremans-harness-claude-or-codex).
- `brigada project list`: every project with its foreman, active plans and needs-you count. See [section 3](#3-several-repositories).
- `brigada project set [--repo URL] key=value ...`, `brigada project show [--repo URL]`: set and read the project's settings and where each comes from. See [section 6](#local-only-settings).
- `brigada project import [--repo URL] [--file PATH] [--yes]`: import `.brigada/project.yaml`. See [section 6](#importing-and-provenance).

`plan_amend` is a foreman MCP tool ([section 7](#7-plans-checks-and-reviews)): it takes `plan` and optional `add`, `cancel`, `retarget`, `retier` and `respec` lists. Each `retier` entry names `task` (name or id) and optionally `tier` and `effort`. Each `respec` entry names `task`, the `spec` text and a `mode` of `replace` (the default) or `append`; it edits a `pending` task, one with no job yet, and is refused for a task that has started, where a follow-up task is the way. An edit that trips a plan rule (a protected path in the new text, the task's class) holds that task at `needs_foreman` while the plan stays active.

`plan_accept` is a foreman MCP tool over the plan review ([section 7](#7-plans-checks-and-reviews)): it takes `plan` and a required `reason`, and accepts a plan the review parked at `needs_foreman` as reviewed — the plan proceeds exactly as a spent round's verdict does, so its tasks start, or the plan waits for your yes when a rule holds it. The reason is recorded in the plan's history.

### Care

- `brigada engine [--idle-minutes N]`: run the engine in the foreground. You rarely need it; commands start it on demand.
- `brigada recover [--snapshot-dir DIR] [--repo URL]...`: rebuild a lost store; the engine must be stopped. See [section 10](#10-backups-and-recovery).
- `brigada version`: print the version.
- `brigada help [<command> [<subcommand>]]`: print top-level or command usage.

## 15. Known limits

- Model-name tier patterns can go stale as vendors rename models; correct them with `tier.<harness>/<model>` in the config file.
- Every tier bump spends a restart, including a second bump after the bumped attempt fails; earlier retries can exhaust the cap before a task reaches heavy.
- The runner does not record the commit a reviewer approved, so `brigada land` only warns that the head was not compared ([section 5](#5-finishing-the-land-job)).
- Read-only extra repositories are a convention, not a sandbox ([section 7](#another-repository-to-read)).
- A repository whose `.mcp.json` still carries an older init's `brigada` entry must drop it (rerun `brigada init` or run `brigada remove`), or the two `brigada` servers clash in an attached chat ([section 2](#attach)).
- An attached Codex session does not carry brigada's tools, and Codex has no verified project-scope registration for the coordinator: init in a parent folder prints a user-level command ([section 3](#3-several-repositories)).
- Cross-repository plans do not exist: a plan belongs to one project ([section 3](#3-several-repositories)).
- There is no `brigada cache clear` command; the setup cache expires by age and count ([section 7](#an-environment-for-checks-and-reviews)).
- Workers run in git worktrees, not containers, and checks run as you, in your account, though without your forge and cloud credentials (`GH_TOKEN`, `SSH_AUTH_SOCK` and the like). Do not point brigada at a repository you do not trust.

### Infrastructure outages

A failure of brigada's own plumbing, such as a host that cannot resolve `github.com` or a remote that cannot be fetched, is the infra class. It is not a failed task: the work or fix job is queued again on the same key and no restart, round or foreman wake is spent. The retry waits out a growing backoff of 1 minute, then 2, 5 and 15 minutes, staying at 15 minutes while the window lasts. A waiting retry shows in `brigada status` and in the QUEUE section of `brigada top` as `task #4 work: retry in 5m: could not resolve github.com`, with the time left and the failure's words. A run of infra failures that fills a 3-hour window from its first failure is spent: the next one parks the task for the foreman, and the needs-you entry names the outage. A foreman turn that fails this way waits out the same backoff and window.

Back to the [README](../README.md).
