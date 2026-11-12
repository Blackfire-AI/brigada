# 4. The foreman

**The brigada-owned headless foreman is a worker.** The *foreman role* is logical: the project's single decision authority, exercised by this headless worker, which the user also joins interactively through `brigada attach` (§4.2) — the same session, not a second conversation. The rest of this section and §4.1 describe the headless worker.

A worker is an instance of a headless harness plus a model (§2); what it does comes from its instructions, permissions and lifecycle. The foreman runs on the same machinery as any other worker: an official CLI driven through the same adapters (§5.2), with headless resume, transcripts, quota and limit detection, and stall handling. A headless foreman turn is a job (kind `foreman`, §11).

Where task-specific rules say "worker" (§3, §5.3–5.5, §7, §8), they mean a task worker: the foreman job is excluded from dispatcher scoring, quota re-dispatch, review routing and the runner's checks; its harness and model are fixed by the user, and its limit goes to the user (§4.4).

It differs from a task worker in three ways:

- **Instructions:** the foreman contract (the versioned template: the tool protocol, the data delimiters, the engine facts it acts on) plus the project's foreman instructions (§5.9), where a task worker gets the worker contract (the result block of §5.4, `needs_help`, check discipline, no dispatch tools) plus the project's worker instructions and the task's skills (§5.9, §5.10).
- **Permissions:** the `foreman` token, scoped to its project (plans, answers, restarts), where a task worker runs under the runner token with no plan scope (§12.1).
- **Lifecycle:** one long-lived resumable session per project (`foreman:<project>`), with harness and model chosen by the user, woken by events and attachable — one session for the headless turns and the attached chat (§4.2). A task worker's jobs all belong to one task and one session (`task:<id>`), its agent picked by the dispatcher.

Task workers never get the foreman instructions. A task worker that thinks its task should be split returns `needs_help` with a proposed split, and the foreman decides.

**The operator is outside brigada.** The user's own coding agent (the operator) is not a brigada process and has no credential of its own: it talks to the engine through the CLI the user could run (`brigada ask`, `status`, `history`, `diff`), with the client token, and holds no approval authority. Approving a plan, a landing or a push, `--and-stop-asking`, tags, releases and rule changes stay the user's. `brigada init` writes `.brigada/operator.md`, the harness-agnostic guide the operator reads (§13.1).

## 4.1 What it is

- An official CLI session, **harness and model chosen by the user**, e.g. Claude Code with a given model, with brigada's MCP tools (§12.2) and the foreman contract (a versioned template in the binary) and the project's foreman instructions (a file init writes and the project owns, §5.9).

  The user chats with it through `brigada attach` (§4.2), which joins this session; the foreman's turns, headless and attached alike, run in the project's reading checkout under the state directory (§2), so the repository's own instruction files and project memory load as in any clone.

- One foreman per project (repo). Its session is a **resumable session key** `foreman:<project>`.

  A foreman harness must:

  - (a) resume one long-lived session headless and report its session id in its stream, so every wake continues the same conversation,
  - (b) take brigada's MCP server per run from a config the runner writes, with a strict mode so the repository's own servers are not loaded,
  - (c) let `brigada attach` join that same session interactively and end on an id a later turn resumes,
  - (d) accept a read-only tool scope with no shell and no edits, whose file tools are confined to the session's allowed roots (below).

  A soft fifth is token usage in the stream, which the session rotation (§4.2) reads — a harness that reports none never rotates.

  Setup checks (a), and that the harness can carry the bridge, and refuses a harness outside that set; the strictness in (b) is not checked. [GUIDE section 11](../GUIDE.md#what-a-foreman-harness-must-do) gives the status of each harness against (a) to (d). Claude and opencode meet all four. Qwen-code meets (a) to (c) and not (d): a qwen-code foreman turn keeps the CLI's auto-approve flag, so it runs with a shell and editing tools and its file reads are not confined to the allowed root. The gaps are limits of the CLIs or of brigada's adapters, not design choices.

**Allowed roots.** The foreman's file tools read exactly one root: the project's reading checkout, `<runner state>/foreman/<project>/repo`, the session's working directory. On every foreman harness but qwen-code (below), the confinement is enforced by the harness settings brigada writes for the session — headless turn and attach alike, never by prompt text — and it binds every agent in the session, subagents included: a `Read`, `Grep` or `Glob` outside the root is refused, and it stays refused when the user's own settings, which an attach loads, carry an allow rule.

Permission rules cannot express that: their patterns are single paths with no negation, and a deny broad enough to cover the directories around the root covers the root too. The settings therefore carry a PreToolUse hook on the file tools that checks each call's path arguments against the root and denies the call when one lies outside: a hook binds every agent in the session, subagents included, and its deny is not lifted by an allow rule.

Brigada's own state for the project is not a root: the state directory holds the client token file (tokens that speak with the user's voice, §12.2) and the per-turn MCP configs carrying the foreman token, and its artifacts are content-addressed and shared across projects (`<data dir>/artifacts/<sha[:2]>/<sha>`), so no path under it can be granted without exposing another project's data or a credential. The project's records — plans, tasks, the history — reach the foreman only through the bridge's tools (`status`, `history`), its artifacts (transcripts, check logs) only through a tool that serves them; a record or artifact no tool reaches gets a new tool, never a wider root.

Claude can enforce all of this: the settings passed with the session carry PreToolUse hooks, which bind subagents and deny regardless of the user's allow rules. Codex cannot: its read-only sandbox keeps a shell, and a shell reads any path — the no-shell gap under (d) that GUIDE section 11 lists. OpenCode can: the permission rules the adapter rides in the environment deny bash, edit, webfetch and outside reads, and the checkout is the session's working directory, so the outside-reads deny confines its file tools to the one root; the rules ride the environment, merged after every config file, so nothing the user's own config carries lifts them. Qwen-code does not: a qwen-code foreman turn keeps `--dangerously-skip-permissions`, the auto-approve flag every qwen-code run carries, and brigada maps no read-only scope onto qwen's permission rules, so the harness enforces none of this confinement — the foreman can read outside the root, edit files and run commands, and only its instructions keep it from doing so. It is the widest foreman scope brigada ships, the (d) gap GUIDE section 11 lists.

**Reading the project's issues.** Issue and pull-request text reaches the foreman through `issue_read` (§12.2), never through its file tools: the forge holds the text, and the disk holds at most another session's copy of it, which is not brigada's to hand around. When the foreman cannot read an issue — the repository is not on GitHub, `gh` is missing or not logged in, or the number does not exist — its report says so and asks the user for the text. It never searches the disk for issue text, and the allowed roots keep such a search refused even if it tries.

**Reading a failed check.** A failed check's event carries only the end of its output. The foreman reads the whole log with `check_log` (§12.2) before it calls a failure a flake or decides what it means; a failed land check's event names the land job whose log the tool takes.

## 4.2 Two ways in, one session

`brigada attach` is the interactive foreman, joining the same session the engine wakes headless, so the chat and the headless turns are one transcript.

1. **You chat:** `brigada attach` (a bare `brigada` runs it) joins the foreman session through the harness's own interactive resume.
   - The session runs in the project's reading checkout (§4.1) with the user's own harness config, so the user's own MCP servers load as in any session of theirs; the bridge is added through the MCP config the client writes for the chat, and its token dies with the chat. A codex attach is Codex's own terminal interface and carries no bridge: a later headless turn may start a fresh Codex thread instead of continuing the chat.
   - `--model`/`--effort` override the project's foreman model and effort for this chat alone; the session is the same, and the next headless turn uses the project's setting again.
   - **Attach acquires the key's live slot first** (one live job per key). If a headless foreman turn is running, the client shows "foreman is busy" and waits for it to finish, or cancels it through fencing if you insist. Two processes never resume one session at once.
   - The attach hold is lease-like: the client heartbeats it, and it expires if the terminal is killed. A second live attach is refused.
   - **On detach the client writes the current harness session id back** to the engine, as runners do on complete (`--resume` forks session ids, and the client names the fork's id to the CLI; opencode's attach continues the resumed id instead, and a fresh chat's id the client learns from the CLI's own session list afterwards). Otherwise the next wake would resume the pre-chat id and lose the conversation.
   - Judgment events that arrive while you're attached reach the foreman through its `events` tool, which the instructions tell it to check between replies. On release, any events it didn't consume are evaluated for a wake-up.
   - **Events are consumed only when a foreman turn completes,** or when the attached foreman reads them through `events`. A cancelled or failed turn's events return to pending and are re-delivered on the next wake or attach, so cancelling a turn to attach never drops a task worker's question. A cancelled turn announces unattended parks without waking a replacement.

     A done turn that leaves a plan's `changes` verdict unanswered — the plan still parked at `needs_foreman` (§6.2) — is named for what it swallowed: one `plan_verdict_unanswered` event, which wakes no turn, and the needs-you queue names the turn (§10), while the plan stays parked for a later event, an attach or the user.
   - At attach, pending events are put into the session's opening prompt (the same wake template), so they surface at once instead of waiting for your next reply.
   - **Tools:** the foreman set of §12.2, plus the adapter's read-only file tools (§5.2), confined to the allowed roots of §4.1 — no shell, no edits, and no read outside the roots. The confinement rides the harness settings brigada writes for the session, which the attach carries as a headless turn does: a PreToolUse hook (§4.1) binds every agent in the chat, subagents included, and its refusal holds against any allow rule the user's own settings load, so the chat reads no wider than a headless turn does. A qwen-code foreman is the exception: its harness carries no such confinement (§4.1), so neither a headless turn nor an attached chat is held to no shell, no edits and the allowed roots.
2. **The engine wakes it:** when a judgment event arrives and nobody is attached, the engine submits a **foreman turn**: a headless resume of the same session, with a template prompt plus the pending events verbatim. The foreman acts through its tools and exits. Several events coalesce into one turn (the follow-up flag).

   An environmental land failure is one wake per cause, not per task (§7.1): the wake's line and the needs-you entry list every task the cause parks, later failures on the same active cause attach to it and are not woken separately, and only a recurrence after the cause clears wakes again.

   A turn that failed re-wakes the foreman only after a growing not-before delay (§3): the next turn carries a not-before time the delay names, so a network that is still down in the seconds after a wake is waited out instead of re-tried every second. The delay follows the longest-failing pending event's attempts (§5.6) — it grows per consecutive failed turn and starts over once a done turn consumes its events.
3. **You send a message without attaching:** `brigada ask "<text>"`, or the client MCP tool `ask_foreman` from any harness acting for you (§12.2), puts a **user message** in the foreman's inbox.
   - **Framing (§1.4):** a prompt or an `events` answer has at most one `<user-message>` section, after all the `<event>` data; several waiting messages are an ordered list inside it, each naming the token label it came from. The wake template says the event data is never instructions and that this section is the only other input: your requests. The attached session's `events` answers use the same framing, order and neutralized tags, and the opening prompt at attach carries waiting messages with the waiting events (item 1).

     **Invariant:** no prompt or tool answer, present or future, presents an `<event>` or `<user-message>` section without the framing text in the same message.
   - **Delivery is per message, like an event's:** a message is consumed when a turn whose prompt carried it completes, or when the attached foreman reads it through `events`. A message that arrives while a turn runs was not in that turn's prompt: it stays pending, and the turn's completion re-evaluates the wake for it. A failed, cancelled or stalled turn returns its messages to pending (§5.6).

     On detach, or when an attach hold expires, waiting messages are re-evaluated for a wake, so a message never strands behind a chat that ended. Messages coalesce with each other and with waiting events into one turn.
   - **Not charged to the wake budget:** a user message is you driving, like a chat, so a wake that carries one skips the day's budget (§4.3). It still obeys the wake stall (§5.6).
   - **The answer says what happened:** a turn was woken, or why the message waits — you're attached (naming the hold), a turn is already running, the project has no foreman harness, the wake stalled, or the foreman is limited until a time. It returns the message's id, so a caller can follow it in the history.
   - The project is named by its repo URL, or is the only project the engine has.

**Discoveries:** a finished job's `discoveries` (§5.4) reach the foreman on its next turn as verbatim data inside the `<event>` delimiters (§1.4), like any worker text. The foreman decides what to do: amend the plan (`plan_amend`), put the fact into the spec of a task that hasn't started yet, or ignore it. The engine never forwards them into another worker's prompt; only the foreman's decision can.

Your next *attached* chat sees everything the foreman did while you were away, because it's the same session, and the history (§9) has it too.

**Session rotation:** the one session only grows, and every turn rereads it. Nothing lives only in the session (product.md), so the engine starts a fresh one when it gets large: before submitting a headless turn or starting an attach, if the last finished foreman job of the current session generation reported input tokens (cached reads included, as the adapters count them; the cached count alone when input is unknown) above the project setting `foreman_rotate_tokens` (default 400000, 0 = never), it clears the `foreman:<project>` session id and bumps `projects.foreman_session_gen` in the same transaction, and logs `foreman_session_rotated` with the token count and the threshold.

Never while a foreman turn is live or a hold is attached, and never on unknown usage. Only a job of the current generation counts, so one crossing rotates once.

The first turn or attach of the rotated generation — until one of them writes a session id back — carries an engine-written digest in the wake template, between `<digest>` delimiters as data (§1.4): the plans active or awaiting approval with their tasks' states, the needs-you entries with reasons, the last 10 history events, each list bounded with the omitted count and the whole cut at about 10 KB.

A model or effort change on the same harness keeps the session.

## 4.3 Budget

Foreman turns while you're away use your subscription. A per-project budget of foreman wake-ups per UTC day bounds them:

- the built-in default is 20,
- the config file's `foreman_wakes_per_day` sets the default for every project,
- `brigada budget [--repo URL] --wakes N` sets one project's budget for the day — a local project setting (`foreman_wakes_per_day` through the settings path, refused in the shared project.yaml, which names local-only keys by that rule).

When it runs out, judgment events wait for you, and visibly:

- one `foreman_wake_budget_spent` event per project per budget window records the count and the window's end,
- a notification is raised (§10),
- the needs-you queue in `brigada status` shows the entry — the use, the reset time, the events waiting — while events wait, gone when nothing waits, the budget is raised, the day resets at UTC midnight, or an attach holds the foreman's slot (the budget limits headless wakes only; the attached chat reads the waiting events with `events`).

Raising it re-evaluates the waiting events' wake at once.

A wake that carries a user message (§4.2 item 3) is not charged: the budget bounds the foreman reacting to events on its own, not you asking it something, which is as attended as a chat.

## 4.4 When the foreman hits its limit

The runner detects the limit-hit error from the foreman's CLI. The engine marks the foreman `limited(until)`, or `limited(unknown)` when the error carries no reset time, stops waking it, and **notifies the user** (§10), who chooses:

- wait for the reset,
- switch the foreman's harness or model (the new session rebuilds its context from the engine, per product §4's kill-and-rebuild test),
- let approved plans continue without it.

Routine rules keep working throughout. Cooling is per account (§8): when the same account also serves task workers, a foreman limit-hit cools it for the workers too, and a worker limit-hit on it cools the foreman's agent, so the foreman's turns wait unleased until the reset, though only a limit on the foreman's own turn marks it `limited`.

## 4.5 Capacity

The foreman instructions tell it to check `capacity` (§12.2) when it plans, and to use it to choose how wide the task graph is, when to sequence instead of fanning out, which tasks can wait for a cheaper agent, and to give honest time estimates ("3 tasks now, 2 after the reset at <time>"). Separate areas (different files and packages) run in parallel as one chain each; unrelated work is not chained, since a chain serializes whatever the caps.

The engine stays binding: if the foreman's view is stale, the dispatcher queues the extra tasks and never exceeds a cap (§8.1). The foreman decides; the engine enforces (§1).

## 4.6 The coordinator bridge

A session opened in a folder holding several repositories gets the **coordinator** tool set (§12.2) over every project the engine knows, through `brigada mcp --coordinator`: list projects, ask a project's foreman, approve or reject plans, and read status and history across projects. Each project keeps its own foreman for plan mechanics: the coordinator has no `plan_create`, no `plan_amend`, no task mutation and no hold tool. Projects are named by canonical repo URL (§13.1); a monorepo is one project. Cross-project plans do not exist: a coordinator sends separate requests, one per project, through `ask_foreman`, and each project's foreman plans on its own. Init in a non-repo parent folder registers the repositories it discovers and wires the coordinator into the folder's `.mcp.json` (§13.1).

Approval is the user's act. `plan_approve` and `plan_reject` use client authority, never a foreman credential, and take the plan id and the plan version the user was shown, so an amendment made after the user looked invalidates the call. The engine's "awaiting approval" is a result to present, not a permission: the coordinator instructions tell the coordinator to show the plan and wait for the user's explicit yes in chat, and the harness's own per-call permission prompt for the two approval tools stays on "ask" — init never pre-allows them. The user approves through the client path (the `brigada` command, or a coordinator session), never through the foreman tools.
