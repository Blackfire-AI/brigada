# brigada: design

Status: **v0.1, approved** 2026-10-12 after a cross-model review, as the Go design baseline. What and why: [product.md](product.md). Terms: [glossary.md](glossary.md). It supersedes the design of the earlier Python implementation ("v3.2"), which stays the reference for what is ported (§14). Tags: **(decided, date)** is settled; everything else is a proposal.

## 1. Principles

1. **The foreman decides; the engine enforces.** Judgment (planning, answering, re-planning) is an LLM worker (the foreman, §4). Mechanics (queues, leases, eligibility, caps, routine retries) are deterministic Go code.
2. **The engine is the mailbox.** The foreman and the task workers never talk directly. Every message goes through the engine, and every message is kept (§9).
3. **No state lives only in a session.** Any session, the foreman's or a task worker's, can die at any moment; the engine and the plan file hold everything needed to continue.
4. **Prompts: templates plus verbatim data.** The engine may compose a prompt only from fixed, versioned templates plus verbatim data (a task spec, review text, check output, a foreman answer). Verbatim data is always wrapped in explicit data delimiters, so one worker's output can't pose as instructions to the foreman or another worker. Anything that needs judgment goes to the foreman. This relaxes Python v3.2's "the server never builds prompts", which assumed no routine fix rounds in the server. **The one other input that is not data is a user message** (§4.2 item 3; proposal, 2026-10-14): the user's own words, sent through the client API, reach the foreman as a request to act on, in their own section and never inside an event's. Only a `client` or `admin` token can send one, which stops a worker or a foreman turn from *posting* one. Delimiters stop the rest: in the foreman's prompts the engine neutralizes the delimiter tags (`<event>`, `</event>`, `<user-message>`, `</user-message>`) inside any verbatim data it wraps, so no data can close its section and open another; the worker prompts' delimiters get the same treatment (backlog).
5. **Remote-ready** (product §5): clients only through the API, runners only through the lease protocol, tokens everywhere, repos by URL, self-contained state.

## 2. Processes

| Process | What | Local (launch) | Remote (backlog) |
|---|---|---|---|
| `brigada` (client) | CLI: chat, ask, status, history, stats, cancel, restart, review, budget, recover | your terminal | any device |
| engine | API, store, rules, dispatcher, foreman wake-ups | background process, started on demand | a server |
| runner | leases jobs, runs agent CLIs, runs checks | same host, separate process | any host with logged-in CLIs |
| foreman session | an official CLI (your choice of harness and model) with brigada's MCP tools | started by the runner (headless) or by `brigada` (when you chat) | same |
| worker session | a **worker**: one instance of an official CLI, a harness plus a model, running headless. A task worker runs one task; the foreman is a long-lived worker (§4) | started by the runner | same |

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
| quota limit hit by a worker | rule | cool the account (§8); re-dispatch to another eligible agent as a **fresh** attempt (on the task's next `-r<n>` branch, §6.4) carrying the previous job's summary if it produced one, else its last progress events (a session can't move between agents), counting toward the task's attempt caps; no eligible agent → the task parks and the user is notified (§10) |
| worker `needs_help` | **foreman** | answer → next job on the same session, or re-plan |
| task out of rounds or retries | **foreman** | re-plan, switch agent, split, or ask the user |
| worker stalled (§5.6) | **foreman** (decided 2026-10-12: always; no automatic nudge) | restart with a new instruction, switch agent, or ask the user |
| plan above the approval limit | **user** | approve, edit or reject (§6.3) |
| foreman limit hit (§4.4) | **user** | wait, switch foreman, or continue without it (decided 2026-10-12) |
| job finished with `discoveries` (§5.4; proposal, 2026-10-14) | **foreman** | read them on the next turn as verbatim data in delimiters (§1.4); then `plan_amend`, put the fact into the spec of a task that hasn't started, or ignore it |
| plan done | rule | notify the user directly (§10); the foreman may add a summary on its next turn, skipped when it's `limited` or out of budget |

## 4. The foreman

**The foreman is a worker** (decided 2026-10-13). A worker is an instance of a headless harness plus a model (§2); what it does comes from its instructions, permissions and lifecycle. The foreman runs on the same machinery as any other worker: an official CLI driven through the same adapters (§5.2), with headless resume, transcripts, quota and limit detection, and stall handling. A headless foreman turn is a job (kind `foreman`, §11). Where task-specific rules say "worker" (§3, §5.3–5.5, §7, §8), they mean a task worker: the foreman job is excluded from dispatcher scoring, quota re-dispatch, review routing and the runner's checks; its harness and model are fixed by the user, and its limit goes to the user (§4.4). It differs from a task worker in three ways:

- **Instructions:** the foreman instructions, where a task worker gets the worker instructions (the result block of §5.4, `needs_help`, check discipline, no dispatch tools).
- **Permissions:** the `foreman` token, scoped to its project (plans, answers, restarts), where a task worker runs under the runner token with no plan scope (§12.1).
- **Lifecycle:** one long-lived resumable session per project (`foreman:<project>`), with harness and model chosen by the user, woken by events and attachable. A task worker's jobs all belong to one task and one session (`task:<id>`), its agent picked by the dispatcher.

Task workers never get the foreman instructions. A task worker that thinks its task should be split returns `needs_help` with a proposed split, and the foreman decides.

### 4.1 What it is

- An official CLI session, **harness and model chosen by the user** (decided 2026-10-12), e.g. Claude Code with a given model, with brigada's MCP tools (§12.2) and the foreman instructions (a versioned file shipped with brigada).
- One foreman per project (repo). Its session is a **resumable session key** `foreman:<project>` (decided 2026-10-12: option B). The harness must support headless session resume (claude, kimi, zcode: yes, verified in the Python implementation; codex: `exec resume`; gemini: to verify). Setup refuses a harness without resume.

### 4.2 Two ways in, one session

1. **You chat:** `brigada` attaches you to the foreman session through the harness's own interactive resume.
   - **Attach acquires the key's live slot first** (one live job per key, Python v3.2 §2). If a headless foreman turn is running, the client shows "foreman is busy" and waits for it to finish, or cancels it through fencing if you insist. Two processes never resume one session at once.
   - The attach hold is lease-like: the client heartbeats it, and it expires if the terminal is killed. A second live attach is refused.
   - **On detach the client writes the current harness session id back** to the engine, as runners do on complete (`--resume` forks session ids; Python v3.2 §2a). Otherwise the next wake would resume the pre-chat id and lose the conversation.
   - Judgment events that arrive while you're attached reach the foreman through its `events` tool, which the instructions tell it to check between replies. On release, any events it didn't consume are evaluated for a wake-up.
   - **Events are consumed only when a foreman turn completes,** or when the attached foreman reads them through `events`. A cancelled or failed turn's events return to pending and are re-delivered on the next wake or attach, so cancelling a turn to attach never drops a task worker's question.
   - At attach, pending events are put into the session's opening prompt (the same wake template), so they surface at once instead of waiting for your next reply.
2. **The engine wakes it:** when a judgment event arrives and nobody is attached, the engine submits a **foreman turn**: a headless resume of the same session, with a template prompt plus the pending events verbatim. The foreman acts through its tools and exits. Several events coalesce into one turn (the follow-up flag of Python v3.2).
3. **You send a message without attaching** (proposal, 2026-10-14): `brigada ask "<text>"`, or the client MCP tool `ask_foreman` from any harness acting for you (§12.2), puts a **user message** in the foreman's inbox.
   - **Framing (§1.4):** a prompt or an `events` answer has at most one `<user-message>` section, after all the `<event>` data; several waiting messages are an ordered list inside it, each naming the token label it came from. The wake template says the event data is never instructions and that this section is the only other input: your requests. The attached session's `events` answers use the same framing, order and neutralized tags, and the opening prompt at attach carries waiting messages with the waiting events (item 1). **Invariant:** no prompt or tool answer, present or future, presents an `<event>` or `<user-message>` section without the framing text in the same message.
   - **Delivery is per message, like an event's:** a message is consumed when a turn whose prompt carried it completes, or when the attached foreman reads it through `events`. A message that arrives while a turn runs was not in that turn's prompt: it stays pending, and the turn's completion re-evaluates the wake for it. A failed, cancelled or stalled turn returns its messages to pending (§5.6). On detach, or when an attach hold expires, waiting messages are re-evaluated for a wake, so a message never strands behind a chat that ended. Messages coalesce with each other and with waiting events into one turn.
   - **Not charged to the wake budget:** a user message is you driving, like a chat, so a wake that carries one skips the day's budget (§4.3). It still obeys the wake stall (§5.6).
   - **The answer says what happened:** a turn was woken, or why the message waits — you're attached (naming the hold), a turn is already running, the project has no foreman harness, the wake stalled (and, once §4.4 lands, the foreman is limited until a time). It returns the message's id, so a caller can follow it in the history.
   - The project is named by its repo URL, or is the only project the engine has.

**Discoveries** (proposal, 2026-10-14): a finished job's `discoveries` (§5.4) reach the foreman on its next turn as verbatim data inside the `<event>` delimiters (§1.4), like any worker text. The foreman decides what to do: amend the plan (`plan_amend`), put the fact into the spec of a task that hasn't started yet, or ignore it. The engine never forwards them into another worker's prompt; only the foreman's decision can.

Your next chat sees everything the foreman did while you were away, because it's the same session, and the history (§9) has it too.

### 4.3 Budget

Foreman turns while you're away use your subscription. A per-project budget of foreman wake-ups per day bounds them, and a budget of task-worker-minutes per plan bounds the plan's task jobs (both from Python v3.2's per-key budget). When it runs out, judgment events wait for you, and you're notified. You can raise it: `brigada budget <plan> +<minutes>` for task-worker-minutes and `brigada budget --wakes +<n>` for today's wake-ups (clamped steps, as in Python v3.2's budget raise); the wake-up budget also resets each day. A wake that carries a user message (§4.2 item 3) is not charged: the budget bounds the foreman reacting to events on its own, not you asking it something, which is as attended as a chat.

### 4.4 When the foreman hits its limit

The runner detects the limit-hit error from the foreman's CLI. The engine marks the foreman `limited(until)`, or `limited(unknown)` when the error carries no reset time, stops waking it, and **notifies the user** (§10), who chooses: wait for the reset, switch the foreman's harness or model (the new session rebuilds its context from the engine, per product §4's kill-and-rebuild test), or let approved plans continue without it. Routine rules keep working throughout. When the same account also serves task workers, a foreman limit-hit cools that account in the dispatcher (§8), and a worker limit-hit on that account stops foreman wakes until the reset.

### 4.5 Capacity

(decided 2026-10-13) The foreman instructions tell it to check `capacity` (§12.2) when it plans, and to use it to choose how wide the task graph is, when to sequence instead of fanning out, which tasks can wait for a cheaper agent, and to give honest time estimates ("3 tasks now, 2 after the reset at <time>"). The engine stays binding: if the foreman's view is stale, the dispatcher queues the extra tasks and never exceeds a cap (§8.1). The foreman decides; the engine enforces (§1).

## 5. Workers and runners

### 5.1 Runner

- Registers with a runner token and advertises its adapters: harness, models, family, account label, classes it can take (product §5 rule 5).
- Leases jobs (long-poll), heartbeats with progress, completes with a fencing token. Lease expiry plus fencing make crashes safe (Python v3.2's lease engine, ported with its tests).
- Materializes its own workspace: clone or fetch the repo URL with its own git credentials into a per-task worktree, and push result branches to the remote (product §5 rule 6).

### 5.2 Adapters

One per CLI: build the headless command (fresh or resume), parse the CLI's event stream into progress events, detect limit-hit and auth errors, find the session id for resume. Copied where useful from Orchestrator.inc's Apache-2.0 adapters, keeping upstream license headers, carrying their NOTICE and marking modified files (CodexBar, if used, is MIT: same idea). The claude adapter's default worker scope allows exactly `go build`, `go test`, `go vet` and `gofmt` — the worker compiles and tests its own work as it goes; the runner still runs the check, so no new code reaches execution — and an agent config's explicit tool scope replaces the default (decided 2026-10-15).

### 5.3 The worker prompt

A fixed worker template (versioned) plus the task spec verbatim, plus the answer or feedback for follow-up jobs. The template tells the worker to end with the result block (§5.4).

### 5.4 Worker result contract

The worker's final message ends with one JSON block:

```json
{"status": "done" | "needs_help" | "failed", "summary": "…", "question": "…", "branch": "…", "discoveries": "…"}
```

`discoveries` is optional (proposal, 2026-10-14). It holds facts the worker learned that matter beyond its own task: an API quirk, a flaky test, a wrong assumption in the plan, something a later task will trip over. An absent or empty field means none. A review's verdict block (§7) and foreman turns don't carry it. Workers never message each other, and the engine never forwards discoveries into another worker's prompt on its own. Only the foreman's decision can carry them (§4.2).

A missing or malformed block counts as `failed` with reason `no-result`. The runner verifies the branch was pushed.

The contract binds task jobs (`work`, `fix`, `review`, §11); a review job's block is the verdict block of §7. A foreman job is exempt: its turn's output is its tool calls (§12.2), and ending without a result block is not `failed`. On `needs_help`, `question` carries the question or a proposed split of the task; the foreman answers it or re-plans through `plan_amend` (§3, §6.2), so the split needs no field of its own.

### 5.5 Checks run by the runner

After the worker exits with `done`, **the runner runs the task's check** in the worktree, not the worker. Exit code plus the output tail are the check result. That keeps the outcome record honest (a self-reported check would bias it).

### 5.6 Stalled workers and restart with a new instruction

Workers sometimes get stuck: alive, but making no progress (seen in practice).

- **Detection:** the adapter turns the CLI's event stream into progress events (tool calls, messages); the runner sends the time of the last one in each heartbeat. **Stalled = 10 minutes with no activity at all** (decided 2026-10-12; `stall_minutes`, configurable). A tool call in flight (a long build or test run) suspends the clock, bounded by its own `tool_call_cap_minutes` per class (default 30). Stalled, or the class time cap reached → the runner kills the process tree and completes the job with the outcome `stalled` or `time_cap`; either ends the job in the terminal `stalled` status, and the result block names which, with the last progress events attached.
- A stalled job **always goes to the foreman** (decided 2026-10-12); there is no automatic nudge.
- A stalled foreman turn is killed and completed like any stalled job, but never routes to the foreman itself: its unconsumed events return to pending and are re-delivered on the next wake or attach (§4.2), the engine may wake it again while wake-up budget remains (§4.3), and the user is notified when it stalls repeatedly or the budget runs out.
- **Restart:** `restart(task, instruction, mode, agent?)` cancels any live job and creates a new job on the same task with the new instruction.
  - `mode=resume`: same session, the instruction appended (the worker keeps its context);
  - `mode=fresh`: a new session, the instruction plus the previous job's summary and last progress (for a worker stuck in a loop);
  - optional `agent` to move the task to another agent.
- **Who restarts:** the foreman (a judgment event), or the user directly: `brigada restart <task> -m "…" [--fresh] [--agent …]`. A restart is a new attempt on the task and counts toward its attempt caps. A resume that fails (the session is gone or forked) restarts fresh in place, as in Python v3.2 §2a.

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
- **In-flight work:** recover waits one lease-expiry window before re-dispatching, so runners whose heartbeats now fail have killed their workers. Re-dispatched work does not get a new branch name; the rules below decide which branch each kind of re-dispatch works on.
- **Branch names:** a task's first job works on `brigada/task-<id>`, and the engine names each job's branch.
  - **Re-lease** (the lease expired or the runner crashed, and the same job is leased again): keeps its job's branch.
  - **Restart** (§5.6, either mode, or a re-dispatch to another agent): works on `brigada/task-<id>-r<n>`, where `<n>` counts the task's restarts.
  - **Fix round** (§3): continues the task's branch, the one its last job pushed, so the fix lands on top of the work under review.

## 7. Review policy (in the core)

After an `implement` job is done and its check passed, the engine creates a review job for an agent of a **different family** than the author. The reviewer gets the diff (base…branch), the task spec and the check result, **never the implementer's summary** (Python v3.2 §7.1's anti-steering rule), and ends with a verdict block (`approve` | `changes`) plus findings. An approve without substantive findings text (v3.2's minimum length) counts as `no-verdict` and is retried with another reviewer, so lazy approvals don't inflate the outcome record. `changes` → a fix round (§3 rule) up to `review_rounds` (default 2), then a judgment event. Family detection: the job record; for `brigada review --branch` without a job, the `Co-Authored-By` trailer. That standalone command creates a one-task plan of class `review`: the runner runs the repo's configured check, and there are no fix rounds unless you ask. Merging is out of scope at launch: the foreman reports approved branches and the suggested merge order.

## 8. Dispatcher

- **Eligible:** the adapter supports the class; family rule for reviews; agent `ready` (not `broken`, not `cooling`); account not cooling; session affinity (a resumed session stays on its agent and runner).
- **Cap room:** an eligible agent is picked only if every concurrency cap on its path has room (§8.1). Caps shrink the set of eligible agents with room, so a task whose best agent is cap-blocked goes to the next-best agent with room; it waits in the queue only when no eligible agent has room, and that never parks it. **Parking** means no agent is eligible with caps ignored.
- **Score:** per (repo, class, agent): a success rate with a prior, where success = check passed and review approved within the rounds. Defaults for a cold start; an exploration share (default 10%) goes to the next-best eligible agent with room.
- **Quota:** limit-hit errors cool the whole account until its window resets; CodexBar's CLI JSON is an optional extra signal.
- **Health:** repeated failures, auth errors or a dead runner → `broken(reason)` until a probe passes or the user resets it.
- Every decision is recorded with its reason ("why this agent").

### 8.1 Concurrency caps

**Layered caps** (decided 2026-10-13). A job may start only if every layer has room; the effective capacity is the minimum across the layers:

- **Runner:** max parallel jobs per runner, for the host's CPU, memory and parallel builds and tests. The runner advertises its own cap when it registers; the effective runner cap is the smaller of that and the config's.
- **Account:** max parallel jobs per adapter account, for the vendor's session and rate limits; parallel sessions also drain the 5-hour window faster.
- **Model** (optional): a per-model cap, such as 1 for a heavy model.
- **Plan:** max parallel tasks per plan, because tasks touching the same files end in merge conflicts. A task counts while it has a live job (`running`, `checking` or `reviewing`, §6.1).
- **Global:** an overall ceiling for the engine.

A task job (a task's first run, a fix round or a review) that cannot start because some layer is full waits in the queue, oldest first, and starts when a slot frees; it does not park (§3) and does not notify the user.

**The foreman is never starved.** Foreman turns count toward the runner, account and global caps (not the plan or model caps), and a pending foreman turn outranks every queued task job at each of those layers: the next slot that frees on its path goes to it. Its wait is therefore bounded by the running jobs' class time caps (§5.6). An attached chat (§4.2) is not a job, but it is a live session on the foreman's account: it counts toward that account's cap while held, without ever waiting for room, so the dispatcher starts no new task job there until there is room again.

Each agent also carries a **cost weight** (default 1), a relative cost the user sets, which the foreman sees through `capacity` (§12.2) when it chooses which tasks can wait for a cheaper agent.

**Defaults** (proposal): conservative per adapter, all set in the engine's config file. An example:

```
caps.global = 6
caps.plan = 3
# a runner may advertise its own (--max-parallel); the smaller wins
caps.runner = 4
# per account label
caps.account.claude = 3
caps.account.codex = 2
caps.account.kimi = 1
caps.model.<heavy-model> = 1
# cost weight per agent, default 1
cost.<harness>/<model> = 2
```

Comments go on their own line; the parser does not strip trailing comments.

**Backlog: adaptive caps.** When an account hits its limit while running N parallel jobs, the engine lowers that account's cap automatically and records the change and its reason; the user can raise it again.

### 8.2 Foreman headroom

(proposal) The foreman's session uses quota on its own account, so task workers must not drain that account and leave the foreman `limited` mid-plan (§4.4). The dispatcher prefers other accounts for task workers, and stops dispatching task workers to the foreman's account above a reserve threshold: by default 80% of the 5-hour window used, when CodexBar data is available; without it, a lower parallel cap for task workers on that account: `max(cap − 1, 0)`, so one slot stays reserved for the foreman even while no foreman turn runs. At an account cap of 1 that is 0: task workers never run on the foreman's account.

## 9. History

Everything is kept and readable (decided 2026-10-12).

- **Messages:** one table of conversation items per project: user ⇄ foreman turns (a user message, §4.2 item 3, is kept with author `user`), foreman tool calls and their results, worker task specs and results, questions and answers, reviews, notifications. A job's `discoveries` (§5.4; proposal, 2026-10-14) stay with that job's result message. `brigada history [--plan|--task]` shows them as one timeline.
- **Transcripts:** after every job, the runner uploads the session's full transcript (the CLI's own event log) to the engine as an artifact, linked from the job. The foreman's chat transcript is archived the same way after each attach and each headless turn.
- **Events:** an append-only event log is the backbone. Messages and transcripts reference events.
- Retention is configurable; the default keeps everything.

## 10. Notifications (launch: local)

Local OS notifications plus a "needs you" queue in `brigada status`: plan approval, user-directed questions, the foreman's limit, the foreman's budget out, a broken agent, a task parked with no eligible agent, a plan done. Remote channels (webhook, Telegram) are in the backlog.

## 11. Data model (SQLite, one file)

`projects` (repo URL, default branch, settings, foreman harness and model) · `plans` · `tasks` (+ `task_deps`) · `jobs` (task, kind: `work`|`fix`|`review`|`foreman`, agent, mode, prompt, lease and fence, status, result, check result, last progress, transcript ref) · `sessions` (key, agent, runner, harness session id, state) · `runners` · `agents` (runner, adapter, harness, model, family, account, classes, health, cooling until) · `outcomes` · `events` (append-only) · `messages` · `questions` · `tokens` (hashed) · `artifacts` (transcripts, check logs).

## 12. Interfaces

### 12.1 Engine API

JSON over HTTP; locally on a Unix socket, remotely over TLS. Bearer tokens with roles: `client`, `foreman` (scoped to its project), `runner`, `admin`. Groups: projects, plans, tasks (create, amend, cancel, restart), questions (answer), foreman messages (a user message, §4.2 item 3: `client` and `admin` only, idempotent; a message identical to one the same token sent to the same project within the last minute is a duplicate — a double Enter, a shell retry — answered with the first one's id), events (long-poll and SSE), history, stats, budget, agents and runners, artifacts.

**Idempotency.** The caller is often an LLM whose tool calls may repeat. `plan_create`, `plan_amend`, `task_restart`, `answer` and a foreman message (§4.2 item 3) take an idempotency key (client-supplied, or derived: task + round for restart and answer); a repeat returns the first call's result. The key is minted by the `brigada mcp` bridge per logical tool call (a hash of the call's content within the session), not left to the LLM; identical `plan_create` content within a short window also dedupes. Task jobs are keyed `task:<id>`: one live job per key makes duplicate job creation coalesce into the follow-up flag, which `status` shows.

### 12.2 Foreman MCP tools

`plan_create` · `plan_amend` · `task_restart` · `task_cancel` · `answer` (a task worker's question) · `ask_user` · `events` (long-poll, for the attached session) · `status` · `stats` · `history` · `capacity`. `capacity` returns, per agent: its effective cap and the layer that sets it (§8.1; on the foreman's account, after headroom, §8.2), running jobs, quota state (`ready`, or `cooling` until a time, plus the percent used of the 5-hour and weekly windows when CodexBar data is available) and cost weight. It also returns each layer's cap, use and queue depth: per runner, account and any capped model, per active plan of the foreman's project, and global. A queued task has no agent yet, so queue depth is reported per layer, not per agent. The MCP server is a `brigada mcp` stdio process started by the foreman's harness; it calls the engine API with the foreman token. The token is lease-scoped (decided 2026-10-13): the engine mints a short-lived one when it leases a foreman turn — only its hash is stored, the runner keeps the secret in memory alone — and revokes it when the turn ends, so a runner never holds a long-lived foreman token.

**Client MCP tools** (proposal, 2026-10-14): `brigada mcp --client` is the same bridge for any other harness acting for the user, a coding session or a coordinator: `ask_foreman` (a user message, §4.2 item 3), `status`, `history`. It authenticates with a client token from the state directory and never holds a foreman token, so a harness can talk to the foreman and watch the work without a person relaying between the two. Every client token speaks with the user's voice, which is the point; the token file can hold several, each with its own label (one per device or harness) and revocable on its own, and the label travels with each message into the foreman's prompt and the history (§9), so it's clear which surface sent what. The roles can't close one residual boundary of the local model: any local process that can read the state directory, a task worker with a shell included, can use a client token. That was already true of every client action; it matters more now that a user message is a request by design, and containers (backlog) are the answer.

### 12.3 Runner protocol

`register` · `lease` (long-poll) · `heartbeat` (progress, last activity) · `complete` (result or error; an outcome of `stalled`/`time_cap` when the runner stopped the run itself, §5.6; check result; artifacts) · `release`. Fencing as in Python v3.2.

## 13. Engine lifecycle

Single instance (a lock on the store); started by any `brigada` command when not running; shuts down after `idle_minutes` with no live jobs, attached sessions, pending wake-ups or unsent notifications (it flushes them first); crash or reboot recovery through lease expiry and fencing, then pending foreman wake-ups are re-evaluated.

## 14. Porting from Python v3.2

Moves over with its tests: the lease engine with fencing, one live job per key, follow-up coalescing, the run cap and per-key budget, fresh/session modes and affinity, review after implement (from the connector into the core). Stays in the fleet: the merge gate, bot forks, worker images, egress proxy, chat-front-end connectors.

Transformations, so nothing is lost in translation:
- **`trigger` human/bot is obsolete:** there are no human-submitted jobs (your input is chat, not a job). Don't port it.
- **The run cap (bot rounds)** becomes per-task caps on fix, review and restart rounds, plus per-plan task-worker-minutes.
- **The per-key budget raise** becomes `brigada budget` (§4.3).
- **Keys:** task jobs `task:<id>`, the foreman `foreman:<project>`. One live job per key, coalescing, the follow-up flag and affinity hang off them.
- **Session write-back and resume-fail → restart in place** (§2a) are kept for task workers and for the foreman's attach (§4.2).

## 15. Phases

1. **Engine:** store, API, runner protocol, local runner, one adapter (claude), check by the runner, history (events, messages, transcripts), the concurrency caps (§8.1).
2. **Foreman, one task:** the foreman for one task end to end (attach and headless wake), review, `brigada status/history`, the `capacity` tool (§12.2) and foreman headroom (§8.2).
3. **Plans:** plans with dependencies, the approval limit, the plan file, the dispatcher with outcomes.
4. **Failures:** failure handling: stall detection, restart with new instruction, needs_help loop, the foreman's limit flow, notifications.
5. **Re-planning:** re-planning, the planner's outcomes.

## 16. Open questions

1. Gemini CLI's headless resume support.
2. The foreman's wake-up budget defaults.
3. `tool_call_cap_minutes` defaults per class.
4. Foreman entry point: in addition to the `brigada` command, a `/brigada` skill or MCP prompt in the user's own harness that opens or attaches the dedicated foreman session, subject to the attach discipline of §4.2. Decide in phase 2.

Settled 2026-10-12: stalled = 10 minutes with no activity; stalls always go to the foreman; plan files at `.brigada/plans/` on the `brigada/plans` branch by default, `main` as a setting.

Note for remote: §6.4 gives the engine push credentials to user repos; fine locally, re-examine for remote or multi-user use.
