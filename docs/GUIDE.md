# brigada: user guide

This guide follows how an engineer uses brigada: install and run `brigada init` in a repository, work from your own harness session, add a coordinator over several repositories, watch the work, land it, and configure the project. The [README](../README.md) is the short version. For the terms of use, see [TERMS.md](TERMS.md).

Contents:

1. [Install and `brigada init`](#1-install-and-brigada-init)
2. [Your own session is the foreman (mode A)](#2-your-own-session-is-the-foreman-mode-a)
3. [Several repositories (mode B)](#3-several-repositories-mode-b)
4. [While work runs](#4-while-work-runs)
5. [Finishing: `brigada land`](#5-finishing-brigada-land)
6. [Configuring a project](#6-configuring-a-project)
7. [Plans, checks and reviews](#7-plans-checks-and-reviews)
8. [Agents, quota and health](#8-agents-quota-and-health)
9. [The record and stats](#9-the-record-and-stats)
10. [Backups and recovery](#10-backups-and-recovery)
11. [The foreman's harness: claude or codex](#11-the-foremans-harness-claude-or-codex)
12. [Fallback: attach and ask](#12-fallback-attach-and-ask)
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

- The **foreman** is an agent session (a `claude` or `codex` CLI) that has brigada's tools. It turns your asks into plans and decides what to do when a task needs judgment.
- The **engine** holds the store (one SQLite file), the rules and the dispatcher, and serves everything on one Unix socket. It runs no model. It starts on demand when any command needs it and exits after 30 idle minutes (no live jobs, no attached chat, no open requests).
- The **runner** is the process you leave running. It offers your signed-in CLIs as agents, leases jobs from the engine, runs each worker in a git worktree, runs the task's check, and pushes the task's branch.
- The **repo remote** is where the branches land, along with the plan files on the `brigada/plans` branch.

### What init does

Run `brigada init` inside a repository. (At the top of a folder that holds several repositories, init does something else: see [section 3](#3-several-repositories-mode-b).) The repository must have an `origin` remote, and all its URLs must name one repository. Init prints the canonical repository identity, then walks you through three steps. Nothing is written or started until you confirm the plan at the end.

1. **Agents.** Init checks installed CLIs in order: `claude`, `codex`, `kimi`, `opencode`. For each one, it checks or asks about sign-in, then offers a numbered model menu.

   - For Claude and Codex, init runs `claude auth status` and `codex login status` if their installed help documents them. If neither the probe nor `--signed-in` confirms authentication, it asks whether you are signed in. Answering no skips that CLI and its menu.
   - Kimi has no verified sign-in check. Init tells you to run `kimi login` first if needed, then asks `Use kimi? [y/N]:`. Answering no leaves it out.
   - For opencode, init runs `opencode providers list` if its help documents `providers`, or `opencode auth list` on older versions. Stored provider credentials confirm sign-in. If sign-in remains unconfirmed, it asks `Use opencode? [y/N]:`. When the check is unavailable, it tells you to run `opencode auth login` first if needed. Keys may also come from your environment.
   - `--signed-in` confirms authentication without a question. With a distinct `account=LABEL` on `--agent`, a sign-in or use question names that account. Status output is read in memory, never printed or stored. Init never runs a login command or a model.

   Each menu asks `Which model should HARNESS run?`. Its numbered entries show the model and family:

   | CLI | Menu source and choices |
   |---|---|
   | `claude` | The three explicit aliases `sonnet`, `opus`, `haiku` (Anthropic). `sonnet` is first; these are not a configured CLI default. |
   | `codex` | One entry for the CLI default. Init shows the top-level `model` from `$CODEX_HOME/config.toml`, or `~/.codex/config.toml` when `CODEX_HOME` is unset. |
   | `kimi` | The CLI default from top-level `default_model` in `~/.kimi-code/config.toml`, followed by other configured model names from `[models.…]` tables, without duplicates. |
   | `opencode` | `provider/model` entries from `opencode models`, when its help documents the command. Only models with a recognised family are numbered. Init reports how many unknown-family models it left out. |

   - Pick a number or type a model ID. For example, Claude asks `Pick 1-3 or -, or type a model id [1]:`. Enter picks the first entry for Claude, Codex and Kimi. For opencode the default is `-`: Enter leaves it out. Claude, Codex and opencode show `-` to skip; leave Kimi out by answering no to `Use kimi?`.
   - Codex and Kimi keep the model value `default` for their first entry, even when init can display its configured name. If the config cannot be read or recognised, the entry says `CLI default`. Codex's `default` counts as OpenAI and Kimi's as Moonshot. An opencode `default` supplied through `--agent` has an unknown family and does not count toward review families.
   - An explicit opencode model must use `provider/model`, for example `zai-coding-plan/glm-5.3` (shown as `Zhipu (GLM)`); a bare `glm-5.3` is refused. Init rejects explicit models whose family it cannot identify. Kimi aliases fall back to Moonshot unless their names identify another known family.
   - After a Claude, Codex or opencode model with two or more effort levels, init asks how hard it should think. Pick a number or type a listed level. Enter keeps the CLI default and records no `effort=` override. The levels stay in the source's order, with the default marked; an unknown default gets its own first entry. Opencode models without variants have no effort question.
   - Kimi's configured effort is shown for information only; change it in `~/.kimi-code/config.toml`.
   - After you pick Kimi's model, init asks `Let kimi write code too, not only review? [y/N]:`. Enter or no gives it only `classes=review`. Yes also allows implementation and answers. The warning explains that Kimi has no sandbox: when writing code it can run any command on this machine without asking. As a reviewer it only reads ([section 8](#8-agents-quota-and-health)).
   - `--agent` replaces the model and effort menus and Kimi's question; use `effort=LEVEL` for an explicit effort. Set `classes=review` explicitly to keep Kimi reviewing only; the flag's class default is `implement+review+answer`.

   A reviewer must come from a different model family than the author. Init counts distinct families among usable agents whose classes include `review`; two CLIs serving the same family count once. With fewer than two, init prints `LIMITED SETUP`, stops, and writes and starts nothing, unless you pass `--accept-limited`. A limited setup still runs, but reviews that need another family wait; `brigada status` shows them.

2. **The foreman.** Pick among the usable Claude and Codex agents, using the models and efforts chosen above. The foreman plans work and handles decisions, and spends that agent's quota when work finishes. With two candidates, init asks for a number. Enter picks the first usable resume-capable agent in `--agent` order, or Claude before Codex without `--agent`. With only one candidate, init names it without asking. `--foreman HARNESS/MODEL` supplies the choice and skips the question; it must name a usable Claude or Codex agent.

3. **How brigada tests the work.** Init explains that when an agent finishes a task, brigada runs one command in that task's own copy of the repo (never in your working copy). If it passes (exit 0), the work goes on to review by another model family; if it fails, the work goes back to the agent with the output. It then lists what it found in the repo, best first, and asks `Pick 1-N or -, or type a command [1]:`:

   | Source | Candidates |
   |---|---|
   | `Makefile`, `justfile` | targets named `check`, `ci`, `test`, `lint`, `typecheck` (as `make T` or `just T`), aggregates first; the note shows the target's prerequisites or first recipe line |
   | `package.json` scripts | `npm run check`, `npm test`, `npm run lint`, `npm run typecheck` |
   | `go.mod` | `go test ./...`, plus the Go tool scope |
   | `Cargo.toml` | `cargo test` |
   | `uv.lock` | `uv run pytest` |
   | `poetry.lock` | `poetry run pytest` |
   | `pyproject.toml` or `pytest.ini` | `python -m pytest` |

   A number picks that entry, Enter takes entry 1, and other text is the command itself. Every menu ends with `-  no check`: pick `-` to send work straight to review and add a check later. With no candidates, init prints `Found in this repo: nothing brigada recognises.`, marks no check as the default, and asks `Type a command, or - for no check [-]:`; Enter picks no check. The worker tools follow the choice: the picked candidate's tools, or `Bash(COMMAND:*)` for a typed command (file tools only if the command cannot be represented as a tool scope). No check gives Claude file tools only (`none`), or the Go default scope (`go build`, `go test`, `go vet`, `gofmt`) in a repository with `go.mod`. `--tools` and an agent's own `tools=` still win. The step ends with how to change the check later: edit `default_check` in `.brigada/project.yaml`, then run `brigada project import`. No check leaves that key out, removing it on a rerun if needed. Init never executes the check. The tool scope applies to Claude workers only ([section 8](#8-agents-quota-and-health)).

   On a rerun with an existing `.brigada/project.yaml`, the step is asked again: the current `default_check` is entry 1, marked `current, from .brigada/project.yaml`, followed by the other candidates, and Enter keeps it with its tools. If the project has no check, the last row, `-  no check`, is marked `current` and Enter keeps it even when candidates are found. `--check` answers the step without a question; `--check none` and `--check -` both pick no check. With `--yes` or `--probe` init asks nothing and keeps the project file's choice, else candidate 1, else no check.

   Steps 1 and 2 each end with a line saying how to change that choice later.

The `Plan` block then shows:

- `Agents:`, with each harness, model and effort (`@high`, for example; shown only when one is set), family and role phrase, such as `code + review` or `review only`. Configured defaults use their resolved names when available, otherwise `CLI default`. Family labels include `Anthropic`, `OpenAI`, `Moonshot` and `Zhipu (GLM)`.
- `Reviews:`, which counts the review-capable model families and describes cross-family review. In a limited setup, reviews that need another family still wait.
- `Foreman:`, which names the chosen agent.
- `Check:`, which names the check and reminds you that init does not run it and that you can edit `.brigada/project.yaml`, or shows `none (work goes straight to review)`.
- `What workers may do:`, with a row for each agent that can implement. Claude gets `edit files; run only: …`, or `edit files; no commands` for `none`. Codex can edit and run commands inside its sandbox. Kimi, if allowed to implement, can edit and run any command without asking. Opencode gets `edit files; no commands`.
- `Files in this repository`, which lists each file's state first (`new`, `changes`, `unchanged`, or `left alone`, with its reason after the path), then its path and its purpose. Private Claude settings have `private, kept out of git:` in their purpose text. Changes to existing files show diffs and keep other settings and comments; new files show full contents only with `--verbose`. The list covers `.brigada/project.yaml`, the harness wiring below, and any supporting migration or git exclude edit. Init never commits these files.
- Optional Codex setup text, when Codex is installed and wiring is enabled. Init prints the command to run yourself and writes no Codex configuration:

  ```sh
  codex mcp add brigada-foreman -- brigada mcp --foreman
  ```

  Codex has no verified project-scope registration, so this is user-level.
- `In the background:`, with the persistent runner, its log, `brigada stop`, and the engine's exit after 30 idle minutes, then a line saying `brigada config` shows every setting later.

Then, after you confirm (`y` at the prompt, or `--yes`), init:

- writes **`.brigada/project.yaml`** ([section 6](#6-configuring-a-project)). The file is yours to commit; brigada never commits or pushes anything. The write is refused if the file is a symlink or names another repository, and a file that changed after the plan was shown is not overwritten;
- **wires Claude Code** for [mode A](#2-your-own-session-is-the-foreman-mode-a), when `claude` is installed: the bridge in `.mcp.json`, only the `Stop` hook in private `.claude/settings.local.json`, and the foreman skill at `.claude/skills/brigada-foreman/SKILL.md`. A rerun removes brigada's old hooks from shared `.claude/settings.json`. Only brigada's entries change; the details, including the git exclude entry, are in [section 2](#2-your-own-session-is-the-foreman-mode-a);
- starts the engine on demand, imports the project file (recording where each value came from, [section 6](#6-configuring-a-project)), registers the foreman, saves the agent choices and starts one detached runner. If its agents changed, init restarts the runner with the new choices; otherwise it keeps the running one. The runner's log is `runner.log` in the state directory.

### Flags

| Flag | Meaning |
|---|---|
| `--probe` | Print the plan; write nothing and start nothing. |
| `--verbose` | Also print the full contents of new repository setup files. Diffs for existing files are always shown. |
| `--yes` | Apply the displayed plan without prompting. It does not confirm authentication. `--yes` and `--probe` together are a usage error. |
| `--agent SPEC` | Repeatable. The same syntax and defaults as `brigada runner --agent` ([section 8](#8-agents-quota-and-health)): `harness=<h>[,model=<m>][,classes=…][,account=<label>][,agent-file=<path>][,effort=<level>][,tools=…]`. Replaces the model and effort menus and Kimi's writing question. Use `classes=review` to keep Kimi reviewing only. |
| `--signed-in HARNESS` | Confirm an installed harness is signed in (repeatable or comma-separated). Local only. |
| `--foreman HARNESS/MODEL` | Choose a usable Claude or Codex agent as foreman; skips the foreman question. |
| `--check CMD` | The default check; answers step 3 without a question. |
| `--tools SCOPE` | Worker tools in runner `tools=` syntax (`Bash(npm test:*)+Bash(npx jest:*)`, or `none`). An explicit agent `tools=` wins; a `tools.claude/<model>` line in your config file is kept unless you pass `--tools`. |
| `--accept-limited` | Go ahead with fewer than two review families. |
| `--no-wiring` | Skip the harness wiring: no `.mcp.json`, hooks or skill, and no Codex text. |
| `--all` | Only in a folder that is not a repository ([section 3](#3-several-repositories-mode-b)). |
| `--state-dir DIR`, `--config PATH` | As for every command. |

### Without a terminal

Init still reads answers from standard input, one line per question, and prints each prompt on its own line. Supply answers only for questions that are asked, in this order:

1. For each installed CLI in order (`claude`, `codex`, `kimi`, `opencode`): its unconfirmed sign-in or use answer, then its model choice if you agreed to use it, then its effort choice when offered. After Kimi's model, answer whether it may write code too.
2. The foreman choice, if both Claude and Codex are usable and `--foreman` was not supplied.
3. The check, if `--check` was not given. Worker tools are asked only when the answer gives no valid scope and `--tools` was not given.
4. The final confirmation.

Enter picks model entry 1 for Claude, Codex and Kimi. Opencode defaults to `-`, so Enter leaves it out. Claude, Codex and opencode show `-` to skip; skip Kimi with no at `Use kimi?`. With `--agent`, there are no model or effort menus or Kimi writing question; sign-in or use questions may still be asked.

With `--yes` or `--probe` init asks nothing, so the agents must come from `--agent`, sign-in from the status command or `--signed-in`, and the check and tools from the project file, the first check candidate, or `--check` and `--tools`. If a question is needed and input is at end of file, init fails and names the flag that supplies the answer.

The final prompt says what will happen, for example `Write 4 files and start the runner? [y/N]`. Answering no exits 1 and starts nothing. Declining one harness's sign-in only skips that harness.

A probe without prompts or writes:

```sh
brigada init --probe --agent harness=claude,model=sonnet --agent harness=codex,model=gpt-5 --signed-in claude,codex
```

If init fails after writing `project.yaml`, it says `partial init` and names the step that failed; fix the error and rerun it. Init is safe to rerun.

### Changing your setup later

Run `brigada init` again to change agents, models or effort. Its menus default to the saved agent choices, including effort and whether Kimi may write code. Pick the foreman again from the offered Claude and Codex agents. The check step lists the existing `.brigada/project.yaml` check first, and Enter keeps it. Confirm the plan to apply changes. If the agents changed, init stops the detached runner and starts it with the new choices.

To change only the foreman, use `brigada project foreman --harness H --model M [--effort E]`. To change the check, edit `.brigada/project.yaml`, then run `brigada project import`.

After a reboot, run `brigada start`. It starts the engine and detached runner using the saved agents, without the init questions. It succeeds if the runner is already running; if there are no saved agents, it asks you to run `brigada init` first.

`brigada config` shows the saved agents and runner state, this repository's foreman and every project setting with its provenance, and all machine config-file keys and defaults. Each section says how to change it. Outside a registered repository it still shows agents and machine settings, with a hint to run inside a repository brigada knows. Project reads start the engine on demand. Use `--state-dir DIR` and `--config PATH` to inspect another setup.

### Stopping

`brigada stop` stops the detached runner. It applies to the whole state directory, not just the current repository. `brigada stop --engine` also stops the engine.

Stop checks the lock before signalling, sends SIGTERM, and waits up to 10 seconds for each process to release its lock; it reports failure if a process is still running. It clears stale runner records without signalling their old pids, and succeeds when nothing is running. Neither form starts an engine. Other commands start the engine again on demand; `brigada start` starts the runner again with the saved agents. `brigada config` shows whether it is running. Stop keeps the saved agent choices.

### Removing brigada from a repository

Run `brigada remove` inside the repository. It prints a removal plan, then asks `Remove brigada from this repository? [y/N]:`. Answering no leaves everything in place and prints `Nothing removed.`. The repository is identified by its `origin`, as with init.

The repository cleanup covers brigada's wiring:

- the `brigada` server in `.mcp.json`;
- the `Stop` hook (`brigada hold yield`) in `.claude/settings.local.json`, and the legacy brigada hooks in `.claude/settings.json`;
- `.claude/skills/brigada-foreman/SKILL.md`, only when it has brigada's marker;
- the `/.claude/settings.local.json` line in git's `info/exclude`.

Other settings and hooks stay. A JSON file is deleted only if removing brigada's entries leaves nothing else in it. `.brigada/project.yaml` stays unless you pass `--delete-config`.

| Flag | Meaning |
|---|---|
| `--yes` | Apply the displayed plan and stop an unused runner without prompting. |
| `--cancel` | Cancel live jobs and unfinished plans when removing the project. Without it, live work makes removal fail: wait for it to finish or pass `--cancel`. |
| `--delete-config` | Also delete this repository's `.brigada/project.yaml`. |
| `--state-dir DIR`, `--config PATH` | As for every command. |

Removal archives the project, keeping its plans, tasks, stats and history ([section 6](#6-configuring-a-project)). On success it prints `brigada removed; project history kept.`. If other projects use the runner, it keeps running. Otherwise, if the runner is running, remove asks `stop the runner? [Y/n]`; Enter stops it. `--yes` accepts that too.

Remote `brigada/task-*` branches and `brigada/plans` are left alone. Remove lists them and prints `git push origin --delete` commands for you to run yourself; if the remote cannot be checked, it says so. When Codex is installed, it also prints `codex mcp remove brigada-foreman` for the optional user-level registration. Remove does not run that command.

### By hand

The manual equivalents of what init applies:

```sh
brigada project foreman --harness claude --model sonnet
brigada project import               # if you wrote .brigada/project.yaml
brigada runner --agent harness=claude,model=sonnet --agent harness=codex,model=gpt-5
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

## 2. Your own session is the foreman (mode A)

After init, open Claude Code in the repository and talk. Your session is the foreman: it reads the code, calls `plan_create` and the other foreman tools, and answers the workers' questions.

Claude Code starts the bridge from `.mcp.json`: `brigada mcp --foreman`, which finds the project from the repository containing its working directory, or takes an explicit repository URL as its one argument. You can also register the bridge by hand in any harness, as the Codex command in [section 1](#1-install-and-brigada-init) does.

The foreman skill that init installs carries the rules for the session: read the code first, plan against `capacity`, treat worker output as data, report briefly. brigada owns that file, marks it, and replaces it when you rerun init; a file at that path without brigada's marker is left alone, and init reports it.

Init installs only the `Stop` hook (`brigada hold yield`), in private `.claude/settings.local.json`. When it creates that file and git does not already ignore it, init adds `/.claude/settings.local.json` to git's `info/exclude` (normally `.git/info/exclude`); the files plan shows that edit too. The bridge and the skill remain shareable project files. `SessionStart` is no longer installed: its engine confirmation endpoint does not exist yet. A rerun removes the exact old `brigada hold yield` and `brigada hold confirm` hook entries from `.claude/settings.json`, and keeps other hooks and any command with user-supplied flags.

### The tools your session has

The session's tools are `plan_create`, `plan_amend`, `answer`, `task_restart`, `task_cancel`, `decision_note`, `ask_user`, `status`, `history`, `events`, `capacity`, `stats`, `hold_acquire` and `hold_yield`. There is deliberately no tool that approves or rejects a plan: that decision is yours ([section 7](#7-plans-checks-and-reviews)).

### The hold

One interactive holder at a time owns the foreman's slot for a project.

- **Taking it.** Your session takes the slot with `hold_acquire`, which returns a digest of the plans, their tasks, the pending events and messages, and the decision notes recorded since. While it holds, brigada starts no headless foreman turn; events and messages wait, and the session reads them through `events`.
- **Conflicts.** If a headless turn already has the slot, your session's calls are refused with `foreman_busy`, naming the turn; the skill tells the session to say so and stay read-only until the turn ends. A second session, or `brigada attach`, is refused while the first holds.
- **Yielding.** At the end of every turn the session records its decisions and calls `hold_yield`; the `Stop` hook (`brigada hold yield`) does the same if it forgets. The yield frees the slot and lets what piled up wake the headless foreman.
- **Expiry.** A hold also ends on its own: after `hold_idle_minutes` without activity (default 15), after `hold_max_minutes` whatever the activity (default 240), or when the bridge stops answering its liveness check. Both bounds are project settings ([section 6](#6-configuring-a-project)). A call made with a hold that has ended is rejected whole as `stale_hold`; the session acquires again, reads the digest and decides afresh.

If a hold is stuck, release it yourself:

```sh
brigada hold release --repo <origin-url>
```

This revokes the hold's credential and re-evaluates the pending events for a wake.

`brigada hold yield` is the installed hook's command; you do not run it by hand. It always exits 0, so a failing hook never breaks your turn, and a failure is logged in the state directory. `brigada hold confirm` remains as a compatibility command for old `SessionStart` hooks; it changes nothing and always exits 0.

### Why decision notes

Two conversations act for one project: yours, and the headless foreman that wakes when work finishes while you are away. The headless turns run in a separate, brigada-owned session and read the engine, not your chat, so a preference or a reason that lives only in your chat is invisible to them.

That is why the session records such things with `decision_note` (up to 16384 bytes) before it yields. A note goes into the project's history as written by `foreman-interactive`, and the next headless turn receives it as data, never as an instruction from you. Your own words reach the foreman only as messages (`brigada ask`, [section 12](#12-fallback-attach-and-ask)). When you come back, the digest at `hold_acquire`, and `status`, `history` and `events`, bring your session up to date.

### What brigada enforces, and what your session decides

brigada enforces its own side:

- the hold's credential is limited to the one project, and cannot approve or reject plans or touch another project;
- the headless foreman turns run under a restricted scope: read-only file tools and brigada's tools, with no shell and no edits on `claude`, and a read-only sandbox on `codex` ([section 11](#11-the-foremans-harness-claude-or-codex));
- plans over the approval limit wait for you;
- checks and reviews run whatever the foreman believes.

Your interactive session runs under your own harness's permissions and edits as you do. brigada cannot stop your session from editing the repository; the skill tells it not to, and init widens none of your permissions.

Interactive turns are not charged to the foreman's wake budget ([section 4](#4-while-work-runs)); headless wakes are. If your own subscription hits its limit, the hold lapses and wakes go on with the foreman configured for the project.

## 3. Several repositories (mode B)

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
- `status` and `history`: across every project, or one with `repo_url`; `status` shows each plan awaiting approval in full, with its id and version;
- `plan_approve` and `plan_reject`: your decision on a plan over the approval limit, naming the project, the plan id and the **plan version you were shown**. A plan amended since is refused, and the coordinator must read `status` and show you the plan again. These tools act for you: the coordinator is told to call them only after you said an explicit yes or no in the chat.

The coordinator never plans and never edits a task graph: it has no `plan_create`, `plan_amend` or task tool. Each project's own foreman plans.

Out of scope: a plan whose tasks span repositories (a plan belongs to one project, and each task runs in that repository), and a foreman that knows another repository's work. For a change that touches two repositories, the coordinator sends each project its own request, and you carry the coordination, such as merging in order.

`brigada project list` prints one line per project.

## 4. While work runs

`brigada status` prints:

- the engine (pid, uptime) and the job counts by status;
- the jobs a runner holds right now, with the worker's last move;
- any agents cooling down;
- the plans with their tasks, and the foreman's latest report;
- how many messages wait for the foreman;
- the **needs you** queue.

`brigada status --line` prints one short line for a terminal status bar; it never starts the engine.

The needs-you queue lists what only you (or the foreman) can unblock:

| Entry | What it wants |
|---|---|
| `plan #N pending_approval: <why>` | Your decision: `brigada plan approve N` or `brigada plan reject N` ([section 7](#7-plans-checks-and-reviews)), or the coordinator's tools ([section 3](#3-several-repositories-mode-b)). |
| `task #N needs_foreman: <reason>` | Judgment: the worker asked a question, the check failed with no rounds left, a review wanted changes, or the task failed. The foreman is woken for these; the entry is there so you can see it and step in. |
| `task #N needs_user: <reason>` | An answer from you, for a task parked for you directly. |
| `foreman #N limited: <until>` | A choice: wait, switch the foreman, or let approved plans run without it ([section 8](#8-agents-quota-and-health)). |
| `foreman #N budget_spent: wake budget spent (20/20 today, resets <UTC time>); <n> events waiting` | Raise the day's wake budget with `brigada budget --wakes N`, or send `brigada ask`: a wake that carries your message is not charged. |
| `agent <harness>/<model> broken: <reason>` | A fix, then `brigada agent reset <harness>/<model>` ([section 8](#8-agents-quota-and-health)). |

### The wake budget

The foreman is woken headless on events: a plan done, a task parked, a question. Those wakes are limited per project per UTC day, 20 by default, so a loop cannot spend your subscription unseen.

- `brigada budget` shows the budget, today's use, the reset time and how many events wait.
- `brigada budget --wakes N` (1 to 1000) sets the budget for the project, and at once wakes whatever the spent budget had parked.
- `--repo URL` names the project; without it, the engine's only project is used.
- A message you send with `brigada ask` goes ahead even when the budget is spent.

The default for projects that have not set a budget comes from `foreman_wakes_per_day` in the config file ([section 13](#13-configuration-reference)).

When `notify` is `auto` (the default), the engine also raises a desktop notification for the same kinds of moments: a plan done, a plan waiting for approval, a task parked, the foreman's report, a limited foreman, a spent wake budget, a broken agent.

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
5. `done`.

Off this path are `needs_foreman` and `needs_user` (parked, see the queue above), `failed`, and `cancelled`.

Jobs have their own statuses, the ones in the `jobs:` line of status: `queued`, `leased`, `running`, `done`, `failed`, `cancelled`, `stalled`. A worker that makes no progress for ten minutes is killed and its job ends `stalled`; the task goes to the foreman.

### Cancelling

`brigada cancel <job-id>` stops one live job: a worker, a review, or a foreman turn. The runner learns of it on its next heartbeat.

It cancels that job only: not the task, not the plan, and it is not an approval of anything. To drop a whole plan that is waiting for approval, use `plan reject`. To drop tasks of a running plan, ask the foreman (it has `task_cancel`).

Job ids are not task ids. `status` shows a running job as `task #N …`; find its job id with `brigada history --task N`.

## 5. Finishing: `brigada land`

Each task's work is a branch on your remote. Merging and pushing stay yours. `brigada land` does the local part:

```sh
brigada land <task-id>... [--onto BRANCH] [--repo URL] [--landed ID]... [--no-check]
```

Land puts approved work into your own repository, on the checked-out branch: the project's default branch, or the one named with `--onto`. It never switches branches.

- Each task becomes one commit holding only its own diff against its own base, so a stacked task lands as just its work.
- Tasks go in dependency order. A dependency must be in the same call, already landed (found by its `Brigada-Task:` trailer), or named with `--landed`.
- The commit message is built from the task's title, spec and the worker's summary, with `Brigada-Task`, `Brigada-Branch` and `Co-Authored-By` trailers.
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

One limit: the runner does not yet record the head commit the reviewer approved, so land cannot compare it with the branch's head. It prints a warning per task saying the head was not checked against what was reviewed. Look at `brigada diff <task-id>` before you push.

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
| `default_check` | command | none | The check a task runs when its spec names none, and the one `brigada land` reruns. |
| `worker_tools` | `+`-separated Claude Code tool patterns, or `none` | the agent's own scope | The shell scope of Claude workers on this project ([section 8](#8-agents-quota-and-health)). |
| `approval_max_tasks` | integer | `5` | A plan with more tasks waits for you. `0`: every plan waits. Negative: no limit. |
| `approval_classes` | list of class names | `implement`, `review`, `answer` | A task of a class outside the list holds the plan. |
| `approval_protected_paths` | list of repo-relative globs | none | A task whose spec or check mentions a matching path holds the plan. |
| `setup` | command | none | The environment step run before the worker and the check ([section 7](#7-plans-checks-and-reviews)). |
| `setup_cache_key_files` | list of repo-relative files | none (no cache) | Files whose contents key the setup cache. |
| `setup_timeout_minutes` | positive integer | `15` | How long `setup` may run. |
| `extra_repos` | list of `"name repo ref"` strings | none | Other repositories checked out read-only beside every task ([section 7](#7-plans-checks-and-reviews)). |
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
| `hold_idle_minutes` | `15` | The interactive hold's idle bound, in minutes ([section 2](#2-your-own-session-is-the-foreman-mode-a)). |
| `hold_max_minutes` | `240` | The interactive hold's absolute bound, in minutes. |
| `foreman_wakes_per_day` | `20`, or the config file's value | The daily wake budget, 1 to 1000 ([section 4](#4-while-work-runs); also `brigada budget --wakes N`). |
| `instructions` | none | A local override of the instruction text. |

```sh
brigada project set [--repo URL] key=value ...
brigada project show [--repo URL]
```

- `project set` accepts every key in the two tables above except `schema`, `repo` and `instructions_file`, so a local value can also override an imported one.
- Flags go before the `key=value` pairs.
- On the command line, list values are joined with `+`: `approval_classes=implement+review`, `approval_protected_paths=infra/**+db/*.sql`.
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

The project's foreman is chosen with `brigada project foreman [--repo URL] [--harness H --model M [--effort E]]` ([section 11](#11-the-foremans-harness-claude-or-codex)). An agent's `effort` and, for Claude, `tools` are set on the runner's `--agent` ([section 8](#8-agents-quota-and-health)), with `tools.claude/<model>` in the config file as the per-model default ([section 13](#13-configuration-reference)).

## 7. Plans, checks and reviews

You ask; the foreman turns the ask into a **plan**: a set of tasks, each with a spec for the worker, a check command, a class (`implement` by default, also `review` and `answer`), and a list of the tasks it depends on.

- A task with no dependencies is queued at once.
- A task with dependencies waits (`pending`) until all of them are done. If one of them fails or is cancelled, the dependents keep waiting until the foreman re-points them to another task or drops the dependency.
- The foreman can amend a live plan: add tasks, cancel tasks, re-point dependencies.
- A plan is `done` when no live work is left. That is not the same as everything succeeding: the plan's done report names the tasks that failed, that were cancelled without your approval, and that never ran.

### The approval limit

A plan starts on its own unless it trips one of three triggers, each set per project ([section 6](#6-configuring-a-project)):

- more tasks than `approval_max_tasks` (default 5);
- a task class outside `approval_classes` (default `implement`, `review`, `answer`);
- a task whose spec or check mentions a path that matches one of `approval_protected_paths` (default none).

A plan that trips one waits in `pending_approval`, and nothing is queued. The needs-you entry gives the reasons, for example `7 tasks, over the limit of 5`. An amendment that adds tasks is checked against the limit again. You decide:

```sh
brigada plan approve 7    # queues the tasks; prints each task's job
brigada plan reject 7     # cancels the plan's tasks
```

or through the coordinator's `plan_approve` and `plan_reject` ([section 3](#3-several-repositories-mode-b)), which name the plan version you were shown. Only you can decide: no foreman tool approves a plan.

### The plan file

Every change to a plan is mirrored to `.brigada/plans/<plan-id>.yaml` and pushed to the `brigada/plans` branch of your remote, not to your default branch. The file holds the graph, the task states, the branches and the open questions, but no transcripts.

The store is the authority, and the file is a copy that may lag: a push that fails is retried on the next change. The file exists so that `brigada recover` can rebuild plans after a lost store ([section 10](#10-backups-and-recovery)), and so you can see a plan from another machine.

### Checks

A task's spec names a check command. After the worker reports done, the runner runs the check in the task's worktree. The exit code and the tail of the output are the result; the worker's own claim that tests pass is not used. A check that runs longer than 10 minutes is killed and counts as failed. A task may carry no check at all, and then nothing is run.

If the check of an `implement` task fails and fix rounds are left, the worker gets the check's output and fixes on its own session. A failed check on a task of any other class gets no fix round: the task parks at `needs_foreman`.

### An environment for checks and reviews

A worktree is a fresh clone, with no `node_modules` or virtualenv. Set a `setup` command in `.brigada/project.yaml` (or with `brigada project set`), and the runner runs it in the worktree before the worker starts and again before the check. It runs as the runner itself, not under the worker's tool scope. A review job gets the same environment before the reviewer starts.

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

After an `implement` task's check passes, a reviewer reads the diff, the spec and the check result, never the author's summary. The reviewer is an agent whose model family differs from the author's, so with a single family among your agents no review can run ([section 1](#1-install-and-brigada-init), step 1).

An approve counts only with substantive findings text (80 characters or more). A thinner approve, or a review that ends without a verdict, is retried once with another reviewer, and then the task waits for the foreman. Tasks of other classes finish when the run does, without a review.

### Rounds

A review that asks for changes, or a failed check, starts a fix round on the author's session with the findings as feedback. Fix rounds for failed checks and for review changes draw on one shared budget: by default two fix rounds in all per attempt, plus two restarts. When a cap is spent, the task parks at `needs_foreman` and the entry says which cap ran out; the foreman decides whether to re-plan, switch agent, split the task or ask you.

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
| `harness` | `claude`, `kimi`, `codex` or `opencode`. |
| `model` | Defaults to `sonnet` for claude, and to the CLI's own default for the others. For opencode, a model is `provider/model`. |
| `classes` | A `+`-separated list; default `implement+review+answer`. `classes=none` advertises no task classes (foreman only). |
| `account` | The login whose quota the agent spends; defaults to the harness name (see [Accounts](#accounts)). |
| `agent-file=<path>` | kimi only. Starts the session under your own agent definition instead of the runner's review definition. |
| `effort=<level>` | Validated per harness (see [Effort](#effort)). |
| `tools=<p1>+<p2>` | claude only (see [Worker tools (claude)](#worker-tools-claude)). |

Without `--agent`, the shorthand flags `--harness`, `--model`, `--classes` and `--agent-file` describe one agent; there `--classes` is comma-separated (`implement,review`). The two forms cannot be mixed. Other flags:

- `--labels`: the labels the runner offers, comma-separated;
- `--max-parallel N`: how many concurrent jobs this runner runs; unset (`0`) means one at a time;
- `--no-start`: fail instead of starting the engine.

`kimi` and `opencode` are worker-only; the foreman can be `claude` or `codex`.

**Kimi reviews only by default in interactive init.** After its model choice, init asks whether it may write code too. Enter or no keeps `classes=review`; yes enables `implement+review+answer`. With `--agent` there is no such question: set `classes=review` explicitly for a review-only Kimi agent. Omitting `classes` keeps the runner's default of all three classes.

The runner writes a read-only Kimi agent definition to `<state-dir>/runner/kimi-readonly.md`. It allows only `Read`, `Grep` and `Glob`, with no `Bash`. Kimi reviews without their own `agent-file=` use this definition. A custom `agent-file=` takes precedence, and a resumed session keeps the definition it started with.

Letting Kimi write code is an opt-in. Init warns that Kimi has no sandbox: when writing code it can run any command on this machine without asking. Work and fix jobs use Kimi's normal agent, or your `agent-file=`, rather than the runner's read-only definition.

**Keep the runner running.** A job nobody leases within ten minutes fails. The ten minutes do not count while every runner that could take the job is full, or while every agent that qualifies for it is cooling after a usage limit (a fix round is pinned to its author's agent). `brigada status` shows such a job as held: waiting for a runner slot, or `waiting: <harness>/<model> cooling until <time>`. A job pinned to a broken agent still fails after ten minutes, and the failure names the agent and its state.

### Effort

`effort=<level>` on an agent sets how hard its CLI reasons; leave it out for the CLI's own default. brigada accepts these levels per harness and passes each one through one fixed flag, never a raw argument:

| Harness | Accepted levels | Passed as |
|---|---|---|
| `claude` | `low`, `medium`, `high`, `xhigh`, `max` | `--effort <level>` (fresh, resume and attach) |
| `codex` | `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra` | `-c model_reasoning_effort="<level>"` |
| `opencode` | `minimal`, `high`, `max` | `--variant <level>` |
| `kimi` | none: its CLI documents no effort option, so `effort=` is refused | — |

The lists are brigada's accepted values, not a promise that every model supports every one: the CLI judges that. For codex the key is model-dependent. For opencode a variant is provider- and model-specific, and the three listed are the ones its help names, not every variant that exists. Any other level is a usage error at startup.

A task can set `low`, `medium`, `high` or `max` effort per job. Claude and Codex use the same word; opencode maps `low` to `minimal`, `medium` to no variant, and keeps `high` and `max`; Kimi ignores task effort. An unspecified mapped level uses the agent's own effort, else the CLI default. Each job records the level passed.

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
- `codex`, `kimi` and `opencode` refuse `tools=`. Codex workers run in the `workspace-write` sandbox. Kimi reviews use the read-only agent definition above unless you supply `agent-file=`; allowing it to write code also allows commands without asking. Opencode permits edits and denies bash headless by default.
- `brigada runner` prints each claude agent's scope at startup, and the foreman's capacity read shows it as `tools=` on the agent.

### Accounts

An agent's account is the login whose quota it spends. The account label defaults to the harness name, which is right when each CLI has one login. If you have a second login for a CLI, offer it as another runner's agent with `account=<label>`, so that the two logins keep separate quotas, caps and cooling.

### At a vendor limit

**Workers.** A worker that hits its subscription limit cools the account until the reset time the CLI reported, or for 30 minutes when it gave none. Cooling shows on the `cooling:` line of status, not as a queue entry: `cooling: <harness>/<model> until <time> (account <label>)`. Meanwhile the dispatcher skips every agent on that account, and the task that hit the limit gets the [automatic retry](#automatic-retry) below. Cooling clears when the time passes, or at once with `brigada agent reset <harness>/<model>` if you know the limit is lifted.

**The foreman** is separate. Only a limit error on a foreman's own turn marks it limited, shown as `foreman #N limited: …` in the queue, and no new turns are started on it. When the error gave a reset time, the mark clears itself then. When it gave none (`limited, reset time unknown`), waiting never clears it: only changing the foreman does. You choose:

- wait for the reset (only when a time was given);
- switch: `brigada project foreman --repo <origin-url> --harness <h> --model <m>` (a different harness or model than the current pair), which clears the limit and wakes the new foreman for what waited;
- or do nothing: approved plans keep running without it, since the routine rules (dependencies, fix rounds, retries, reviews) do not need the foreman, but anything that needs judgment waits.

### Automatic retry

When a worker's CLI fails for a transient reason (a quota error, an authentication error, a lost runner), a task's first work job gets one automatic restart on another ready agent, with a fresh session and a new branch, carrying the failed job's last progress. Failures in fix rounds and reviews are not restarted this way. If no other agent can take it, or the restart was already used, the task parks for the foreman. Set `task_auto_retry = false` to park such failures at once.

### Broken agents

An agent is taken out of dispatch as broken when:

1. its CLI reports an authentication failure;
2. its last two jobs both failed (a limit hit is quota, not a failure, and does not count); or
3. its runner has not been heard from for 15 minutes.

The first two stay broken until you fix the cause (log the CLI in again, fix what made it fail) and run `brigada agent reset <harness>/<model>`. The third is not a verdict on the agent: the agent comes back by itself as soon as its runner polls again.

Broken agents are listed in the needs-you queue with the reason, and raise a notification. `agent reset` prints how many agents matched. The name is the `harness/model` pair that `status` and `stats` print, and the reset applies to every runner's copy of that agent.

### Capacity

Caps bound how many jobs run at once, in layers: this runner, the account, the model, the plan and the whole engine (`caps.*` keys, [section 13](#13-configuration-reference)). A job starts only when every layer that applies has room. A task blocked only by full caps waits in the queue (it is not parked and nobody is notified) and starts when a slot frees.

A foreman turn is never held back by the plan or model caps, and goes ahead of queued task jobs. While it runs it still counts as use of its account, runner, global and model caps, so it can leave a worker on the same model waiting.

The foreman can look at the capacity it has when it plans (per agent: the tier, the cap, the running jobs, the quota state and the cost weight), but the engine enforces the caps whatever the foreman believes. `cost.<harness>/<model>` is a relative weight, default 1, that the foreman sees when it decides which tasks can wait for a cheaper agent.

Each agent's tier comes from the model name: an `opus` segment prefix or a whole `pro`, `max` or `ultra` segment is heavy; a `haiku` segment prefix or a whole `mini`, `nano`, `flash`, `lite`, `highspeed` or `free` segment is light; other names and CLI defaults are standard. Names are matched without provider prefixes or context suffixes such as `[1m]`, ignoring case. Heavy matches take precedence. Use `tier.<harness>/<model>` to correct the label. The runner's startup line shows the table's tier; stats and capacity include the engine's override.

### Headroom

With `headroom = true` (the default), task workers on the account that serves your foreman's harness see that account's cap lowered by one (never below zero), so a slot stays free for the foreman. This is about parallel slots; it does not stop a vendor quota from running out. It acts only where a `caps.account.<label>` cap is set; with a cap of 1 on that account, task workers do not run there at all. Set `headroom = false` to let workers use every slot.

## 9. The record and stats

`brigada stats` prints one table per repo:

```text
AGENT  TIER  CLASS  ATTEMPTS  SUCCESSES  RATE  CONFIDENCE
```

- **Tier** is the model-name table label (`light`, `standard` or `heavy`), corrected by any `tier.<harness>/<model>` config override. It is informational today; it does not change dispatch. Changing the label keeps the agent’s existing outcome record.
- **Attempts and successes** are counted when a task ends, for the agent that wrote the final change, in the task's class.
  - A success is a task whose check passed and, for `implement` tasks, whose review approved within the allowed rounds. Tasks of other classes succeed when the run is done with its check passed, with no review.
  - A task that runs out of rounds counts an attempt without a success.
  - A reviewer's substantive approve counts as an attempt and a success in the `review` class for the reviewer's agent.
  - When the foreman's plan finishes, it is counted in the class `plan` for the foreman's agent, as a success when every task was done (or cancelled with your approval). This is measured only and does not steer dispatch.
- **Rate** is the score the dispatcher ranks by: `(successes + 0.5) / (attempts + 1)`. The prior is one pseudo-trial at 50%, so an agent with no record scores 0.50 and a single success reads 0.75, not 1.00. More attempts pull the rate toward what actually happened.
- **Confidence** is a word for the attempts behind the score: `none` for 0, `starting` for 1 to 4, `enough` from 5 on.
- An agent whose runner has not been seen lately is marked `(stale)`; a registered agent with no record yet shows at 0 attempts.

Read it by class. For each task, the dispatcher picks the agent with the best score among those that are ready, have quota and have room under the caps, and sends about one task in ten to the next best, so that an agent with a thin record can be measured.

Two limits today:

- the ranking is among the agents of the runner that is asking for work;
- ordinary work and fix jobs are ranked on the `implement` score whatever the task's class, so scores in other classes do not yet steer how those tasks are dispatched (reviews are ranked on `review`).

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

Only `claude` and `codex` can be a foreman; `kimi` and `opencode` are worker-only. Init chooses one ([section 1](#1-install-and-brigada-init)). To change it, run `brigada project foreman --repo <origin-url> --harness <h> --model <m>`; inside the repo, `--repo` can be left out. Without `--harness` and `--model`, the command shows the current choice.

This section is about the **headless** foreman, the brigada-owned session that wakes on events. Your own session in mode A is whatever harness you open ([section 2](#2-your-own-session-is-the-foreman-mode-a)).

### Effort and model

**Effort.** Add `--effort <level>` to choose the foreman's reasoning effort: for `claude`, `low`, `medium`, `high`, `xhigh`, `max`; for `codex`, `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`, `ultra` (the levels are explained in [section 8](#effort)). It goes with `--harness` and `--model`, and choosing again without `--effort` clears it. `project foreman --repo` shows it as `effort=<level>`. It applies to headless turns and to attach, and overrides the effort of the runner agent that serves the turn; workers and reviewers use the task's effort when set, otherwise their agent's effort.

**The model.** A headless turn runs on the harness and model you chose. The engine leases it only to a runner agent that offers exactly that `--harness`/`--model` pair, and the turn runs on that agent's adapter and model (without `--effort`, at that agent's own effort).

- If no registered agent offers the pair, the turn is not handed to another model. It waits, `brigada status` shows `foreman turn waiting: no runner offers <harness>/<model>`, and it does not expire in the queue while it waits.
- `brigada project foreman` warns when you choose a pair no registered agent currently offers, and prints the runner flag that would serve it (`--agent harness=<harness>,model=<model>`). Registering the pair releases the waiting turn.
- An agent that offers the pair but is busy, cooling or broken is a different wait, and does not trigger the warning.
- A turn already queued when you switch keeps the pair it was queued with.

### Reading the code

Headless turns and attach refresh `<state>/runner/foreman/<project>/repo` from the project's repo URL at its configured default branch. The opening prompt names the checkout's absolute path, branch and commit. The foreman reads the code and its CLAUDE.md/AGENTS.md before planning; the snapshot may lag the remote by one fetch. A clone or fetch failure fails the turn (or attach) with the git reason. The session's working directory stays the parent scratch directory, so existing sessions continue to resume.

### What differs between claude and codex

- **claude:** `--add-dir` grants access to the checkout, and additional-directory CLAUDE.md loading is enabled. The foreman may read files (`Read`, `Grep`, `Glob`) and call brigada's tools; the shell and every editing tool (`Bash`, `Edit`, `NotebookEdit`, `Write`) remain denied.
- **codex:** the checkout is named in the prompt, and the foreman runs with `-s read-only`. Independent config overrides keep automatic review for MCP approvals and reject sandbox and permission escalations. The `--approve-for-me` alias cannot be used here: it selects `workspace-write`, which would make the scratch's repo writable. Codex's filesystem sandbox, not instructions, is what protects the checkout from writes. Unlike claude with its explicit tool denials, codex still has shell tools.

### Codex exec approvals

The optional `codex mcp add brigada-foreman -- brigada mcp --foreman` registration from [section 1](#1-install-and-brigada-init) is user-level, across projects. For your own `codex exec` session, the approval settings are likewise user-level Codex configuration, not entries in `.brigada/project.yaml`, and init does not write them. The headless foreman's adapter supplies exactly these settings as `-c` overrides:

```toml
approvals_reviewer = "auto_review"
approval_policy = { granular = { sandbox_approval = false, rules = false, mcp_elicitations = true, request_permissions = false, skill_approval = false } }
```

These settings auto-approve brigada's own tool calls in `codex exec`; they do not approve a brigada plan on your behalf.

Under `mcp_servers.<name>` the adapter emits only `command`, `args` and `env`; it does not set `mcp_servers.<name>.default_tools_approval_mode`. The headless foreman's read-only sandbox is set separately from these approval settings.

### Switching

Running `project foreman` again with another harness starts a fresh foreman session: a session belongs to one CLI, so the old one is dropped, and the new foreman rebuilds what it needs from the engine (the plans, the history, the pending events). A change of model or of effort on the same harness keeps the session. Switching also clears a foreman limit, as in [section 8](#at-a-vendor-limit). Nothing about plans or tasks is lost.

## 12. Fallback: attach and ask

If your harness cannot host brigada's tools (Codex has no verified project scope, and other harnesses have no wiring), you can still talk to the headless foreman.

### Attach

**Attach** is a conversation with the headless foreman. Run `brigada` (or `brigada attach`) inside the repo: the repo's `origin` URL names the project, and you are put into the foreman's own session in your terminal, in brigada's scratch directory.

- **One at a time.** Only one chat can be attached at a time. If the foreman is in the middle of a headless turn, attach waits for it to finish; `brigada attach --force` cancels that turn instead.
- **Your session's hold.** If your own harness session holds the foreman ([section 2](#2-your-own-session-is-the-foreman-mode-a)), attach refuses and names the hold. Wait for the session to yield or the hold to lapse, or release it with `brigada hold release --repo <origin-url>`.
- **While attached**, the engine starts no headless turns.
- **What the foreman sees.** Its opening prompt carries the events that piled up while you were away (finished plans, parked tasks, questions) and any waiting messages; it reads new ones through its `events` tool as they arrive.
- **When you leave**, the engine records where the session ended so the next turn continues it, and anything the foreman did not read is looked at again for a wake-up.
- **No foreman yet.** If the project has none, attach tells you the `brigada project foreman` command to run first.

With `claude`, an attached foreman is the same session the headless turns resume, so the next turn continues your chat. The first attach shows Claude Code's own "trust this folder" prompt for the foreman's working directory under the state directory: choose yes. It is brigada's scratch folder, with its reading checkout under `repo`.

**The Codex limits.** An attached `codex` session is Codex's own terminal interface. It does not carry brigada's tools, and Codex does not expose the thread in a form brigada can write back, so a later headless turn may start a fresh Codex thread instead of continuing the chat.

### Ask

**Ask** is a message. `brigada ask "fix the flaky auth test; the check is: go test ./auth/..."` leaves your words in the foreman's inbox and returns at once with one line that says what happened: the foreman was woken, or why the message waits (you or your session hold it, a turn is already running, the project has no foreman, the foreman is limited, the wake budget is spent). Use it for a request you do not need to discuss.

With `-` as the only argument, ask reads the message from standard input:

```sh
git diff main | brigada ask -
```

- `--repo <origin-url>` names the project. Without it, ask works only while the engine has exactly one project; with more it refuses, and `brigada project list` shows the URLs.
- A message you send is not charged against the foreman's daily wake budget ([section 4](#4-while-work-runs)): a wake that carries your message goes ahead even when that limit is spent.

### From another harness

`brigada mcp --client` is an MCP server over stdio for a harness that should act for you without hosting the foreman. It offers `ask_foreman` (`text`, and `repo_url` when the engine knows several projects), `status` and `history`. Register it with, for example:

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
| `snapshot_dir` | absolute path | none (no snapshots) | Where the daily store snapshots go; the last seven are kept ([section 10](#10-backups-and-recovery)). |
| `headroom` | `true` or `false` | `true` | Keep one slot free for the foreman on its account ([section 8](#headroom)). |
| `task_auto_retry` | `true` or `false` | `true` | One automatic restart on another agent after a transient worker failure ([section 8](#automatic-retry)). |
| `foreman_wakes_per_day` | integer, 1 to 1000 | `20` | Default daily foreman wake-ups per project ([section 4](#the-wake-budget)). `brigada budget --wakes N` sets it per project in the store. |
| `default_reviewer` | harness name | none | The harness that takes a standalone review when the branch's author family cannot be told ([section 7](#a-branch-with-no-plan)). |
| `caps.global` | integer, 0 or more | no cap | Most parallel jobs in the whole engine. |
| `caps.plan` | integer, 0 or more | no cap | Most parallel tasks per plan. |
| `caps.runner` | integer, 0 or more | no cap | Most parallel jobs per runner. A runner's own `--max-parallel` also applies, and the smaller wins. |
| `caps.account.<label>` | integer, 0 or more | no cap | Most parallel jobs on one account label ([section 8](#capacity)). |
| `caps.model.<model>` | integer, 0 or more | no cap | Most parallel jobs on one model. |
| `cost.<harness>/<model>` | integer, 0 or more | `1` | Relative cost weight of an agent, shown to the foreman. |
| `tier.<harness>/<model>` | `light`, `standard` or `heavy` | from the model name | Override the tier for every agent of this harness/model, shown in stats and capacity. |
| `tools.claude/<model>` | `+`-separated Claude Code tool patterns, or `none` | the Go scope | Default shell scope of the claude worker agent on that model; `--agent ...,tools=` overrides it ([section 8](#worker-tools-claude)). Other harnesses are refused. |

A cap of `0` caps the layer at nothing, which is different from leaving it out.

Project settings (the approval limit, `default_check`, `setup`, the hold bounds and the rest) are not config-file keys, and the config file refuses them as unknown; they are project settings ([section 6](#6-configuring-a-project)). Effort is not a config-file key either: it is set per agent with `brigada runner --agent …,effort=<level>` ([section 8](#effort)) and per project foreman with `brigada project foreman --effort <level>` ([section 11](#11-the-foremans-harness-claude-or-codex)). `cost.*` and `tier.*` keys name `harness/model` without an effort, including a provider prefix when the model has one.

An example:

```text
notify = none
snapshot_dir = /home/you/backups/brigada
caps.global = 6
caps.plan = 3
caps.account.claude = 3
caps.account.codex = 2
cost.claude/opus = 2
```

### Project settings

Every key that `brigada project set` and `project show` know, with each default as `project show` prints it:

| Key | Default | In `project.yaml`? |
|---|---|---|
| `approval_max_tasks` | `5` | yes |
| `approval_classes` | `implement+review+answer` | yes |
| `approval_protected_paths` | `none` | yes |
| `default_check` | `none` | yes |
| `worker_tools` | `agent default` | yes |
| `setup` | `none` | yes |
| `setup_cache_key_files` | `none` | yes |
| `setup_timeout_minutes` | `15` | yes |
| `extra_repos` | `none` | yes |
| `hold_idle_minutes` | `15` | no, local only |
| `hold_max_minutes` | `240` | no, local only |
| `foreman_wakes_per_day` | `20`, or the config file's value | no, local only |
| `instructions` | `none` | no, local only (the file's counterpart is `instructions_file`) |

## 14. Command reference

`brigada help` prints the command list. `brigada <command> --help` (also `-h` or `-help`) prints a command's usage, description and flags; `brigada help <command> [<subcommand>]` prints the same help. For the two-level commands `project`, `agent`, `plan`, `hold` and `task`, `--help` or `help` lists the subcommands; use, for example, `brigada project foreman --help` for a subcommand's flags. Help exits 0. A usage error names the relevant help command on stderr.

A command's flags go after its name and before its positional arguments (`diff`, `logs` and `land` also accept flags after their ids). Every executable command also takes `--state-dir DIR` and `--config PATH`, and most client commands take `--no-start` (fail instead of starting the engine); the synopses below leave these out. Exit codes: 0 ok, 1 error, 2 usage. Init's setup refusals exit 1, and the hold hooks always exit 0.

### Starting

- `brigada init [--probe | --yes] [--verbose] [--agent SPEC]... [--signed-in HARNESS]... [--foreman HARNESS/MODEL] [--check CMD] [--tools SCOPE] [--accept-limited] [--no-wiring] [--all]`: set up one repository, or, in a folder that is not a repository, the projects under it. See [sections 1](#1-install-and-brigada-init) and [3](#3-several-repositories-mode-b).
- `brigada start`: start the engine and detached runner with the agents saved by init; succeeds when the runner is already running. Use it after a reboot or `brigada stop`. See [section 1](#changing-your-setup-later).
- `brigada runner [--agent SPEC]... [--labels LABELS] [--max-parallel N]`, where SPEC is `harness=<h>[,model=<m>][,classes=<c1>+<c2>|none][,account=<label>][,agent-file=<path>][,effort=<level>][,tools=<p1>+<p2>|none]`: run a runner that leases jobs, runs workers and checks, and pushes branches. See [section 8](#8-agents-quota-and-health).
- `brigada stop [--engine]`: stop the detached runner in the selected state directory; `--engine` also stops the engine. Sends SIGTERM and waits up to 10 seconds per process; succeeds if nothing is running, and never starts the engine. See [section 1](#stopping).
- `brigada remove [--yes] [--cancel] [--delete-config]`: remove brigada from this repository, keeping its project history. `--yes` applies the displayed plan and stops an unused runner without prompting; `--cancel` cancels live jobs and unfinished plans; `--delete-config` also deletes `.brigada/project.yaml`. See [section 1](#removing-brigada-from-a-repository).
- `brigada mcp (--foreman [URL] | --coordinator | --client)`: serve brigada's tools over stdio. `--foreman` is the mode A bridge of your own session ([section 2](#2-your-own-session-is-the-foreman-mode-a)), `--coordinator` the mode B tools ([section 3](#3-several-repositories-mode-b)), `--client` the tools of [section 12](#from-another-harness). Only one selector at a time. Claude Code runs it from the wiring init writes. Without a selector, `--socket PATH` is the bridge a headless foreman session uses; you do not run that by hand.

### Talking

- `brigada`, `brigada attach [--force]`: chat with this repo's headless foreman. See [section 12](#attach).
- `brigada ask [--repo URL] <message...>` (or `-` for standard input): send the foreman a message without attaching. See [section 12](#ask).
- `brigada hold release --repo URL`: release a stuck interactive hold. `brigada hold yield` is the Stop hook's command; `brigada hold confirm` remains for compatibility with old SessionStart hooks. See [section 2](#the-hold).

### Watching

- `brigada config`: show agents, runner state, foreman, project settings with provenance, and machine config-file values and defaults, with their locations and commands to change them. See [section 1](#changing-your-setup-later).
- `brigada status [--line]`: the engine, jobs, plans and the needs-you queue. See [section 4](#4-while-work-runs).
- `brigada history [--task N] [--job N] [--limit N]`: the timeline. See [section 4](#history-diffs-and-logs).
- `brigada diff <task-id> [--stat] [--repo URL]`: what a task changed, as `git diff base...branch`; run inside the repo. See [section 4](#history-diffs-and-logs).
- `brigada logs <job-id> [--check] [--tail N] [--follow]`, or `brigada logs --task N`: a job's transcript as readable lines, or its check log. See [section 4](#history-diffs-and-logs).
- `brigada stats`: tier, attempts, successes, rate and confidence per agent, class and repo. See [section 9](#9-the-record-and-stats).

### Finishing

- `brigada land <task-id>... [--onto BRANCH] [--repo URL] [--landed ID]... [--no-check]`: squash approved tasks onto the checked-out branch, one commit each, in dependency order; rerun the default check; print the push command and stop. See [section 5](#5-finishing-brigada-land).

### Deciding

- `brigada plan approve <id>`, `brigada plan reject <id>`: your answer on a plan over the approval limit. See [section 7](#the-approval-limit).
- `brigada budget [--repo URL] [--wakes N]`: show the project's foreman wake budget, with today's use and the UTC reset time; `--wakes N` (1 to 1000) sets it. See [section 4](#the-wake-budget).
- `brigada agent reset <harness>/<model>`: clear a broken or cooling agent. See [section 8](#broken-agents).
- `brigada cancel <job-id>`: stop one live job. See [section 4](#cancelling).
- `brigada review --branch <name> [--repo URL]`: review a branch with an agent of another family, with no plan. See [section 7](#a-branch-with-no-plan).
- `brigada task submit --repo URL --spec FILE|- [--base REF] [--check CMD] [--title T] [--harness H]`: submit one task directly, without the foreman. A debugging path; the base ref defaults to `main`.

### Projects

- `brigada project foreman [--repo URL] [--harness H --model M [--effort E]]`: show the repo's foreman (`claude` or `codex`) and its effort, or set them with `--harness` and `--model`; `--repo` defaults to the origin of the working directory. See [section 11](#11-the-foremans-harness-claude-or-codex).
- `brigada project list`: every project with its foreman, active plans and needs-you count. See [section 3](#3-several-repositories-mode-b).
- `brigada project set [--repo URL] key=value ...`, `brigada project show [--repo URL]`: set and read the project's settings and where each comes from. See [section 6](#local-only-settings).
- `brigada project import [--repo URL] [--file PATH] [--yes]`: import `.brigada/project.yaml`. See [section 6](#importing-and-provenance).

### Care

- `brigada engine [--idle-minutes N]`: run the engine in the foreground. You rarely need it; commands start it on demand.
- `brigada recover [--snapshot-dir DIR] [--repo URL]...`: rebuild a lost store; the engine must be stopped. See [section 10](#10-backups-and-recovery).
- `brigada version`: print the version.
- `brigada help [<command> [<subcommand>]]`: print top-level or command usage.

## 15. Known limits

- The hold-only bootstrap credential is not built: the mode A bridge reaches the engine with your client token, and the hold gives it its own limited credential after `hold_acquire`.
- `brigada status` does not show whether your session is connected but not holding (presence is not implemented).
- The runner does not record the commit a reviewer approved, so `brigada land` only warns that the head was not compared ([section 5](#5-finishing-brigada-land)).
- Read-only extra repositories are a convention, not a sandbox ([section 7](#another-repository-to-read)).
- Codex has no verified project-scope registration: init prints a user-level command ([section 1](#1-install-and-brigada-init)), and an attached Codex session does not carry brigada's tools ([section 12](#attach)).
- Cross-repository plans do not exist: a plan belongs to one project ([section 3](#3-several-repositories-mode-b)).
- `brigada cache clear` is not implemented; the setup cache expires by age and count ([section 7](#an-environment-for-checks-and-reviews)).
- Workers run in git worktrees, not containers, and checks run with your credentials. Do not point brigada at a repository you do not trust.

Back to the [README](../README.md).
