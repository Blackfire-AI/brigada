# brigada: design

Status: **v0.1, approved** 2026-10-12 after a cross-model review, as the Go design baseline. What and why: [product.md](product.md). It supersedes the design of the earlier Python implementation ("v3.2"), which stays the reference for what is ported (§14). Tags: **(decided, date)** is settled; everything else is a proposal.

## 1. Principles

1. **The foreman decides; the engine enforces.** Judgment (planning, answering, re-planning) is an LLM agent. Mechanics (queues, leases, eligibility, caps, routine retries) are deterministic Go code.
2. **The engine is the mailbox.** The foreman and the workers never talk directly. Every message goes through the engine, and every message is kept (§9).
3. **No state lives only in a session.** A foreman or worker session can die at any moment; the engine and the plan file hold everything needed to continue.
4. **Prompts: templates plus verbatim data.** The engine may compose a prompt only from fixed, versioned templates plus verbatim data (a task spec, review text, check output, a foreman answer). Verbatim data is always wrapped in explicit data delimiters, so worker output can't pose as instructions to the foreman or a worker. Anything that needs judgment goes to the foreman. This relaxes Python v3.2's "the server never builds prompts", which assumed no routine fix rounds in the server.
5. **Remote-ready** (product §5): clients only through the API, runners only through the lease protocol, tokens everywhere, repos by URL, self-contained state.

## 2. Processes

| Process | What | Local (launch) | Remote (backlog) |
|---|---|---|---|
| `brigada` (client) | CLI: chat, status, history, stats, cancel, restart, review, budget, recover | your terminal | any device |
| engine | API, store, rules, dispatcher, foreman wake-ups | background process, started on demand | a server |
| runner | leases jobs, runs agent CLIs, runs checks | same host, separate process | any host with logged-in CLIs |
| foreman session | an official CLI (your choice of harness and model) with brigada's MCP tools | started by the runner (headless) or by `brigada` (when you chat) | same |
| worker session | an official CLI running one task | started by the runner | same |

All of them are the one `brigada` binary in different roles.

## 3. Communication: the engine as a mailbox

```
 you ⇄ foreman ──tools──▶ engine ◀──lease/result── runner ──▶ worker CLI
            ▲                │
            └──── events ────┘  (judgment events wake the foreman)
```

- **Down:** the foreman calls tools (create a plan, add tasks, answer, restart). The engine turns ready tasks into jobs; a runner leases a job and starts the worker.
- **Up:** a worker can't message anyone mid-run. It ends its job with a structured result (§5.4). The runner adds the check result and progress data. The engine records it as events.
- **Routine events are handled by rules, judgment events go to the foreman:**

| Event | Handled by | Action |
|---|---|---|
| check failed, rounds left | rule | fix round: same session, template + check output |
| review "changes", rounds left | rule | fix round: same session, template + review text |
| transient error (CLI crash, network) | rule | retry the job, capped |
| a dependency is done | rule | the dependent task becomes ready |
| quota limit hit by a worker | rule | cool the account (§8); re-dispatch to another eligible agent as a **fresh** attempt carrying the previous job's summary if it produced one, else its last progress events (a session can't move between agents), counting toward caps; no eligible agent → the task parks and the user is notified (§10) |
| worker `needs_help` | **foreman** | answer → next job on the same session, or re-plan |
| task out of rounds or retries | **foreman** | re-plan, switch agent, split, or ask the user |
| worker stalled (§5.6) | **foreman** (decided 2026-10-12: always; no automatic nudge) | restart with a new instruction, switch agent, or ask the user |
| plan above the approval limit | **user** | approve, edit or reject (§6.3) |
| foreman limit hit (§4.4) | **user** | wait, switch foreman, or continue without it (decided 2026-10-12) |
| plan done | rule | notify the user directly (§10); the foreman may add a summary on its next turn, skipped when it's `limited` or out of budget |

## 4. The foreman

### 4.1 What it is

- An official CLI session, **harness and model chosen by the user** (decided 2026-10-12), e.g. Claude Code with a given model, with brigada's MCP tools (§12.2) and the foreman instructions (a versioned file shipped with brigada).
- One foreman per project (repo). Its session is a **resumable session key** `foreman:<project>` (decided 2026-10-12: option B). The harness must support headless session resume (claude, kimi, zcode: yes, verified in the Python implementation; codex: `exec resume`; gemini: to verify). Setup refuses a harness without resume.

### 4.2 Two ways in, one session

1. **You chat:** `brigada` attaches you to the foreman session through the harness's own interactive resume.
   - **Attach acquires the key's live slot first** (one live job per key, Python v3.2 §2). If a headless foreman turn is running, the client shows "foreman is busy" and waits for it to finish, or cancels it through fencing if you insist. Two processes never resume one session at once.
   - The attach hold is lease-like: the client heartbeats it, and it expires if the terminal is killed. A second live attach is refused.
   - **On detach the client writes the current harness session id back** to the engine, as runners do on complete (`--resume` forks session ids; Python v3.2 §2a). Otherwise the next wake would resume the pre-chat id and lose the conversation.
   - Judgment events that arrive while you're attached reach the foreman through its `events` tool, which the instructions tell it to check between replies. On release, any events it didn't consume are evaluated for a wake-up.
   - **Events are consumed only when a foreman turn completes,** or when the attached foreman reads them through `events`. A cancelled or failed turn's events return to pending and are re-delivered on the next wake or attach, so cancelling a turn to attach never drops a worker's question.
   - At attach, pending events are put into the session's opening prompt (the same wake template), so they surface at once instead of waiting for your next reply.
2. **The engine wakes it:** when a judgment event arrives and nobody is attached, the engine submits a **foreman turn**: a headless resume of the same session, with a template prompt plus the pending events verbatim. The foreman acts through its tools and exits. Several events coalesce into one turn (the follow-up flag of Python v3.2).

Your next chat sees everything the foreman did while you were away, because it's the same session, and the history (§9) has it too.

### 4.3 Budget

Foreman turns while you're away use your subscription. A per-project budget of foreman wake-ups per day and worker-minutes per plan (Python v3.2's per-key budget) bounds them. When it runs out, judgment events wait for you, and you're notified. You can raise it: `brigada budget <plan> +<minutes>` for worker-minutes and `brigada budget --wakes +<n>` for today's wake-ups (clamped steps, as in Python v3.2's budget raise); the wake-up budget also resets each day.

### 4.4 When the foreman hits its limit

The runner detects the limit-hit error from the foreman's CLI. The engine marks the foreman `limited(until)`, or `limited(unknown)` when the error carries no reset time, stops waking it, and **notifies the user** (§10), who chooses: wait for the reset, switch the foreman's harness or model (the new session rebuilds its context from the engine, per product §4's kill-and-rebuild test), or let approved plans continue without it. Routine rules keep working throughout.

## 5. Workers and runners

### 5.1 Runner

- Registers with a runner token and advertises its adapters: harness, models, family, account label, classes it can take (product §5 rule 5).
- Leases jobs (long-poll), heartbeats with progress, completes with a fencing token. Lease expiry plus fencing make crashes safe (Python v3.2's lease engine, ported with its tests).
- Materializes its own workspace: clone or fetch the repo URL with its own git credentials into a per-task worktree, and push result branches to the remote (product §5 rule 6).

### 5.2 Adapters

One per CLI: build the headless command (fresh or resume), parse the CLI's event stream into progress events, detect limit-hit and auth errors, find the session id for resume. Copied where useful from Orchestrator.inc's Apache-2.0 adapters, keeping upstream license headers, carrying their NOTICE and marking modified files (CodexBar, if used, is MIT: same idea).

### 5.3 The worker prompt

A fixed worker template (versioned) plus the task spec verbatim, plus the answer or feedback for follow-up jobs. The template tells the worker to end with the result block (§5.4).

### 5.4 Worker result contract

The worker's final message ends with one JSON block:

```json
{"status": "done" | "needs_help" | "failed", "summary": "…", "question": "…", "branch": "…"}
```

A missing or malformed block counts as `failed` with reason `no-result`. The runner verifies the branch was pushed.

### 5.5 Checks run by the runner

After the worker exits with `done`, **the runner runs the task's check** in the worktree, not the worker. Exit code plus the output tail are the check result. That keeps the outcome record honest (a self-reported check would bias it).

### 5.6 Stalled workers and restart with a new instruction

Workers sometimes get stuck: alive, but making no progress (seen in practice).

- **Detection:** the adapter turns the CLI's event stream into progress events (tool calls, messages); the runner sends the time of the last one in each heartbeat. **Stalled = 10 minutes with no activity at all** (decided 2026-10-12; `stall_minutes`, configurable). A tool call in flight (a long build or test run) suspends the clock, bounded by its own `tool_call_cap_minutes` per class (default 30). Stalled, or the class time cap reached → the runner kills the process tree and completes the job as `stalled`, with the last progress events attached.
- A stalled job **always goes to the foreman** (decided 2026-10-12); there is no automatic nudge.
- **Restart:** `restart(task, instruction, mode, agent?)` cancels any live job and creates a new job on the same task with the new instruction.
  - `mode=resume`: same session, the instruction appended (the worker keeps its context);
  - `mode=fresh`: a new session, the instruction plus the previous job's summary and last progress (for a worker stuck in a loop);
  - optional `agent` to move the task to another agent.
- **Who restarts:** the foreman (a judgment event), or the user directly: `brigada restart <task> -m "…" [--fresh] [--agent …]`. A restart is a new attempt on the task and counts toward its caps. A resume that fails (the session is gone or forked) restarts fresh in place, as in Python v3.2 §2a.

## 6. Plans and tasks

### 6.1 Task graph

A plan is a DAG of tasks (decided 2026-10-12: dependencies at launch). Task fields: title, spec, check, class (`implement` | `review` | `answer` | …), deps, repo and base ref, caps.

Task states: `pending` (deps not done) → `ready` → `running` → `checking` → `reviewing` → `done`; or `needs_foreman`, `needs_user`, `failed`, `cancelled`. Cancelling a task cancels its dependents unless the foreman re-plans.

### 6.2 Plan lifecycle

`draft` (the foreman builds it) → `pending_approval` (above the limit) → `active` → `done` | `cancelled`. The foreman can amend an active plan (add, split, drop tasks); each amendment is recorded and re-checked against the approval limit.

### 6.3 Approval limit

Approval only above a limit (decided 2026-10-12). Proposed default: more than 5 tasks, any task touching protected paths (config, like a merge policy's protected class), or any task class other than `implement`/`review`/`answer`. Configurable per project.

### 6.4 The plan file in the repo

For fault tolerance, the engine writes each plan as **`.brigada/plans/<plan-id>.yaml`** (decided 2026-10-12) and pushes it, on every plan change, to a separate branch **`brigada/plans`** in the repo's remote by default, so the engine never commits to `main` on its own; committing to the default branch instead is a per-project setting (decided 2026-10-12). The file holds the graph, task states, branches and open questions, not transcripts. The engine store is authoritative; the file is a mirror.

**Backups.** The engine also snapshots its store (SQLite backup API) daily to a configurable directory outside the store.

**`brigada recover`** when the store is lost:
- **Restores:** from the latest snapshot if there is one, then plans, task states, branches and open questions from the plan files (newer wins).
- **Doesn't restore** without a snapshot: messages, transcripts and outcomes; the dispatcher's scores restart from defaults. Said plainly in the command's output.
- **Tokens** live in a config file outside the store (the store keeps only hashes), so clients, runners and the foreman can still authenticate; runners re-register.
- **Fence epoch:** recover bumps a store-wide fence epoch, and every lease, heartbeat or complete that references the pre-recovery epoch is rejected, so a late runner can't write into the rebuilt store.
- **In-flight work:** recover waits one lease-expiry window before re-dispatching, so runners whose heartbeats now fail have killed their workers. Re-dispatched attempts use new branch names (an attempt suffix), never the old ones.

## 7. Review policy (in the core)

After an `implement` job is done and its check passed, the engine creates a review job for an agent of a **different family** than the author. The reviewer gets the diff (base…branch), the task spec and the check result, **never the implementer's summary** (Python v3.2 §7.1's anti-steering rule), and ends with a verdict block (`approve` | `changes`) plus findings. An approve without substantive findings text (v3.2's minimum length) counts as `no-verdict` and is retried with another reviewer, so lazy approvals don't inflate the outcome record. `changes` → a fix round (§3 rule) up to `review_rounds` (default 2), then a judgment event. Family detection: the job record; for `brigada review --branch` without a job, the `Co-Authored-By` trailer. That standalone command creates a one-task plan of class `review`: the runner runs the repo's configured check, and there are no fix rounds unless you ask. Merging is out of scope at launch: the foreman reports approved branches and the suggested merge order.

## 8. Dispatcher

- **Eligible:** the adapter supports the class; family rule for reviews; agent `ready` (not `broken`, not `cooling`); account not cooling; under the max-parallel cap; session affinity (a resumed session stays on its agent and runner).
- **Score:** per (repo, class, agent): a success rate with a prior, where success = check passed and review approved within the rounds. Defaults for a cold start; an exploration share (default 10%) goes to the next-best eligible agent.
- **Quota:** limit-hit errors cool the whole account until its window resets; CodexBar's CLI JSON is an optional extra signal.
- **Health:** repeated failures, auth errors or a dead runner → `broken(reason)` until a probe passes or the user resets it.
- Every decision is recorded with its reason ("why this agent").

## 9. History

Everything is kept and readable (decided 2026-10-12).

- **Messages:** one table of conversation items per project: user ⇄ foreman turns, foreman tool calls and their results, worker task specs and results, questions and answers, reviews, notifications. `brigada history [--plan|--task]` shows them as one timeline.
- **Transcripts:** after every job, the runner uploads the session's full transcript (the CLI's own event log) to the engine as an artifact, linked from the job. The foreman's chat transcript is archived the same way after each attach and each headless turn.
- **Events:** an append-only event log is the backbone. Messages and transcripts reference events.
- Retention is configurable; the default keeps everything.

## 10. Notifications (launch: local)

Local OS notifications plus a "needs you" queue in `brigada status`: plan approval, user-directed questions, the foreman's limit, the foreman's budget out, a broken agent, a task parked with no eligible agent, a plan done. Remote channels (webhook, Telegram) are in the backlog.

## 11. Data model (SQLite, one file)

`projects` (repo URL, default branch, settings, foreman harness and model) · `plans` · `tasks` (+ `task_deps`) · `jobs` (task, kind: `work`|`fix`|`review`|`foreman`, agent, mode, prompt, lease and fence, status, result, check result, last progress, transcript ref) · `sessions` (key, agent, runner, harness session id, state) · `runners` · `agents` (runner, adapter, harness, model, family, account, classes, health, cooling until) · `outcomes` · `events` (append-only) · `messages` · `questions` · `tokens` (hashed) · `artifacts` (transcripts, check logs).

## 12. Interfaces

### 12.1 Engine API

JSON over HTTP; locally on a Unix socket, remotely over TLS. Bearer tokens with roles: `client`, `foreman` (scoped to its project), `runner`, `admin`. Groups: projects, plans, tasks (create, amend, cancel, restart), questions (answer), events (long-poll and SSE), history, stats, budget, agents and runners, artifacts.

**Idempotency.** The caller is often an LLM whose tool calls may repeat. `plan_create`, `plan_amend`, `task_restart` and `answer` take an idempotency key (client-supplied, or derived: task + round for restart and answer); a repeat returns the first call's result. The key is minted by the `brigada mcp` bridge per logical tool call (a hash of the call's content within the session), not left to the LLM; identical `plan_create` content within a short window also dedupes. Worker jobs are keyed `task:<id>`: one live job per key makes duplicate job creation coalesce into the follow-up flag, which `status` shows.

### 12.2 Foreman MCP tools

`plan_create` · `plan_amend` · `task_restart` · `task_cancel` · `answer` (a worker's question) · `ask_user` · `events` (long-poll, for the attached session) · `status` · `stats` · `history`. The MCP server is a `brigada mcp` stdio process started by the foreman's harness; it calls the engine API with the foreman token.

### 12.3 Runner protocol

`register` · `lease` (long-poll) · `heartbeat` (progress, last activity) · `complete` (result, check result, transcript upload) · `release`. Fencing as in Python v3.2.

## 13. Engine lifecycle

Single instance (a lock on the store); started by any `brigada` command when not running; shuts down after `idle_minutes` with no live jobs, attached sessions, pending wake-ups or unsent notifications (it flushes them first); crash or reboot recovery through lease expiry and fencing, then pending foreman wake-ups are re-evaluated.

## 14. Porting from Python v3.2

Moves over with its tests: the lease engine with fencing, one live job per key, follow-up coalescing, the run cap and per-key budget, fresh/session modes and affinity, review after implement (from the connector into the core). Stays in the fleet: the merge gate, bot forks, worker images, egress proxy, chat-front-end connectors.

Transformations, so nothing is lost in translation:
- **`trigger` human/bot is obsolete:** there are no human-submitted jobs (your input is chat, not a job). Don't port it.
- **The run cap (bot rounds)** becomes per-task caps on fix, review and restart rounds, plus per-plan worker-minutes.
- **The per-key budget raise** becomes `brigada budget` (§4.3).
- **Keys:** worker jobs `task:<id>`, the foreman `foreman:<project>`. One live job per key, coalescing, the follow-up flag and affinity hang off them.
- **Session write-back and resume-fail → restart in place** (§2a) are kept for workers and for the foreman's attach (§4.2).

## 15. Phases

1. **Engine:** store, API, runner protocol, local runner, one adapter (claude), check by the runner, history (events, messages, transcripts).
2. **Foreman, one task:** the foreman for one task end to end (attach and headless wake), review, `brigada status/history`.
3. **Plans:** plans with dependencies, the approval limit, the plan file, the dispatcher with outcomes.
4. **Failures:** failure handling: stall detection, restart with new instruction, needs_help loop, the foreman's limit flow, notifications.
5. **Re-planning:** re-planning, the planner's outcomes.

## 16. Open questions

1. Gemini CLI's headless resume support.
2. The foreman's wake-up budget defaults.
3. `tool_call_cap_minutes` defaults per class.

Settled 2026-10-12: stalled = 10 minutes with no activity; stalls always go to the foreman; plan files at `.brigada/plans/` on the `brigada/plans` branch by default, `main` as a setting.

Note for remote: §6.4 gives the engine push credentials to user repos; fine locally, re-examine for remote or multi-user use.
