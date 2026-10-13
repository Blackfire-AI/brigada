# brigada: glossary

The terms used by [product.md](product.md) and [design.md](design.md). Where a document uses one of these words, it means exactly what is written here; a document that needs a different sense says so in place. Section references (§) are to design.md.

## People and roles

- **User** ("you"): the person who runs brigada, talks to the foreman, and owns the subscriptions and accounts the workers use.
- **Foreman:** the worker that plans and decides for one project. It talks with the user, creates and amends plans, answers task workers' questions, and handles judgment events (those the table in §3 routes to it). It has the foreman instructions, the `foreman` token and one long-lived session per project (§4).
- **Task worker:** a worker that runs jobs for one task (implement, review, answer, …). It has the worker instructions and ends each job with a result block (§5.3, §5.4).

## Execution

- **Harness:** an official, unmodified agent CLI, such as Claude Code, Codex CLI or Gemini CLI. brigada runs it **headless**: non-interactively, with the prompt as input and an event stream as output.
- **Model:** the LLM a harness runs with, such as a given Claude or GLM model.
- **Family:** the vendor lineage of a model (Claude, GPT, Gemini, Kimi, GLM, …). Cross-family review means the reviewer's family differs from the author's (§7).
- **Account:** the user's sign-in for a harness: one subscription with its own quota window. Several agents can share one account.
- **Agent:** a harness plus a model plus an account, registered by a runner, that the dispatcher can pick (§8, §11). It is a configuration, not a running process. Outside these docs' technical sections, "AI agent" can mean any LLM agent in the everyday sense; the product brief uses it that way for the user's subscriptions.
- **Worker:** an instance of a headless harness plus a model, running for brigada (decided 2026-10-13). A task worker runs on the agent the dispatcher picked; the foreman on the harness and model the user chose. What it does comes from its instructions, permissions and lifecycle; the foreman and task workers are both workers. Its continuity is its session; each run of it is a job.
- **Adapter:** the code for one harness: it builds the headless command (fresh or resume), parses the harness's event stream into progress events, detects limit and auth errors, and finds the session id (§5.2).
- **Runner:** the process that leases jobs from the engine, prepares the workspace, starts workers through adapters, runs checks and reports results (§5.1).
- **Engine:** the background process with the API, the store, the rules, the dispatcher and foreman wake-ups. No LLM runs in it (§2).
- **Store:** the engine's SQLite file; authoritative for all state (§11).
- **Client:** anything that calls the engine API with a client token: the `brigada` command; other harnesses through MCP (the Model Context Protocol) (§12.1).

## Sessions

- **Session:** a harness conversation that brigada can resume. A worker's memory lives in it, so several jobs (fix rounds, foreman turns) can continue one session.
- **Session key:** brigada's stable name for one task's work or the foreman's: `task:<id>` for task jobs, `foreman:<project>` for the foreman (§14). One live job per key. A task's key can hold several sessions in turn, such as the author's and a cross-family reviewer's, one live at a time.
- **Harness session id:** the harness's own id for the conversation. It can change on every resume (a resume may fork it), so the runner, and the client after an attach, write the current id back (§4.2).
- **Resume / fresh:** a job either continues the key's current session (resume) or starts a new one, carrying the previous context in its prompt (fresh). A resume that fails restarts fresh in place (§5.6).

## Work

- **Project:** one repository, identified by its URL, with its own foreman, settings and records.
- **Plan:** a graph of tasks for one ask, with dependencies, a lifecycle and an approval rule (§6). Mirrored to the **plan file** in the repo (§6.4).
- **Task:** one unit of work in a plan: title, spec, check, class, dependencies, repo and base ref, caps (§6.1).
- **Spec:** the task's instructions to the task worker, passed verbatim.
- **Class:** what kind of work a task is: `implement`, `review`, `answer`, …. Eligibility, scores, caps and the approval limit are per class.
- **Job:** one run of a worker, leased by one runner. Its **kind** says why it exists: `work` (a task's plain run, including a retry after a transient error), `fix` (a fix round), `review`, or `foreman` (a foreman turn) (§11). A task usually has several jobs. **Task jobs** are the kinds `work`, `fix` and `review`, keyed `task:<id>`; the foreman's are keyed `foreman:<project>`.
- **Foreman turn:** a job of kind `foreman`: a headless resume of the foreman's session with the pending events (§4.2).
- **Attach:** the user joining the foreman's session interactively through `brigada`, holding the key's live slot while attached (§4.2).
- **Wake:** the engine submitting a foreman turn because a judgment event arrived and nobody is attached.
- **Round:** one more job on the same task and session in answer to feedback: a fix round after a failed check or a review that asked for changes.
- **Attempt:** a new try at a task other than a feedback round, decided by the foreman, the user, the quota rule or `brigada recover` (§6.4): a restart (either mode) or a re-dispatch to another agent. A resumed restart continues the old session; a fresh restart or a re-dispatch starts a new one. Rounds and attempts both count toward the task's caps.
- **Check:** the task's command (tests, lint, a script) that the runner runs after the worker reports `done`. Its exit code decides, not the worker (§5.5).
- **Review:** a job by an agent of a different family over the diff, the spec and the check result, ending with a verdict: `approve` or `changes` (§7).
- **Result block:** the JSON a task worker's final message ends with: `status` (`done`, `needs_help`, `failed`), `summary`, `question`, `branch` (§5.4). A review job's block is the verdict block.
- **Verdict block:** the block a review job's final message ends with: the verdict (`approve` or `changes`) plus findings. An approve without substantive findings counts as `no-verdict` and is retried with another reviewer (§7).
- **`needs_help`:** a task worker's status when it needs a decision. Its `question` carries the question or a proposed split, and the foreman decides.
- **Outcome:** the recorded result of a task on an agent (check passed, review approved within the rounds, or not). Outcomes feed the dispatcher's scores and `brigada stats`.

## Leases

- **Lease:** a runner's time-limited claim on a job, kept alive by **heartbeats** that also carry progress (§12.3).
- **Live slot:** the one live job (or attach) a session key may have at a time.
- **Session affinity:** a job that resumes a session stays on that session's agent and runner (§8).
- **Fencing:** every heartbeat and complete carries the lease's token; a stale holder is rejected. The **fence epoch** is a store-wide counter that `brigada recover` bumps, so pre-recovery leases are rejected too (§6.4).
- **Follow-up flag:** set when work arrives for a key whose job is live; it coalesces into one follow-up job instead of a duplicate.

## Routing and limits

- **Dispatcher:** the engine code that picks an agent for each task job: which agents are **eligible** (agent `ready`, meaning neither broken nor cooling, account not cooling, the class supported, the family rule for reviews, under the parallel cap, session affinity), then the best **score** (a success rate per repo, class and agent), with a small **exploration share** for the next-best (§8).
- **Quota:** what is left of an account's subscription window.
- **Limit hit:** a harness error saying the account's quota is used up. It **cools** the account: no agent on it is picked until the window resets (§8). For the foreman, the engine marks it **limited** and the user decides (§4.4).
- **Parked:** a task with no eligible agent: it waits, and the user is notified (§3, §10).
- **Broken:** an agent taken out of dispatch after repeated failures, auth errors or a dead runner, until a probe passes or the user resets it (§8).
- **Stall:** a worker alive but with no activity for `stall_minutes` (default 10). The runner kills it and completes the job as `stalled` (§5.6).
- **Cap:** a hard bound on rounds, attempts or time: per-task round caps, the class time cap, `tool_call_cap_minutes`.
- **Budget:** a spending bound the user can raise: per project, foreman wake-ups per day; per plan, task-worker-minutes (§4.3).

## Events and history

- **Event:** a recorded state change, appended to the event log (§9). **Routine** events are handled by rules in the engine; **judgment** events go to the foreman, or to the user where the table in §3 says so.
- **Message:** a conversation item kept in the history: user and foreman turns, tool calls, task specs and results, questions and answers, reviews, notifications (§9).
- **Artifact:** a file the runner uploads for a job: a transcript or a check log.
- **Transcript:** the full event log of a worker's session, as the harness wrote it.
- **History:** messages, events and artifacts as one timeline (`brigada history`).

## Access

- **Token:** a bearer credential for the engine API with one role: `client`, `foreman` (scoped to its project), `runner` or `admin` (§12.1).
- **Idempotency key:** a key on a repeatable call (`plan_create`, `plan_amend`, `task_restart`, `answer`), so a repeated tool call returns the first result instead of acting twice (§12.1).
