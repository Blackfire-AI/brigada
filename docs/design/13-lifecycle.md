# 13. Engine lifecycle

Single instance (a lock on the store); started by any `brigada` command when not running; shuts down after `idle_minutes` with no live jobs, no attached chat and no open requests; crash or reboot recovery through lease expiry and fencing, then pending foreman wake-ups are re-evaluated.

Start-up runs the reopen pass of §6.2 first: a plan closed `done` while it still held live work — tasks parked at `needs_foreman`, their dependents waiting to land — returns to active once, its ids logged at info level, so the tools the guards gate read it again; a failure of the pass fails start-up as the neighbouring recovery calls do.

The main loop never waits on a notifier: the tick's delivery runs off the loop (§10), and the shutdown flush — and the wait on a delivery still in flight — is bounded by a short grace (ten seconds), past which the engine leaves without it and says so in the log.

**Suspension.** The engine and its runners sleep together: a laptop's sleep looks, to the sweep, like minutes or hours of wall clock in a single gap. The sweep therefore measures the wall-clock gap between its own ticks — the engine sweeps on the idle tick and before every call — and when the gap is far past the sweep interval (a few intervals, `suspensionFactor` in the code) it knows the engine itself was suspended.

Then it restarts every wall-clock deadline from the wake instead of acting on it:

- lease expiry,
- the kind's wall-time cap (`last_leased`, the current lease's clock, shifts past the gap, so the sleep never counts against it),
- the runner-stale window,
- attach-hold expiry,
- the queue TTL.

A deadline that lapsed during the gap proves nothing about a runner or a client that slept too. One `engine_suspended` event records the detection, naming the gap.

A tick counts only once the transaction that swept commits — an attempt that rolls back (a busy store at commit, an error later in the same transaction) leaves the gap open, so the retry re-detects the sleep — the engine's write transactions serialize through that commit bookkeeping, so concurrent sweeps behind one suspension detect it once and shift its deadlines once, and the sweep, its retention prune, a job read's sweep and a heartbeat each retry a busy store within their own deadline instead of failing the call — the prune apart from the sweep's maintenance transaction, so a busy prune never re-runs a committed pass. The retry is bounded: a store busy past it still fails the call, and the next tick or beat tries again.

## 13.1 `brigada init`

One step sets a repo up. The GUIDE's §1 describes the questions: the foreman model and effort, worker models, tasks at once, then the check. Init has two phases: it probes and prints what it would do (no writes), then applies on confirmation. Nothing is silent.

1. **Identity.** List the repo's git remotes and normalize the chosen URL to the **canonical repo URL**, the project's name. A single remote is used whatever its name; with several, `origin` is the default. With several and no `origin`, init asks which remote, or `--remote NAME` selects one; non-interactive init needs that flag. A local-only repo with no remote is rejected (product §5 rule 4).

   Scp-like SSH (`git@host:org/repo`), `ssh://` and HTTPS URLs of the same host and path give one project. Userinfo is dropped from the identity; a sanitized SSH user may remain in the stored transport, while passwords and HTTPS tokens are never stored, logged or shown. The host is lowercased; the path is not, because paths can be case-sensitive. One trailing `.git` and trailing slashes are removed. The scheme is dropped, and a port is kept only when it is not the scheme's default, so a non-default SSH port stays a different project from that host's HTTPS URL unless the user confirms they are the same.

   A single-word SSH host is resolved through `ssh -G` using its hostname and port. Init shows the alias, canonical identity and SSH transport and asks for confirmation; `--yes` accepts. If resolution fails or still gives a single-word host, init refuses and names `--repo`.

   `--repo` takes a full transport URL, or a bare canonical identity when the selected remote is SSH-like; the latter builds an SSH transport with selected remote's URL user, except when the identity's path ends in `.git`, which requires a full URL to preserve the remote path. Parent-folder init accepts resolved alias children with `--yes` and otherwise skips them with a `--repo` reason. Other ambiguous forms, including a leading-slash or `~` scp path, are refused with `--repo` as the escape.

   The canonical form is `host[:port]/path`. The engine stores a transport URL alongside the identity, and runners fetch with that transport and their own credentials (§5.1).

2. **Probe harnesses.** Find installed harness CLIs and check whether each is signed in, by the harness's own cheapest status check or by asking the user. Detected login state is local and never goes in the shared file. A binary name proves neither a subscription nor a model family: the family comes from the model an agent is configured with, and the user confirms which models an account serves.

3. **Agents and review families.** With zero usable agents init refuses, starts nothing and writes nothing. It counts distinct model *families* among review-capable agents, not CLIs: two CLIs on one family are one.

   Two families are the recommended setup under `review.family=prefer` (the default), where a review falls back to another agent of the author's family or to the author's own agent, visibly labelled (§7), and are required under `require`, where a review no other family can take parks.

   With fewer than two under `require` it does not report ready. It stops the normal setup with an actionable warning (sign in to a harness of another family, or add a model of one) and continues only on the user's explicit acknowledgement of a **limited setup** (`--accept-limited`). Under `prefer` it prints the same advice as a warning and goes on: reviews take the fallback.

   A setup with one family never disables the family rule (§7): under `require`, reviews it cannot serve are named in `status` and park as designed, and init never presents the limited setup as fully ready; under `prefer` they run on the fallback and are labelled `same_family` or `same_agent`.

4. **Foreman.** Pick the foreman harness and model from the usable agents (it needs headless resume, §4.1). The pick is shown and overridable.

5. **Defaults.** Suggest a default check from the repo's build files (for example `go.mod`, `package.json`, `Cargo.toml`, a Makefile target) and the per-agent `permission` default (§5.2). They are shown, overridable, and never executed during discovery. The check is a repo-relative command (product §5 rule 7). Init suggests the default §5.2 gives the agent's harness and no tool scope of its own; a `permission` the user sets replaces it.

6. **Start.** Start the engine on demand and the runner with the detected agents.

7. **Write the project file, the instructions and the skills; no wiring.** Init writes no harness wiring: in a repository it writes `.brigada/project.yaml`, the four role instruction files under `.brigada/instructions/` and the default skills under `.brigada/skills/` (full default text under a marked header with the version and digest; a rerun rewrites an unedited file and leaves an edited one, printing the three-way diff command; `remove` deletes only unedited ones; §5.9, §5.10) and `.brigada/operator.md` (a generated guide for the user's own coding agent, the operator, §4; init overwrites it on a rerun, `--no-wiring` does not skip it, and `brigada remove` deletes it), the same text as a Claude Code skill at `.claude/skills/brigada-operator/SKILL.md` (generated from the same source, with the same rerun and removal rules) and nothing else.

   A rerun, and `brigada remove`, strip the wiring an older init left — the bridge in `.mcp.json`, the hooks in private `.claude/settings.local.json` and the old `brigada hold` hooks in shared `.claude/settings.json`, the `.codex/config.toml` table, `.brigada/foreman.md` with its router block in `AGENTS.md` and `CLAUDE.md`'s import section, and the foreman skill — editing only brigada's own entries, so a file the user never touched comes back byte for byte; the git exclude line for the private settings goes too. `--no-wiring` skips the strip in a repository (in a parent folder it still skips the coordinator's wiring, below).

   No secret is written to any repo file, and init never commits or pushes.

### Setup questions

**Tiers, effort and tasks at once.** With tiers (§6.1, §8) the user chooses which models may work: the foreman picks a tier and an effort per task, and the engine picks the agent. The questions are:

1. **The foreman.** After the sign-in probes, a numbered menu of every model the signed-in claude, codex, opencode and qwen-code CLIs list (Claude's aliases, Codex's `debug models` catalog, OpenCode's `provider/model` list, Qwen Code's static pair of subscription models, models of unknown family left out), then its effort: the effort menu, asked once, for the foreman only. Enter picks the first usable resume-capable model, claude before codex before opencode before qwen-code.

   A foreman model whose harness is not used for tasks is registered as an agent with no classes, so it serves foreman turns and nothing else. The runner accepts `classes=none`, and the engine exempts foreman turns from the class filter: the harness-and-model pair is their constraint, so this agent takes no task job.

2. **Workers.** Per installed CLI, the sign-in check; Claude and Codex then ask `Use <cli>'s models for tasks (<models>)? [Y/n]:` in place of a model menu (Enter is yes on the first run; the `Use kimi? [y/N]:` and `Use opencode? [y/N]:` questions are asked when sign-in is unconfirmed): every model the CLI lists with a family brigada can tell becomes an agent at its tier (§8); unknown-family models stay out, because the family drives review routing.

   Kimi keeps its writing question. opencode keeps a menu, because its list spans providers, and the menu accepts several numbers. There is no effort question for workers.

3. **Tasks at once.** One number, default `min(4, max(1, cores/2))`, explained with the machine's core count and the per-account caps it implies (§8.1); it becomes `caps.global` and the runner's `--max-parallel`.

4. **The check.**

The plan block gains a tier column in the agents table, the foreman's effort, and an "at once" line with the per-account caps. Every prompt keeps the four parts: what is chosen, why, the valid answers, what Enter does. A rerun defaults to the saved choices.

`--agent` keeps working, once per model, so `--yes` runs name each agent; `--foreman HARNESS/MODEL@EFFORT` and `--parallel N` answer steps 1 and 3 without a question; omitting `@EFFORT` leaves the interactive effort question.

### Runner agents

**The agents file.** Init saves the runner's agents, one `--agent` line each, in `runner/agents` under the state directory; `brigada start` and init's restart read it back through the runner's own `--agent` parser and validation, so a saved line is validated as the runner starts it. `runnerstate` is the one reader and writer: it keeps every line it is given, including comments and lines it does not understand.

Nothing re-reads the CLIs' model lists on its own, so a model a CLI offers later stays unregistered until the user says so.

`brigada agents` (read-only) runs the cheap listings init uses for claude, codex and opencode and reports the models offered but not registered, each with its name-table tier and the account it would share, and the registered models a CLI no longer lists (never claude's, whose alias list is not whole, nor from a listing that left out a model of unknown family). The name-based tier is a guess, corrected by `tier.<harness>/<model>` (§8).

`brigada agents add <harness>/<model>` appends one line carrying the account its harness's registered agents use, refuses a model already registered, validates the line through the same `resolveRunnerAgents` the runner runs at startup, and restarts a running runner as §5.1 describes (draining, or cutting its jobs with `--now`), so the line is served from that restart; with no runner running, the next `brigada start` serves it. Registration is never automatic, since a new model can spend a paid account.

`status` and `top` show a one-line hint when the last listing found unregistered models; the listing is cached with its time in `runner/models-offered.json`, `status` lists again only when it is missing or older than twelve hours, and `top` reads the cache on every redraw and refreshes a stale one once in the background, so no CLI runs per call.

### Coordinator setup

**Parent folder (the coordinator).** Init in a folder that is not a repo scans only its direct children by default. It does not descend into a repo (submodules are not projects), does not follow symlinks out, and skips hidden directories.

It prints a visible list: path, canonical URL, new or known, and a skip reason (no remote, ambiguous URL, or several remotes without `origin`, which need `--remote` inside that child). The user selects, and only the selection is registered. Two children with the same canonical URL are one project. It wires the coordinator set in the folder's project scope and writes nothing into the child repos.

`brigada init` outside a repository scans the direct children (a symlink, or a `.git` that is a symlink, is listed as skipped and never followed; a directory counts as a repository only when its own top level is itself), prints `path  canonical URL  new|known|unknown (engine not running)` or `skipped: <reason>`, and registers what the user picks at the prompt (`1,3`, `all`, blank for none) or everything new with `--all`. A second child with a canonical URL already seen is a duplicate, not a second project.

New projects take the foreman of `--foreman harness/model`, else that of a project the engine already has; with neither, init refuses before writing.

It writes the folder's `.mcp.json` entry `brigada-coordinator` (`brigada mcp --coordinator`, additive and idempotent, a damaged file left alone) and the brigada-owned skill `.claude/skills/brigada-coordinator/SKILL.md` (coordinator instructions, `foreman.CoordinatorInstructionsVersion`, golden-tested).

It adds no hook and no permission entry, so `plan_approve` and `plan_reject` stay on the harness's own "ask". It starts no runner: agents and a default check come from `brigada init` inside a repository.

### Repository configuration

**`.brigada/project.yaml`.** The portable, versioned, shareable repository configuration, committed by the user so it travels to colleagues and machines. The cost: one brigada-owned file in the user's repo.

Only nonsecret repo-level intent goes in it: the canonical repo identity, the default check, the worker-tool policy, the `instructions` map (repo-relative paths to the project's text per role, read by the client at import; `instructions_file` is its 1.0 alias for the worker text, §5.9), `skills_dir` (§5.10), and project defaults and preferences. Accounts, credentials and tokens, local executable and state paths, detected login state and machine overrides never do; they stay local.

Init writes it additively and reviewably (a new file, or a diff the user approves) and never commits or pushes it. The client reads the file and sends its content over the API, so the engine never reads a repo path (product §5 rule 6).

Schema 1 of the file holds `schema: 1`, `repo` (the canonical identity, written in canonical form and checked against the selected remote), `default_check`, `worker_tools` (the deprecated `tools=` spelling, an alias of the per-agent `permission=` of §5.2, kept through schema 1 and removed no earlier than the next major version), `approval_max_tasks`, `approval_budget`, `approval_classes`, `approval_protected_paths`, the gates and land rules of §6.3 (`gate_plan`, `gate_land`, `gate_push`, `approval_tiers`, `approval_restarted`, `land_target`, `after_land` — stored, shown and enforced: `gate_plan: ask` holds every plan at the plan gate, the land-gate rules hold a task's landing, the land job obeys `gate_push` and runs `after_land`, §7.1), `land_ready` (a repo-relative command path inside the repository — stored, shown and enforced: the land job runs it as the land gate and the push phase runs it again before pushing, §6.3, §7.1), the environment step's `setup`, `setup_cache_key_files` and `setup_timeout_minutes` (§5.7), the default `extra_repos` (§5.8), the foreman's `foreman_rotate_tokens` (§4.2), the plan-file mirror's `plan_files` and `plan_file_branch` (§6.4), the role instructions map `instructions` with the 1.0 alias `instructions_file` of its `worker` entry (both present is refused naming both, §5.9), and `skills_dir` (§5.10), and nothing else; an unknown key or schema version is refused with the key named.

The batch-landing key `land_batch_max` is not in schema 1: §7.1's batch is not implemented, the key is refused as an unknown key, and brigada lands one task per land job.

`brigada project import [--repo URL] [--file PATH] [--yes]` reads the file in the client and sends its bytes; the store records the hash of the last import and, per key, the value it set. A key whose stored value is still that import's counts as imported; any other stored value is a local override, and `project set` always makes a key local. `project show` prints `(default)`, `(set)` for a local override, or `(imported <hash>)`.

`status` flags a file whose hash differs from the last import, reading the file in the client and changing nothing.

The store holds the *effective runtime settings*, each with its provenance. Precedence, highest first:

- a local override the user set on this machine;
- the last imported file value;
- the built-in default.

Detected suggestions only propose.

Import is explicit (init, or a sync command), never a side effect of `git pull`, and `status` flags a file that differs from the store.

On import, a file change over a store value that is still the previous import's applies. Over a local override it does not: the conflict is shown with both values for the user to resolve. A file against a hand-set store value on first import shows the diff and asks.

An unknown schema version is refused. Neither the file nor the store wins silently, and the engine never writes the file back after init.
