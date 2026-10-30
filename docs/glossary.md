# brigada: glossary

The terms used by [product.md](product.md) and [design.md](design.md). Where a document uses one of these words, it means exactly what is written here; a document that needs a different sense says so in place. Section references (§) are to design.md.

## People and roles

- **User** ("you"): the person who runs brigada, talks to the foreman, and owns the subscriptions and accounts the workers use.
- **Foreman:** the worker that plans and decides for one project. It talks with the user, creates and amends plans, answers task workers' questions, and handles judgment events (those the table in §3 routes to it). It has the foreman instructions, the `foreman` token and one long-lived brigada-owned session per project (§4), which the user joins interactively through `brigada attach` (§4.2).
- **Coordinator bridge:** `brigada mcp --coordinator`, a session over several projects: the client tool set plus `list_projects`, `plan_approve` and `plan_reject`, never plan mechanics. It carries a client token, never a foreman credential (§4.6, §12.2).
- **Task worker:** a worker that runs jobs for one task (implement, review, answer, …). It has the worker instructions and ends each job with a result block (§5.3, §5.4).

## Execution

- **Harness:** an official, unmodified agent CLI, such as Claude Code, Codex CLI or Gemini CLI. brigada runs it **headless**: non-interactively, with the prompt as input and an event stream as output.
- **Model:** the LLM a harness runs with, such as a given Claude or GLM model.
- **Family:** the vendor lineage of a model (Claude, GPT, Gemini, Kimi, GLM, …). A review's placement is `cross_family` (the reviewer's family differs from the author's), `same_family`, `same_agent` (the author's own agent in a fresh session) or `unknown` (the author's family is not known); the `review.family` setting, `prefer` or `require`, decides whether the last three are allowed (§7).
- **Account:** the user's sign-in for a harness: one subscription with its own quota window. Several agents can share one account.
- **Agent:** a harness plus a model plus an account, registered by a runner, that the dispatcher can pick (§8, §11). It is a configuration, not a running process. Outside these docs' technical sections, "AI agent" can mean any LLM agent in the everyday sense; the product brief uses it that way for the user's subscriptions.
- **Worker:** an instance of a headless harness plus a model, running for brigada. A task worker runs on the agent the dispatcher picked; the foreman on the harness and model the user chose. What it does comes from its instructions, permissions and lifecycle; the foreman and task workers are both workers. Its continuity is its session; each run of it is a job. brigada runs a worker; an attached chat is the user joining one, not a second worker.
- **Adapter:** the code for one harness: it builds the headless command (fresh or resume), parses the harness's event stream into progress events, detects limit and auth errors, and finds the session id (§5.2).
- **Runner:** the process that leases jobs from the engine, prepares the workspace, starts workers through adapters, runs checks and reports results (§5.1).
- **Engine:** the background process with the API, the store, the rules, the dispatcher and foreman wake-ups. No LLM runs in it (§2).
- **Store:** the engine's SQLite file; authoritative for all state (§11).
- **Client:** anything that calls the engine API with a client token: the `brigada` command; other harnesses through MCP (the Model Context Protocol) (§12.1).

## Sessions

- **Session:** a harness conversation that brigada can resume. A worker's memory lives in it, so several jobs (fix rounds, foreman turns) can continue one session. The user joins the foreman's session through `brigada attach`; it stays brigada-owned, and the headless turns resume it between chats (§4.2).
- **Session key:** brigada's stable name for one task's work or the foreman's: `task:<id>` for task jobs, `foreman:<project>` for the foreman (§12.1). One live job per key. A task's key can hold several sessions in turn, such as the author's and a reviewer's, one live at a time.
- **Harness session id:** the harness's own id for the conversation. It can change on every resume (a resume may fork it), so the runner, and the client after an attach, write the current id back (§4.2).
- **Resume / fresh:** a job either continues the key's current session (resume) or starts a new one, carrying the previous context in its prompt (fresh). A resume that fails restarts fresh in place (§5.6).

## Work

- **Project:** one repository (a monorepo is one), identified by its canonical repo URL, with its own foreman, settings and records.
- **Plan:** a graph of tasks for one ask, with dependencies, a lifecycle and an approval rule (§6). Mirrored to the **plan file** in the repo (§6.4).
- **Plan file:** the mirror of a plan in the repo, `.brigada/plans/<plan-id>.yaml`, pushed to the `brigada/plans` branch on every plan change; the store stays authoritative and `brigada recover` reads it back (§6.4).
- **Task:** one unit of work in a plan: title, spec, check, class, dependencies, repo and base ref, caps, and the dep it continues, if any (§6.1).
- **Spec:** the task's instructions to the task worker, passed verbatim.
- **Class:** what kind of work a task is: `implement`, `review`, `answer`, …. Eligibility, scores, caps and the approval limit are per class.
- **Job:** one run of a worker, leased by one runner. Its **kind** says why it exists: `work` (a task's plain run, including a retry after a transient error), `fix` (a fix round), `review`, `land` (a landing, §7.1), or `foreman` (a foreman turn) (§11). A task usually has several jobs. **Task jobs** are the kinds `work`, `fix` and `review`, keyed `task:<id>`; the foreman's are keyed `foreman:<project>`.
- **Foreman turn:** a job of kind `foreman`: a headless resume of the foreman's session with the pending events (§4.2).
- **Attach:** the interactive foreman: the user joining the foreman's brigada-owned session (`foreman:<project>`) through `brigada attach`, holding the key's live slot while attached; detach writes the session id back and re-evaluates waiting events for a wake (§4.2).
- **Wake:** the engine submitting a foreman turn because a judgment event arrived and nobody is attached.
- **Bridge:** the `brigada mcp` stdio process a harness starts; it exposes one tool set (project foreman, ordinary client, or coordinator, §12.2).
- **Canonical repo URL:** the normalized `host[:port]/path` that names a project (§13.1).
- **Project config:** the committed, nonsecret `.brigada/project.yaml` (schema 1 with explicit import); the store keeps the effective settings (§13.1).
- **Default check:** a project's check that a task may inherit by omitting its own; copied into the task when it is created (§6.1).
- **Round:** one more job on the same task and session in answer to feedback: a fix round after a failed check or a review that asked for changes.
- **Attempt:** a new try at a task other than a feedback round, decided by the foreman, the user, the quota rule or `brigada recover` (§6.4): a restart (either mode) or a re-dispatch to another agent. A resumed restart continues the old session; a fresh restart or a re-dispatch starts a new one. Rounds and attempts both count toward the task's caps.
- **Check:** the task's command (tests, lint, a script) that the runner runs after the worker reports `done`. Its exit code decides, not the worker (§5.5).
- **Review:** a job, by an agent of a different family when one is available (else the author's family or own agent under `review.family=prefer`, labelled by its placement), over the diff, the spec and the check result, ending with a verdict: `approve` or `changes` (§7).
- **Plan review:** a `plan_review` job, placed on another family than the foreman's when one is available (else the foreman's family or own agent under `review.family=prefer`), over a new plan, or an amendment that adds or respecifies tasks, before any of its work is queued. It ends with a verdict block (`approve` or `changes`) and at most two rounds; a `changes` verdict with a round left parks the plan at `needs_foreman` for the foreman's `plan_amend` or `plan_accept` (§6.2, §7.2).
- **Result block:** the JSON a task worker's final message ends with: `status` (`done`, `needs_help`, `failed`), `summary`, `question`, `branch` (§5.4). A review job's block is the verdict block.
- **Verdict block:** the block a review job's final message ends with: the verdict (`approve` or `changes`) plus findings. An approve without substantive findings counts as `no-verdict` and is retried with another reviewer (§7).
- **`needs_help`:** a task worker's status when it needs a decision. Its `question` carries the question or a proposed split, and the foreman decides.
- **Outcome:** the recorded result of a task on an agent (check passed, review approved within the rounds, or not). Outcomes feed the dispatcher's scores and `brigada stats`.
- **Gate:** one of the three points where the loop may wait for the user: `plan` (before a plan's tasks are queued), `land` (before an approved task is squashed onto the target branch), `push` (before the target is pushed). Each is `auto` or `ask` (`push` also `never`); a **rule** (a flat key in the project file, such as a protected path or a tier) turns `auto` into `ask` for the plan or task that trips it. Enforced by the engine; no foreman tool passes one (§6.3).
- **Land job:** a job of kind `land`, with no worker: a runner squashes one approved task onto the target branch, reruns the project's default check on the result, and pushes or waits by the push gate. Created by a rule when the task is done, approved and its dependencies are landed; a conflict or a failed check parks the task for the foreman (§7.1). `brigada land` is the hand-driven form.

## Leases

- **Lease:** a runner's time-limited claim on a job, kept alive by **heartbeats** that also carry progress (§12.3).
- **Live slot:** the one live job or attach a session key may have at a time.
- **Session affinity:** a job that resumes a session stays on that session's agent and runner (§8).
- **Fencing:** every heartbeat and complete carries the lease's token; a stale holder is rejected. The **fence epoch** is a store-wide counter that `brigada recover` bumps, so pre-recovery leases are rejected too (§6.4).
- **Follow-up flag:** set when work arrives for a key whose job is live; it coalesces into one follow-up job instead of a duplicate.

## Routing and limits

- **Dispatcher:** the engine code that picks an agent for each task job: which agents are **eligible** (agent `ready`, meaning neither broken nor cooling, account not cooling, the class supported, the family rule for reviews, session affinity), then, among those with room under every concurrency cap, the first in a sampled order: each agent draws a success rate from its record per repo, class and agent (its **score** is that record's mean, shown in `stats`), and the agents rank by cost weight divided by the draw (§8).
- **Quota:** what is left of an account's subscription window.
- **Limit hit:** a harness error saying the account's quota is used up. It **cools** the account: no agent on it is picked until the window resets (§8). For the foreman, the engine marks it **limited** and the user decides (§4.4).
- **Parked:** a task with no eligible agent when caps are ignored: it waits, and the user is notified (§3, §10). A task blocked only by full caps is queued instead.
- **Queued:** a task whose next job cannot start because some concurrency cap is full. It starts when a slot frees, oldest first, and notifies nobody (§8.1).
- **Broken:** an agent taken out of dispatch after repeated failures, auth errors, a model the provider rejects or a dead runner, until a probe passes (repeated failures only), the runner returns (a dead runner) or the user resets it (§8).
- **Stall:** a worker alive but with no activity for `stall_minutes` (default 10). The runner kills it and completes the job as `stalled` (§5.6).
- **Cap:** a hard bound on rounds, attempts, time or parallel jobs: per-task round caps, the class time cap, `tool_call_cap_minutes`, the concurrency caps.
- **Concurrency cap:** a limit on parallel jobs at one layer: runner, account, model, plan or global. A job starts only if every layer has room; a task blocked only by full caps is queued and does not park; a pending foreman turn outranks queued task jobs (§8.1).
- **Capacity:** what the foreman sees through the `capacity` tool: per agent, the effective cap, running jobs, quota state and cost weight; per layer, cap, use and queue depth (§12.2).
- **Cost weight:** a relative cost per agent that the user sets (default 1), so the foreman can let tasks wait for a cheaper agent (§8.1).
- **Foreman headroom:** a slot kept free on the foreman's account so task workers don't leave the foreman limited mid-plan (§8.2).
- **Budget:** a spending bound the user can raise: per project, foreman wake-ups per day (§4.3).

## Events and history

- **Event:** a recorded state change, appended to the event log (§9). **Routine** events are handled by rules in the engine; **judgment** events go to the foreman, or to the user where the table in §3 says so.
- **Message:** a conversation item kept in the history: user and foreman turns, tool calls, task specs and results, questions and answers, reviews, notifications (§9).
- **Artifact:** a file the runner uploads for a job: a transcript or a check log.
- **Transcript:** the full event log of a worker's session, as the harness wrote it.
- **History:** messages, events and artifacts as one timeline (`brigada history`).

## Access

- **Token:** a bearer credential for the engine API with one role: `client`, `foreman` (scoped to its project), `runner` or `admin` (§12.1). The foreman token is lease-scoped for headless turns and hold-scoped for an attached chat (§12.1).
- **Idempotency key:** a key on a repeatable call (`plan_create`, `plan_amend`, `plan_accept`, `task_restart`, `answer`), so a repeated tool call returns the first result instead of acting twice (§12.1).
