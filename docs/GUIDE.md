# brigada: user guide

The [README](../README.md) takes you from install to a first task. This guide is for the days after: using brigada every day, reading what it tells you, and fixing what goes wrong. It assumes brigada is installed, a runner is up and a foreman is chosen. For the terms of use, see [TERMS.md](TERMS.md).

Contents: [1 The pieces](#1-the-pieces-and-where-state-lives) · [2 Daily use](#2-daily-use) · [3 Plans](#3-plans) · [4 Checks and reviews](#4-checks-and-reviews) · [5 Agents, quota and health](#5-agents-quota-and-health) · [6 The record and stats](#6-the-record-and-stats) · [7 Backups and recovery](#7-backups-and-recovery) · [8 Configuration reference](#8-configuration-reference) · [9 Command reference](#9-command-reference) · [10 Foreman on claude or codex](#10-foreman-on-claude-or-codex)

## 1. The pieces and where state lives

Four parts, one binary. You talk to the **foreman**, an agent session (a `claude` or `codex` CLI) that has brigada's tools; it turns your asks into plans and decides what to do when a task needs judgment. The foreman talks to the **engine**, which holds the store (one SQLite file), the rules and the dispatcher, and serves everything on one Unix socket; it starts on demand when any command needs it and exits after 30 idle minutes (no live jobs, no attached chat, no open requests). The engine runs no model. The **runner** is the process you leave running: it offers your signed-in CLIs as agents, leases jobs from the engine, runs each worker in a git worktree, runs the task's check, and pushes the task's branch. The **repo remote** is where the branches land, along with the plan files on the `brigada/plans` branch.

State lives in the state directory: `~/.local/state/brigada`, or `$XDG_STATE_HOME/brigada`, or what `state_dir` or `--state-dir` says. In it: `store.db` (the store), `brigada.sock` (the socket), `engine.lock`, `engine.log`, `artifacts/` (transcripts and check logs, one file each; the store only points to them), `tokens` (the credentials the roles use to talk to the engine, readable by you only; keep it that way) and `runner/` (the runner's `mirrors/` of your repos and a `worktrees/` directory per running job). The socket path has a length limit of 104 bytes; a state directory that is too deep is refused with a message that says so. The config file is `~/.config/brigada/config`, or `$XDG_CONFIG_HOME/brigada/config`, or what `--config` names; it is optional (section 8). The flags `--state-dir` and `--config` go after the command: `brigada status --state-dir DIR`.

## 2. Daily use

### Attach, or ask

**Attach** is a conversation. Run `brigada` (or `brigada attach`) inside the repo: the repo's `origin` URL names the project, and you are put into the foreman's own session in your terminal. Use it when you want to think out loud, review a plan, or answer questions. Only one chat can be attached at a time; if the foreman is in the middle of a headless turn, attach waits for it to finish, and `brigada attach --force` cancels that turn instead. While you are attached the engine does not start headless turns. What the foreman sees when you attach: the events that piled up while you were away (finished plans, parked tasks, questions) and any waiting messages, in its opening prompt, and it reads new ones through its `events` tool as they arrive. When you leave, the engine records where the session ended so the next turn continues it, and anything the foreman did not read is looked at again for a wake-up. If the project has no foreman yet, attach tells you the `brigada project foreman` command to run first.

**Ask** is a message. `brigada ask "fix the flaky auth test; the check is: go test ./auth/..."` leaves your words in the foreman's inbox and returns at once with one line that says what happened: the foreman was woken, or why the message waits (you are attached, a turn is already running, the project has no foreman, the foreman is limited). Use it for a request you do not need to discuss. It takes `--repo <origin-url>` to name the project when the engine knows several, and reads the message from stdin when the only argument is `-`:

```
git diff main | brigada ask -
```

A message you send is not charged against the foreman's daily limit on its own wake-ups (20 per UTC day, per project); a wake that carries your message goes ahead even when that limit is spent.

### Following the work

`brigada status` prints the engine (pid, uptime), the job counts by status, the jobs a runner holds right now with the worker's last move, any agents cooling down, the plans with their tasks, the foreman's latest report, how many messages wait for the foreman, and the **needs you** queue. `brigada status --line` prints one short line for a terminal status bar; it never starts the engine. The queue lists what only you (or the foreman) can unblock:

| Entry | What it wants |
|---|---|
| `plan #N pending_approval: <why>` | your decision: `brigada plan approve N` or `brigada plan reject N` (section 3) |
| `task #N needs_foreman: <reason>` | judgment: the worker asked a question, the check failed with no rounds left, a review wanted changes, the task failed. The foreman is woken for these; the entry is there so you can see it and step in |
| `task #N needs_user: <reason>` | an answer from you, for a task parked for you directly |
| `foreman #N limited: <until>` | a choice: wait, switch the foreman, or let approved plans run without it (section 5) |
| `agent <harness>/<model> broken: <reason>` | a fix, then `brigada agent reset <harness>/<model>` (section 5) |

When `notify` is `auto` (the default), the engine also raises a desktop notification for the same kinds of moments: a plan done, a plan waiting for approval, a task parked, the foreman's report, a limited foreman, a broken agent.

`brigada history` prints the timeline, oldest first: events and messages together. `--task N` and `--job N` narrow it to one task or one job; `--limit N` (1 to 500, default 100) is the page size, and every page is followed to the end.

### The path a task walks

A task moves through these states, as `brigada status` shows them next to the task:

1. `pending`: waiting for the tasks it depends on. Status adds `(waiting on …)` with their names. This is the "waiting on dependencies" state.
2. `ready`: queued for an agent. A task can also wait here because every cap that applies is full (section 5); it starts when a slot frees.
3. `running`: a worker has the job. The runner's check also happens while the task is `running`: there is no separate `checking` state in the output, though the check runs as its own step after the worker reports done.
4. `reviewing`: for `implement` tasks whose check passed, a reviewer of another family reads the change (section 4). A review that asks for changes sends the task back to `running` for a fix round.
5. `done`.

Off the road: `needs_foreman` and `needs_user` (parked, see the queue above), `failed`, and `cancelled`. Jobs have their own statuses, the ones in the `jobs:` line of status: `queued`, `leased`, `running`, `done`, `failed`, `cancelled`, `stalled`. A worker that makes no progress for ten minutes is killed and its job ends `stalled`; the task goes to the foreman.

### Cancelling

`brigada cancel <job-id>` stops one live job: a worker, a review, or a foreman turn. The runner learns on its next heartbeat. It cancels that job only. It does not cancel the task or the plan, and it is not an approval of anything. To drop a whole plan that is waiting for approval, use `plan reject`. To drop tasks of a running plan, ask the foreman; it can cancel tasks and re-point what depended on them. Job ids are not task ids: `status` shows a running job as `task #N …`, so find the job id with `brigada history --task N`.

### Several repositories

A **project** is one repo URL (the `origin` URL, exactly as the engine was told it), and each project has its own foreman session, plans, history and stats. If you work in `api`, `web` and `infra` repos, you have three projects and three foremen, all served by one engine and the same runners. A monorepo is one project.

Working with several:

- Choose a foreman once per repo: inside the repo, `brigada project foreman --harness <h> --model <m>` (the repo defaults to the origin of the working directory like `attach`; `--repo <origin-url>` names it from anywhere).
- `brigada project list` prints one line per project: the repo URL, the foreman as `harness/model` (`-` if none is chosen), the active plans and the needs-you count.
- Attach inside the repo you want to talk to. Ask one repo at a time: `brigada ask --repo <origin-url> "..."`. Without `--repo`, `ask` works only while the engine has exactly one project; with more it refuses and `project list` shows the URLs. When `attach` or `ask` cannot map a repo to a project, the error lists the known projects' URLs.
- `brigada status` shows the plans and the needs-you queue of all projects together; use `project list` to see the counts per repo.

Not supported today: a plan whose tasks span repos (a plan belongs to one project, and each task runs in that repo), and a foreman that knows about another repo's work (it sees only its own project's events). For a change that touches two repos, ask each repo's foreman separately and carry the coordination yourself, such as merging in order. Cross-repo plans are an open question in [design.md](design.md) section 16.

## 3. Plans

You ask; the foreman turns the ask into a **plan**: a set of tasks, each with a spec for the worker, a check command and a class (`implement` by default, also `review` and `answer`), and a list of the tasks it depends on. A task with no dependencies is queued at once. A task with dependencies waits (`pending`) until all of them are done; if one of them fails or is cancelled, the dependents keep waiting until the foreman re-points them to another task or drops the dependency. The foreman can amend a live plan: add tasks, cancel tasks, re-point dependencies. A plan is `done` when no live work is left, and that is not the same as everything succeeding: the plan's done report names the tasks that failed, that were cancelled without your approval, and that never ran. A foreman that cancels a task is abandoning it; brigada does not count that as your approved skip, and there is no command that records one for you.

**The approval limit.** A plan starts on its own unless it trips one of three triggers, each per project:

- more tasks than `approval_max_tasks` (default 5);
- a task class outside `approval_classes` (default `implement`, `review`, `answer`);
- a task whose spec or check mentions a path that matches one of `approval_protected_paths` (a list of glob patterns; default none).

A plan that trips one waits in `pending_approval` and nothing is queued. The needs-you entry gives the reasons, for example `7 tasks, over the limit of 5`. An amendment that adds tasks is checked against the limit again. You decide:

```
brigada plan approve 7    # queues the tasks; prints each task's job
brigada plan reject 7     # cancels the plan's tasks
```

Only you can decide: the foreman has no way to approve its own plans. `approval_max_tasks = 0` makes every plan wait, and a negative value switches the limit off.

These three settings belong to the project and are held in the store. They are not keys of the config file, which refuses them as unknown. Change them with `brigada project set [--repo URL] key=value ...` and read them with `brigada project show [--repo URL]`; `--repo` defaults to the `origin` of the repo you are in, and flags go before the `key=value` pairs. The values:

- `approval_max_tasks=N`: an integer. `0` makes every plan wait; a negative number switches the limit off.
- `approval_classes=implement+review`: classes joined with `+`.
- `approval_protected_paths=infra/**+db/*.sql`: glob patterns joined with `+`.

An empty value, as in `approval_max_tasks=`, puts the key back to its default. An unknown key or a value the key does not take is refused with a usage error that names the valid keys, and nothing is stored. `project show` prints every key with its effective value and whether it is the default, for example `approval_max_tasks=5 (default)`; an empty path list shows as `none`. `project set` creates the project if the repo has none yet, and prints the same listing afterwards. Only you can run these commands; the foreman has no tool for them.

**The plan file.** Every change to a plan is mirrored to `.brigada/plans/<plan-id>.yaml` and pushed to the `brigada/plans` branch of your remote, not to your default branch. The file holds the graph, the task states, the branches and the open questions; not transcripts. The store is the authority, and the file is a copy that may lag (a push that fails is retried on the next change). It exists so that `brigada recover` can rebuild plans after a lost store (section 7), and so you can see a plan from another machine.

## 4. Checks and reviews

**Checks.** A task's spec names a check command. After the worker reports done, the runner runs the check in the task's worktree. The exit code and the tail of the output are the result. The worker's own claim that tests pass is not used. A task may carry no check at all, and then nothing is run. If the check of an `implement` task fails and fix rounds are left, the worker gets the check's output and fixes on its own session. A failed check on a task of any other class gets no fix round: the task parks at `needs_foreman`.

**Reviews.** After an `implement` task's check passes, a reviewer reads the diff, the spec and the check result, never the author's summary. The reviewer is an agent whose harness differs from the author's, so with a single family among your agents no review can run (README, quick start). An answer of approve counts only with substantive findings text (80 characters or more); a thinner approve, or a review that ends without a verdict, is retried once with another reviewer, and then the task waits for the foreman. Other classes finish when the run does, without a review.

**Rounds.** A review that asks for changes, or a failed check, starts a fix round on the author's session with the findings as feedback. Fix rounds for failed checks and for review changes draw on one shared budget: by default two fix rounds in all per attempt, plus two restarts. When a cap is spent the task parks at `needs_foreman` and the entry says which cap ran out; the foreman decides whether to re-plan, switch agent, split the task or ask you.

**A branch with no plan.** `brigada review --branch <name> [--repo <origin-url>]` queues a review of a branch on its own and prints the job id (or says one is already queued or running). The author's family is read from the branch's last work job; failing that, from the `Co-Authored-By` trailers on its commits; failing that, `default_reviewer` in the config file (a harness name) takes the review. With none of the three the review is refused. There are no fix rounds, and a review that ends without a usable verdict is not retried: run it again. The verdict (approve, changes or no verdict, with the findings) is written to the history; it raises no desktop notification and no needs-you entry, so read it with `brigada history`.

## 5. Agents, quota and health

**What the runner offers.** `brigada runner` takes one `--agent` per harness, repeatable, at most one per harness:

```
brigada runner --agent harness=claude,model=sonnet --agent harness=codex,account=work
```

`harness` is `claude`, `kimi`, `codex` or `opencode`; `model` defaults to `sonnet` for claude and to the CLI's own default for the others (for opencode, a model is `provider/model`); `classes` is a `+`-separated list, default `implement+review+answer`; `agent-file=<path>` is for kimi only. Without `--agent`, the shorthand flags `--harness`, `--model`, `--classes` and `--agent-file` describe one agent; the two forms cannot be mixed. Also: `--labels` (labels the runner offers), `--max-parallel N` (how many concurrent jobs this runner runs; the default 1 is one at a time) and `--no-start`. Keep the runner running: a job nobody leases within ten minutes fails, but the ten minutes do not count while every runner that could take the job is full: `brigada status` shows such a job as held, waiting for a runner slot. `kimi` and `opencode` are worker-only; the foreman can be `claude` or `codex`.

**Accounts.** An agent's account is the login whose quota it spends. The account label defaults to the harness name, which is right when each CLI has one login. If you have a second login for a CLI, offer it as another runner's agent with `account=<label>` so that the two logins keep separate quotas, caps and cooling.

**At a vendor limit.** A worker that hits its subscription limit cools the account (the `cooling:` line below, not a queue entry) until the reset time the CLI reported, or for 30 minutes when it gave none. Every agent on that account is skipped by the dispatcher meanwhile. The task that hit the limit gets the automatic retry below. `brigada status` shows `cooling: <harness>/<model> until <time> (account <label>)`. It clears when the time passes, or at once with `brigada agent reset <harness>/<model>` if you know the limit is lifted. The **foreman** is separate: only a limit error on a foreman's own turn marks it limited, shown as `foreman #N limited: …` in the queue, and no new turns are started on it. When the error gave a reset time, the mark clears itself then. When it gave none (`limited, reset time unknown`), waiting never clears it: only changing the foreman does. You choose:

- wait for the reset (only when a time was given);
- switch: `brigada project foreman --repo <origin-url> --harness <h> --model <m>` (a different harness or model than the current pair), which clears the limit and wakes the new foreman for what waited;
- or do nothing: approved plans keep running without it, since the routine rules (dependencies, fix rounds, retries, reviews) do not need the foreman, but anything that needs judgment waits.

**Automatic retry.** When a worker's CLI fails for a transient reason (a quota error, an authentication error, a lost runner), a task's first work job gets one automatic restart on another ready agent, with a fresh session and a new branch, carrying the failed job's last progress. Failures in fix rounds and reviews are not restarted this way. If no other agent can take it, or the restart was already used, the task parks for the foreman. Set `task_auto_retry = false` to park such failures at once.

**Broken agents.** An agent is taken out of dispatch as broken when: its CLI reports an authentication failure; or its last two jobs both failed (a limit hit is quota, not a failure, and does not count); or its runner has not been heard from for 15 minutes. The first two stay broken until you fix the cause (log the CLI in again, fix what made it fail) and run `brigada agent reset <harness>/<model>`. The third is not a verdict on the agent: the agent comes back by itself as soon as its runner polls again. Broken agents are listed in the needs-you queue with the reason, and raise a notification. `agent reset` prints how many agents matched; the name is the `harness/model` pair that `status` and `stats` print, and it resets every runner's copy of that agent.

**Capacity.** Caps bound how many jobs run at once, in layers: this runner, the account, the model, the plan and the whole engine (`caps.*` keys, section 8). A job starts only when every layer that applies has room. A task blocked only by full caps waits in the queue (it is not parked and nobody is notified) and starts when a slot frees. A foreman turn is never held back by the plan or model caps and goes ahead of queued task jobs, but while it runs it still counts as use of its account, runner, global and model, so it can leave a worker on the same model waiting. The foreman can look at the capacity it has (per agent, the cap, the running jobs, the quota state and the cost weight) when it plans, but the engine enforces the caps whatever the foreman believes. `cost.<harness>/<model>` is a relative weight, default 1, that the foreman sees when it decides which tasks can wait for a cheaper agent.

**Headroom.** With `headroom = true` (the default), task workers on the account that serves your foreman's harness see that account's cap lowered by one (never below zero), so a slot stays free for the foreman. This is about parallel slots; it does not stop a vendor quota from running out. It acts only where a `caps.account.<label>` cap is set; with a cap of 1 on that account, task workers do not run there at all. Set `headroom = false` to let workers use every slot.

## 6. The record and stats

`brigada stats` prints one table per repo:

```
AGENT  CLASS  ATTEMPTS  SUCCESSES  RATE  CONFIDENCE
```

- **Attempts and successes** are counted when a task ends, for the agent that wrote the final change, in the task's class. A success is a task whose check passed and, for `implement` tasks, whose review approved within the allowed rounds; tasks of other classes succeed when the run is done with its check passed, with no review. A task that runs out of rounds counts an attempt without a success. A reviewer's substantive approve counts as an attempt and a success in the `review` class for the reviewer's agent. When the foreman's plan finishes, it is counted in the class `plan` for the foreman's agent, as a success when every task was done (or cancelled with your approval); this is measured only and does not steer dispatch.
- **Rate** is the score the dispatcher ranks by: `(successes + 0.5) / (attempts + 1)`. The prior is one pseudo-trial at 50%, so an agent with no record scores 0.50 and a single success reads 0.75, not 1.00. More attempts pull the rate toward what actually happened.
- **Confidence** is a word for the attempts behind the score: `none` for 0, `starting` for 1 to 4, `enough` from 5 on.
- An agent whose runner has not been seen lately is marked `(stale)`; a registered agent with no record yet shows at 0 attempts.

Read it by class: the dispatcher picks, for each task, among the agents that are ready, have quota and have room under the caps, the one with the best score, and sends about one task in ten to the next best so that an agent with a thin record can be measured. Two limits today: the ranking is among the agents of the runner that is asking for work, and ordinary work and fix jobs are ranked on the `implement` score whatever the task's class, so scores in other classes do not yet steer how those tasks are dispatched (reviews are ranked on `review`). Scores are per repo and per class: an agent that wins `implement` on one repo says nothing about another repo or about `review`. On a fresh install the table is mostly priors; trust it as the `enough` rows appear.

Nothing leaves the machine for this: the record is in the local store, and brigada sends no telemetry. That is a statement about brigada, not about the CLIs it drives: they send your prompts and the code they read to their own providers, as when you run them by hand, and the runner pushes branches to your git remote with your credentials (README, "No telemetry").

## 7. Backups and recovery

Set `snapshot_dir` in the config file to an absolute directory, outside the state directory. A running engine then copies the whole store there through SQLite's backup interface when the newest snapshot is more than a day old (and when there is none), as `store-<UTC time>.db`, and keeps the last seven. The check looks at the directory, so an engine that idles out and restarts still snapshots daily. A failed snapshot is retried an hour later.

If the store is lost or damaged, stop the engine, then:

```
brigada recover
```

With no snapshot, at least one `--repo` is required: the bare command has nothing to rebuild from. (`--snapshot-dir DIR` names a directory other than the config's, and `--repo <origin-url>`, repeatable, names a repo whose plan files to apply beyond those of the snapshot's projects.) `recover` moves the old store aside, restores the newest snapshot into a fresh store, and applies the plan files from the repos' `brigada/plans` branches, with the newer of snapshot and file winning. It refuses while the engine runs.

**Restored:** everything in the snapshot, as of the moment it was taken, and plans, their task states, branches and open questions from the plan files after that. **Not restored:** what happened after the snapshot, other than plans rebuilt from their files. The snapshot copies the SQLite store only; transcripts and check logs live in `artifacts/` in the state directory, so if those files are lost their contents are lost even with a snapshot. Plan files carry no task specs or check commands: a task rebuilt from one has neither, and a task that was mid-flight (running, checking or reviewing) has no job any more and waits at `needs_foreman` for the foreman. With no snapshot at all, the store starts empty and is rebuilt from plan files alone, so messages, transcripts and outcomes are gone and the dispatcher's scores start again from the prior.

Recovery also raises the *fence epoch*, a counter that makes every lease from before the recovery invalid, so a runner that was running through it cannot write into the rebuilt store. Dispatch is held for one lease window, so such runners stop their workers first. The tokens are not in the store, so clients, runners and the foreman keep working, and runners register again on their next poll.

## 8. Configuration reference

The config file is optional. Format: one `key = value` per line; a line starting with `#` is a comment (comments go on their own line, not after a value); blank lines are fine; an unknown key is an error, and so is a value of the wrong kind.

| Key | Value | Default | Meaning |
|---|---|---|---|
| `state_dir` | path | `$XDG_STATE_HOME/brigada`, else `~/.local/state/brigada` | The state directory. `--state-dir` overrides it. |
| `idle_minutes` | number, 0 or more | 30 | The engine exits after this many minutes with no live jobs and no open requests. `0` means the default. `brigada engine --idle-minutes` overrides it. |
| `notify` | `auto` or `none` | `auto` | `auto` raises desktop notifications with the OS tool when found; `none` turns them off. |
| `snapshot_dir` | absolute path | none (no snapshots) | Where the daily store snapshots go; the last seven are kept (section 7). |
| `headroom` | `true` or `false` | `true` | Keep one slot free for the foreman on its account (section 5). |
| `task_auto_retry` | `true` or `false` | `true` | One automatic restart on another agent after a transient worker failure (section 5). |
| `default_reviewer` | harness name | none | The harness that takes a standalone review when the branch's author family cannot be told (section 4). |
| `caps.global` | integer, 0 or more | no cap | Most parallel jobs in the whole engine. |
| `caps.plan` | integer, 0 or more | no cap | Most parallel tasks per plan. |
| `caps.runner` | integer, 0 or more | no cap | Most parallel jobs per runner; a runner's own `--max-parallel` also applies, and the smaller wins. |
| `caps.account.<label>` | integer, 0 or more | no cap | Most parallel jobs on one account label (section 5). |
| `caps.model.<model>` | integer, 0 or more | no cap | Most parallel jobs on one model. |
| `cost.<harness>/<model>` | integer, 0 or more | 1 | Relative cost weight of an agent, shown to the foreman. |

A cap of `0` caps the layer at nothing, which is different from leaving it out. An example:

```
notify = none
snapshot_dir = /home/you/backups/brigada
caps.global = 6
caps.plan = 3
caps.account.claude = 3
caps.account.codex = 2
cost.claude/opus = 2
```

## 9. Command reference

`brigada help` prints the same list. A command's flags go after its name; every command also takes `--state-dir DIR` and `--config PATH`, and most client commands take `--no-start` (fail instead of starting the engine). Exit codes: 0 ok, 1 error, 2 usage.

**Talking**

- `brigada`, `brigada attach` [`--force`]: chat with this repo's foreman. See section 2.
- `brigada ask [--repo URL] <message...>` (or `-` for stdin): send the foreman a message without attaching. See section 2.

**Working**

- `brigada runner --agent harness=<h>[,model=<m>][,classes=<c1>+<c2>][,account=<label>][,agent-file=<path>]`: run a runner that leases jobs, runs workers and checks, and pushes branches. See section 5.
- `brigada task submit --repo URL [--base REF] --spec FILE|- [--check CMD] [--title T] [--harness H]`: submit one task directly, without the foreman. A debugging path; the base ref defaults to `main`. See sections 3 and 4.
- `brigada review --branch <name> [--repo URL]`: review a branch with an agent of another family, with no plan. See section 4.

**Watching**

- `brigada status` [`--line`]: the engine, jobs, plans and the needs-you queue. See section 2.
- `brigada history` [`--task N`] [`--job N`] [`--limit N`]: the timeline. See section 2.
- `brigada stats`: attempts, successes, rate and confidence per agent, class and repo. See section 6.

**Deciding**

- `brigada plan approve <id>`, `brigada plan reject <id>`: your answer on a plan over the approval limit. See section 3.
- `brigada agent reset <harness>/<model>`: clear a broken or cooling agent. See section 5.
- `brigada cancel <job-id>`: stop one live job. See section 2.
- `brigada project foreman [--repo URL] [--harness H --model M]`: show, or with both flags set, the repo's foreman (`claude` or `codex`); `--repo` defaults to the origin of the working directory. See section 10.
- `brigada project list`: every project with its foreman, active plans and needs-you count. See section 2, "Several repositories".
- `brigada project set [--repo URL] key=value ...`, `brigada project show [--repo URL]`: set and read the project's approval limit (`approval_max_tasks`, `approval_classes`, `approval_protected_paths`). See section 3.

**Care**

- `brigada engine` [`--idle-minutes N`]: run the engine in the foreground. You rarely need it; commands start it on demand. See section 1.
- `brigada recover` [`--snapshot-dir DIR`] [`--repo URL`]: rebuild a lost store; the engine must be stopped. See section 7.
- `brigada mcp --client`: serve brigada's client tools (`ask_foreman`, `status`, `history`) over stdio to another coding harness that acts for you. The same command with `--socket PATH` is the bridge a foreman session uses; you do not run that by hand.
- `brigada version`: print the version.
- `brigada help`: print the usage.

## 10. Foreman on claude or codex

Only `claude` and `codex` can be a foreman; `kimi` and `opencode` are worker-only. Choose with `brigada project foreman --repo <origin-url> --harness <h> --model <m>` (inside the repo, `--repo` can be left out); without `--harness` and `--model` it shows the choice.

**Reading the code.** Headless turns and attach refresh `<state>/runner/foreman/<project>/repo` from the project's repo URL at its configured default branch. The opening prompt names the checkout's absolute path, branch and commit. Read the code and its CLAUDE.md/AGENTS.md before planning; the snapshot may lag the remote by one fetch. A clone or fetch failure fails the turn (or attach) with the git reason. The session's working directory stays the parent scratch directory, so existing sessions continue to resume.

**What differs.** With `claude`, `--add-dir` grants access to the checkout, and additional-directory CLAUDE.md loading is enabled. The foreman may read files (`Read`, `Grep`, `Glob`) and call brigada's tools; the shell and every editing tool (`Bash`, `Edit`, `NotebookEdit`, `Write`) remain denied. With `codex` 0.162.1, the checkout is named in the prompt and the foreman runs with `-s read-only`. Independent config overrides keep automatic review for MCP approvals and reject sandbox/permission escalations. The `--approve-for-me` alias cannot be used here: it selects `workspace-write`, which would make the scratch's repo writable. Codex's filesystem sandbox protects the checkout from writes; instructions alone are not that protection. Codex still has shell tools, unlike Claude's explicit tool denials.

**Attach differs too.** An attached `claude` foreman is the same session the headless turns resume, so the next turn continues your chat. The first attach shows Claude Code's own "trust this folder" prompt for the foreman's working directory under the state directory. Choose yes: it is brigada's scratch folder, with its reading checkout under `repo`. An attached `codex` session is Codex's own terminal interface; it does not carry brigada's tools, and Codex does not expose the thread in a form brigada can write back, so a later headless turn may start a fresh Codex thread instead of continuing the chat.

**Switching.** Running `project foreman` again with another harness starts a fresh foreman session: a session belongs to one CLI, so the old one is dropped and the new foreman rebuilds what it needs from the engine (the plans, the history, the pending events). A change of model on the same harness keeps the session. Switching also clears a foreman limit, as in section 5. Nothing about plans or tasks is lost.

Back to the [README](../README.md).
