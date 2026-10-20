# brigada: product brief

Status: **v3**, 2026-10-20. It replaces a first brief written for an earlier Python implementation. Terms are defined in [glossary.md](glossary.md). The contract is [design.md](design.md); where this brief and the design disagree, the design wins until amended. Tags: **(decided, date)** is settled; **(proposal)** is open.

## 1. One line

**Talk to a foreman. It splits your work into tasks, gives each one to the AI agent that does that kind of task best on your repo, checks and cross-reviews the result, and learns from every outcome.**

## 2. Who it's for and their pain

A developer who pays for two or more AI coding subscriptions (Claude, Codex, Gemini, Kimi, GLM, …). Today they are the foreman themselves:
1. They split work into agent-sized tasks by hand.
2. They pick an agent per task by gut feel, and switch CLIs when one hits its 5-hour or weekly limit.
3. They babysit the agent: rerun it, paste errors back, decide when to give up.
4. They don't know which agent is actually best on *their* repo for *this* kind of task.
5. Agents say "done" without running the tests, and nobody from another model family reviews the change.

## 3. What it does

**The foreman is the main feature** (decided 2026-10-12). You talk to it; it resolves everything else and asks you only when it needs a decision.

1. **Understands** your ask in conversation and asks back when it's unclear.
2. **Plans:** splits the ask into a **task graph** (decided 2026-10-12: tasks with dependencies at launch). Each task has a spec, a **check** command, a class (implement, review, answer, …), its dependencies and (decided 2026-10-19) a **tier**, how heavy a model it needs, and an **effort**, how hard that model should think. The plan is also written into the repo as a file, so it survives the loss of the engine store and can be read and reviewed like code (decided 2026-10-12; details in [design.md](design.md) §6.4).
3. **Confirms** the plan with you **only above a size or risk limit** you set (decided 2026-10-12); smaller plans start on their own.
4. **Dispatches** each task to an eligible agent chosen by sampling: each agent's measured record for that class on this repo, weighed against its configured cost, among agents with quota left and healthy. With no record yet (a new user or repo) the draws are wide, so every agent gets tried, and `stats` shows how confident each number is. The foreman says per task how heavy a model it needs (light, standard or heavy) and how hard it should think (decided 2026-10-19); the engine samples a model of that tier among the CLIs you allowed, by record weighed against cost (decided 2026-10-20), never leaving the tier by choice. A failed task first retries at a higher effort, then climbs a tier (decided 2026-10-20) ([design.md](design.md) §6.1, §8).
5. **Runs** the agent's official CLI headless in its own git worktree, signed in with your account.
6. **Checks** the work by running the task's check, never trusting "done".
7. **Reviews** every change with a model from a different family. Fix rounds are capped.
8. **Handles failures:** retries, moves the task to another agent, splits it further, or asks you.
9. **Reports** progress and results: branches, the suggested merge order, what needs you. You can **stop or cancel** a plan or a task at any time.
10. **Learns:** every outcome feeds the record, so the next pick is better. `brigada stats` shows who wins where.

## 4. How it's built

```
 you ⇄ foreman (an agent session: an official CLI + brigada's tools)
            │ tool calls (MCP)
            ▼
 engine (code, no LLM): task graph · job store and leases · dispatcher · quota · outcome store · review policy · caps
            │ lease / heartbeat / result
            ▼
 runner: worktree → agent CLI headless → check → branch
```

- **The foreman is an agent, not our own model loop.** It runs in an official CLI session (configurable: claude, codex, …) with brigada's MCP tools and foreman instructions — a brigada-owned session: the engine wakes it headless when work needs judgment, and you talk to it with `brigada attach`; the chat and the headless turns are one transcript. The headless foreman is a worker, as every task agent is: an instance of a headless harness plus a model, on the same machinery, differing only in its instructions, permissions and lifecycle (decided 2026-10-13). Its turns, headless and attached alike, run in a reading checkout of your repository under brigada's state directory, with your own harness config, so the conversation feels like your own session. A coordinator session in a parent folder (mode B) can also watch several projects ([design.md](design.md) §4.2, §4.6, §13.1).
- **The engine does the mechanics in code:** who is eligible, healthy and has quota; leases and fencing; caps and budgets; the outcome record; review routing. The foreman decides; the engine enforces.
- **No state lives only in the foreman's session.** The plan, tasks, decisions, pending questions and outcomes are in the engine store (and the plan also in the repo), so a foreman session can end, crash or resume without losing work. Acceptance test: kill the foreman mid-plan; a fresh session rebuilds the plan, in-flight tasks and open questions from engine tools alone.
- **You choose the foreman's harness and model** (decided 2026-10-12), e.g. Claude Code with a given model. Init asks its effort too (decided 2026-10-19; shipped) (settable per project today), and for the workers you choose only which CLIs may run tasks and how many tasks run at once; brigada offers every model those CLIs list, and the foreman chooses the tier and effort per task ([design.md](design.md) §13.1). **When it hits its subscription limit, brigada notifies you and you decide** (decided 2026-10-12): wait for the reset, switch the foreman to another harness or model (the new session rebuilds everything from the engine, per the test above), or let approved plans continue without it. Without any foreman, the plain commands still work: `brigada status`, `stats`, `cancel`, and approved plans keep running.

## 5. Local first, remote-ready (decided 2026-10-12)

**Launch is local:** client, engine and runner on one machine. The engine is a background process that `brigada` starts on demand, because tasks run for many minutes after the conversation ends (proposal; this replaces v1's "no resident daemon"). It runs as a single instance (a lock on its store), shuts down when idle, and recovers from a crash or reboot through the lease protocol: leases expire, fencing holds, capped retries resume.

**Remote is in the backlog,** but the local version is built for it from day one, so remote later is a deployment change, not a rewrite. The rules (proposal):
1. API callers (the CLI, the foreman's MCP tools, other harnesses) talk to the engine **only through its API**, never the store directly, even on localhost.
2. Runners talk to the engine **only through the lease protocol** (lease, heartbeat, complete). The local runner is just a runner on the same host.
3. **Tokens from day one,** even locally: a client token and a runner token.
4. **No host-local assumptions in task data:** repos are identified by URL plus ref, not by laptop paths; results are pushed branches, not files on disk. A task's repo must have a remote to fetch from and push to; a local-only repo is rejected with a clear error.
5. **Runners register and advertise their adapters.** The dispatcher picks only from that registry; health and quota attach to it. A remote runner joins without touching dispatch.
6. **Runners materialize their own workspace:** clone or fetch from the repo URL with their own git credentials, push result branches to the remote, and send check logs with the lease-complete call. Never an engine-side checkout path or a shared disk.
7. **Checks are host-independent:** a repo-relative command whose environment comes from the repo, never a laptop path or tool, because every check is recorded as task data.
8. The engine's state is self-contained (one SQLite file plus its config), so it can move to a server.

**One core, many deployments.** This repo is the core. An earlier Python implementation stays private as a fleet deployment and the reference for what is ported, and later moves onto this core. Anything specific to one deployment (a particular forge, home servers, a chat front end) stays in that deployment or in a plugin, never here: the merge gate, bot forks, the egress proxy, worker images, implementer governance.

Remote later means: the engine on a server; runners on any host with logged-in CLIs; `brigada --server …` from any device; then GitHub issues as a source, notifications, and chat front ends as clients.

## 6. Launch scope (decided 2026-10-20)

In:
- the foreman as an agent in a brigada-owned session: `brigada attach` for the conversation, headless turns while nobody is attached — conversation, plan with dependencies, confirm above a limit, dispatch, watch, failure handling, stop and cancel, report;
- the coordinator session over several projects (mode B);
- `brigada init`, which sets a repo up in one step;
- `brigada land`, which squashes approved branches onto a local branch and stops before any push;
- tiers and effort per task, with the escalation ladder (effort, then tier) and cost-weighted dispatch;
- local notifications: an OS notification plus a "needs you" queue in `brigada status` (for the foreman's limit, plan approval, questions);
- `brigada review --branch <x>` without the foreman (the author's family from the job record or the `Co-Authored-By` trailer): it seeds the record and shows value on its own;
- the engine and a local runner;
- adapters for Claude Code, Codex, Kimi and opencode (Gemini waits for headless sign-in);
- check and cross-family review;
- the outcome record and `brigada stats`;
- quota from CodexBar's CLI (optional) plus limit-hit errors;
- an MCP server so other harnesses can call brigada.

**What local gives up:** worktrees instead of containers, so review jobs aren't enforced read-only, there's no per-job credential injection or egress restriction, and checks run in the user's shell with their credentials. No worse than running the CLI by hand, and said plainly in the docs.

Not at launch (backlog):
- remote engine and runners;
- GitHub issues as a source, notifications;
- a web UI, containers, auto-merge (`brigada land` never pushes);
- the Gemini adapter;
- connectors to chat front ends.

## 7. Alternatives and position

brigada is a standalone tool beside your own harness session: not a board, and not an engine for one.

| Alternative | What's missing for this user |
|---|---|
| Orchestrator.inc (13k★, Go, Apache-2.0): orchestrator, workers, 27+ harnesses, kanban | you pick the agent per task; no outcome or quota routing; desktop app; telemetry on by default |
| Multica (52k★): board, agents as teammates, remote runtimes | no routing, checks or cross-review; its license restricts embedding |
| Claude Code Projects, Codex cloud | one vendor each, cloud |
| Claude Code agent teams (experimental): a lead session, teammates, a shared task list, a mailbox; local | interactive only (no teammates in headless mode); one team per session with a fixed lead, in-process teammates not restored on resume; the lead approves teammates' plans without the user; single vendor; no outcome routing, no checks it runs itself, no cross-family review. A neighbour, not a substitute: it can't run inside brigada's headless workers |
| Gas Town, Symphony, vibe-kanban, claude-squad | orchestration without outcome routing |
| CodexBar (22k★) | shows quota only; brigada uses it as a source |

**Edge:** a foreman that routes by measured outcomes, runs checks, enforces cross-family review, works headless, and sends no telemetry. In our landscape check (2026-10-12) no tool routed by per-repo outcomes.

## 8. Terms of use (proposal)

- Official CLIs only, unmodified, signed in by the user through the vendor's own flow. Anthropic's Claude Code legal page ("Authentication and credential use", read 2026-10-12) allows an end user to sign in to the unmodified binary with their own subscription, including where it is hosted. Recheck every vendor's terms before launch.
- No account rotation, no credential pooling, no reselling, no telemetry: the outcome record and everything else stay on your machine (or your server, later).
- An API-key mode per adapter as a fallback.

## 9. Stack

**Go** (decided 2026-10-12), one binary, SQLite. CLI adapters may be copied from Orchestrator.inc (Apache-2.0) with attribution. The reviewed engine of the earlier Python implementation moves over piece by piece with its tests: lease and fencing, caps and budgets, modes, review after implement.

## 10. Validation and metric (proposal)

- A quiet public release with a few users, then the launch posts: a 30-second demo (the foreman splitting a real ask, plus `brigada stats`) and a problem post (HN, r/ClaudeAI, r/LocalLLaMA).
- Dogfood gate: continue past the smallest version only if the maintainers use it daily for their own repo work.
- Metric: weekly active installs, and repos with 50+ recorded outcomes.

## 11. Risks

| Risk | Mitigation |
|---|---|
| The foreman splits work badly | approval above a size or risk limit; the planner's own outcomes are recorded; re-planning when a task fails |
| The foreman's subscription hits its limit | you're notified and decide (wait, switch harness or model, or continue without it); plain commands and approved plans work without it |
| No outcome data on a fresh install | defaults plus wide draws from a thin record, confidence shown in `stats`; standalone `review` seeds the record |
| Scope: foreman plus engine plus adapters | phase it: one task end to end first, then plans with dependencies, then failure handling and re-planning |
| Vendor terms on headless subscription use change | official CLIs, own accounts; an API-key fallback |
| Untrusted repo content runs with your logins | document it; containers later |
| The local design blocks remote later | the §5 rules, checked in every design review |

## 12. Open decisions

Decided (2026-10-12): you choose the foreman's harness and model, and decide what happens when it hits its limit; plan approval only above a size or risk limit; tasks with dependencies at launch; the plan is also written into the repo (fault tolerance), at `.brigada/plans/` on the `brigada/plans` branch by default with `main` as a setting, the engine store being authoritative; license Apache-2.0 (in the repo since c8cb5c3).

Decided (2026-10-16): the harness you already use is the foreman (mode A; retired 2026-10-20 — `brigada attach` is the interactive foreman), or one session coordinates several projects (mode B); `brigada init` with a committed `.brigada/project.yaml`; `brigada land`, local only.

Decided (2026-10-19): tiers and effort per task, an agent per model, the tier fallback; init's foreman-first questions and tasks at once.

Decided (2026-10-20): cost-weighted dispatch, the escalation ladder of effort then tier, and the `AGENTS.md` router block with `.brigada/foreman.md`, with Codex wired through `.codex/config.toml` (that init wiring retired the same day with mode A: init writes `.brigada/project.yaml` only, design §13.1).

Open:
1. The size and risk limit for plan approval: how it's measured (task count, classes, files touched, protected paths).
2. ~~How the foreman learns about events while nobody is typing~~ Settled: with nobody attached the engine wakes a headless turn of the same session, and an attached chat reads `events` between replies ([design.md](design.md) §4.2).
3. Tiers and effort ([design.md](design.md) §16 item 12): the tier table as vendors rename models, and whether the effort levels reach a CLI's extremes.
