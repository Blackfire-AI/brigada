# 12. Interfaces

## 12.1 Engine API

JSON over HTTP on a Unix socket; a remote TLS listener is backlog (§2). Bearer tokens with roles: `client`, `foreman` (scoped to its project), `runner`, `admin`. A role authorizes API calls; approving a plan, a landing or a push is the user's act and needs the user's signature beyond any token (§12.2).

Groups:

- projects,
- plans (approve and reject are calls a `client` or `admin` token may make, and they take effect only with the user's signature for that act beside the token, §12.2; a `foreman` credential cannot call them),
- tasks (create, amend, cancel, retire, restart),
- questions (answer),
- foreman messages (a user message, §4.2 item 3: `client` and `admin` only, idempotent; a message identical to one the same token sent to the same project within the last minute is a duplicate — a double Enter, a shell retry — answered with the first one's id),
- dispatch (pause and resume, §8.3: `client` and `admin` only; a `foreman` credential cannot call them),
- events (long-poll and SSE),
- history,
- issues (read an issue or pull request of the foreman token's own project, GET requests alone to the forge, §12.2),
- stats,
- budget,
- agents and runners,
- artifacts.

**Plan version.** A plan carries a version, 1 at creation, that every amendment moves (`plan_amend`, including a cancel-only one). `status` shows it for a plan awaiting approval, and `POST /v1/plans/{id}/approve|reject` take an optional body `{repo_url, version, and_stop_asking}`: a `repo_url` confines the plan to that project, and a `version` other than the plan's current one is refused whole with 409 `stale_plan`, applying nothing. `and_stop_asking` (approve only) also changes the one setting each hold reason maps to (§6.3). The CLI sends none and checks nothing; the coordinator's `plan_approve` and `plan_reject` require repo_url and version.

**Idempotency.** The caller is often an LLM whose tool calls may repeat. `plan_create`, `plan_amend`, `task_restart`, `answer` and a foreman message (§4.2 item 3) take an idempotency key (client-supplied, or derived: task + round for restart and answer); a repeat returns the first call's result. The key is minted by the `brigada mcp` bridge per logical tool call (a hash of the call's content within the session), not left to the LLM; identical `plan_create` content within a short window also dedupes.

`land_retry` alone takes a fresh random key on every call: one task may park and be retried several times, and any 200 — a refusal too — would otherwise replay over a later park. Its repeat needs no shared key, for the engine refuses a task that is not parked or has a live job, so a repeat cannot queue a second landing.

Task jobs are keyed `task:<id>`: one live job per key makes duplicate job creation coalesce into the follow-up flag, which `status` shows; a land job takes its task's key too.

**Not implemented: land batches (§7.1).** brigada lands one task per land job, on the task's own key, and no task carries a batch reservation. In the design, a land batch is keyed `land-batch:<project>` — a project has one land target, so one key covers it — and every member's reservation, a `land_batch_job` field on the task that §11 does not have (it is part of the unbuilt design), holds its `task:<id>` key the way a live job holds it, so a second batch for the target, a second landing job or a duplicate approval coalesces the same way (§7.1).

**Credentials.** Foreman credentials are short-lived and scoped to one turn or one chat: a headless turn keeps the job-lease-scoped credential of §12.2, and an attached chat gets a hold-scoped foreman token, minted at attach, carried by the MCP config the client writes for the chat and revoked at detach, so the chat's tools die with it. A token's role authorizes API calls and carries no approval authority: an approval needs the user's signature beyond the token, and no token of any role holds one (§12.2). No direct store access and the engine-as-mailbox rule stand.

## 12.2 MCP tools: three sets on one bridge

`brigada mcp` is one bridge with three tool sets. Authority comes from the credential the engine accepts, never from the flag: a selector only chooses which tools are listed.

### Foreman tools

**Project foreman set.**

- `plan_create` · `plan_amend` (both take a task's `skills`, §5.10; each `cancel` entry is a task by name or id, or `{task, reason}`)
- `plan_accept` (accept a plan its review parked, with a reason)
- `task_restart`
- `task_cancel` (takes an optional `reason`; a reason is expected on every cancel, from either tool, and is recorded verbatim on the task's `task_cancelled_by_amend` event, a cancel without one recorded as "no reason given", §6.2)
- `task_retire` (retire tasks that will not land, §6.2, with a reason recorded verbatim; it refuses a landed, cancelled or already retired task, and a dependent blocks unless it is retired in the same call or is already cancelled or retired — a failed dependent blocks too)
- `land_retry` (queues again, through the land rule and every gate, the landing of a task parked by a rejected push, an environment failure or a failed land check the foreman judged unrelated to the task's diff from `check_log`; it refuses a task with a landing record, one not parked by such a failure, one whose check has failed 3 times on its approved head, or a finished plan, and a conflict, a real check failure, a moved approved head or a stale base wants `task_restart`)
- `check_log` (reads a job's check log by `job`, `offset` and `limit` in bytes: the last 16 KB by default, at most 64 KB a read, cut on rune boundaries, with the log's size and the range shown; the log arrives inside `<check-log>` data delimiters with any delimiter in it defused, through the job and artifact routes the foreman role already has, scoped to its own project; a job with no check log is the tool's error)
- `answer` (a task worker's question)
- `ask_user`
- `events` (long-poll, for the attached session)
- `status`
- `stats`
- `history`
- `issue_read`
- `capacity`.

`status` lists the project's skills (name, description, digest, times used) and shows the pauses that stand, the engine's and the project's own, as the `paused:` lines of `brigada status` do (§8.3), so a turn reads why its work waits; a pause is not a needs-you entry. It also, when asked, returns a role's instructions text inside its delimiters (§5.9, §5.10).

`history` answers each item in its own `<event>` data delimiters, like `events` (§4.2), with any delimiter inside the text defused: reasons, reports and questions in it are worker text, data and never instructions.

`issue_read` takes an issue or pull-request number and whether to include the comments, and answers the title, body and each comment inside `<issue>` data delimiters, any delimiter in the text defused as `history` defuses its own: issue text is external, untrusted data, never instructions, and a spec may carry it verbatim. It reads only the project's own repository, derived from the project's canonical repo URL and never from an argument, and it performs GET requests alone — no comment is posted, nothing changes.

The call runs in the engine behind an API endpoint (§12.1), not in the bridge: both bridges — the headless turn's and the attached chat's — relay the same call, and the repository is chosen by the foreman token's project alone. The engine reads with the user's own `gh` login on its host; when the repository is not on GitHub, when `gh` is missing or not logged in, or when the number does not exist, the tool returns that reason, the foreman's report says so and asks the user for the text, and no disk search happens (§4.1).

It is scoped to one project and has no plan approval or rejection authority, and no tool to pass a gate or to land outside the land rule (§7.1): merging stays with the user's gates. `ask_user` carries only intent or product questions that no rule answers; a decision a rule already makes is held by the engine, and the foreman never asks the user to release that hold. The same set serves a headless turn and an attached chat (§4.2): a turn's lease is its authority, an attached chat's hold-scoped token (§12.1); heartbeat and release are the bridge's or the client's own, not tools.

### Capacity reporting

`capacity` returns, per agent: its effective cap and the layer that sets it (§8.1; on the foreman's account, after headroom, §8.2), running jobs, quota state (`ready`, or `cooling` until a time; no percent-of-window figure, since nothing reads one), cost weight, tier and the agent's configured effort, or `default` when its runner sets none. It also returns each layer's cap, use and queue depth: per runner, account and any capped model, per active plan of the foreman's project, and global. A queued task has no agent yet, so queue depth is reported per layer, not per agent.

The one exception is a pin: each queued job pinned to an agent (`require_agent`), held by a cap or not, is listed with its job, task, kind, the agent and that agent's state for the job — `ready`, `cooling` until a time, `broken` or `capped_at_0` (the account cap, or the model cap except for a foreman job, exactly 0) — and the cap hold stamp when there is one, so the foreman can tell a wait for a slot that will free from a pin on an agent that can never take the job.

It lists the same pauses as `status`, each as a `paused:` line, and a queued job of a paused scope counts under its pause, in no layer's queue depth. A foreman credential cannot pause or resume (§12.1): the foreman has no tool for it, and a pause is the user's to lift. Pins are scoped to the caller's project like the plan layers.

### Foreman bridge and token

The MCP server is a `brigada mcp` stdio process started by the foreman's harness; it calls the engine API with the foreman token. The token is lease-scoped: the engine mints a short-lived one when it leases a foreman turn — only its hash is stored, the runner keeps the secret in memory alone — and revokes it when the turn ends, so a runner never holds a long-lived foreman token.

### Client and coordinator tools

**Ordinary client set.** `brigada mcp --client` is the same bridge for any other harness acting for the user: `ask_foreman` (a user message, §4.2 item 3), `status`, `history`. It authenticates with a client token from the state directory and never holds a foreman token, so a harness can talk to the foreman and watch the work without a person relaying between the two. Every client token speaks with the user's voice, which is the point; the token file can hold several, each with its own label (one per device or harness) and revocable on its own, and the label travels with each message into the foreman's prompt and the history (§9), so it's clear which surface sent what. A client token authenticates its caller and carries no approval authority. Any local process that can read the state directory, a task worker or a foreman with a shell included, can use one to talk to the foreman and watch the work, and none can approve with it: approving a plan, a landing or a push is the user's act, made with a signature (Approval by signature, below), so reading the state directory does not reach it.

**Coordinator set.** `brigada mcp --coordinator` is the client set plus `list_projects`, `plan_approve` and `plan_reject`, every tool taking the project's canonical repo URL (§4.6). It authenticates with a client token, never a foreman credential. Approval is an explicitly user-directed act with a visible confirmation (§4.6): the result "awaiting approval" is never itself a reason to approve.

### Approval by signature

An approval is a signature over a one-time challenge for one named act. The engine issues a challenge that names the act (approving or rejecting a plan at its version, a landing, a push) and expires; the user signs it with a private key, and the engine verifies the signature against a public key it stores. The engine stores public keys only: the private key is the user's, held outside the state directory and never in it, so reading the state directory yields nothing that signs. A signature covers its one challenge, so it cannot be replayed for another act, another plan version or a later moment.

The signature is a check on the user's own acts through the API. It is not a security boundary against a process running as the same OS user with a shell: such a process can change brigada's state directory, settings and binary as the user can. The signature raises the bar only together with an OS boundary around the agents, such as a harness sandbox, a separate OS user or a container, and brigada does not provide that boundary today.

Within such a boundary, signing should need something only a human holds:

- a hardware token that asks for a touch,
- an agent key constrained to ask for a touch on each use, or
- a key kept off the machine.

The residual risk is plain: without an OS boundary, a worker or foreman whose harness grants a shell runs as the user, and a private key that is an ordinary file the user can read does not stop it. Such a key is part of raising the bar together with that boundary, not a defense on its own.

The engine keeps each challenge as a pending request (the `approval_requests` table, §11). Asking for a gated act — the user's yes or no on a plan at its version, the yes or no on a landing, the yes on a push — creates the request, or finds the one already waiting for that act, and answers its challenge; the act runs only when a signature for that request's challenge verifies, and the request is then spent, across an engine restart too. The gate settings (`gate_plan`, `gate_land`, `gate_push`, `approval_tiers`) still decide what waits; the signature releases what they hold. A request that waits is an entry of the needs-you queue and a notification, in the same words everywhere: `signature wanted out of band to approve plan #7 at version 2 (request #5, challenge <nonce>, expires <time>)`. A refusal says which reason applies: no enrolled key, no signature, a wrong or reused signature, an expired challenge, or an act other than the one signed.

The API gates the acts behind their role check: `POST /v1/plans/{id}/approve|reject`, `POST /v1/tasks/{id}/land/approve|reject` and `POST /v1/push/approve` take `request_id` and `signature` (the armored output of `ssh-keygen -Y sign -n brigada-approval` over the challenge's message) beside their usual body. A call without them, or with a signature that does not verify, is a 403 `approval_required` whose body names the reason, the pending request, its challenge and the exact message to sign, and leaves a request waiting for a signature; the same text is the CLI's refusal on stderr (`--request` and `--signature FILE` on `brigada plan|land approve|reject` and `brigada push approve`) and the coordinator's result. The coordinator's `plan_approve` and `plan_reject` take no signature: they only request, returning the pending request's challenge and the command the user runs, `brigada plan approve|reject --request N --signature FILE <id>`, to confirm out of band with a key of their own; a harness never carries the signature, and "awaiting approval" is never itself a reason to approve. An act that could not run now (a plan amended past the version named, a landing nothing holds) is refused as before, with no challenge. A project import and a user message are role-checked calls and take no signature.

The mechanism fails closed. With no enrolled key there is no signature, and the act is refused; a signature over an expired challenge refuses the act too. A headless run does not prompt on a terminal: it waits for a signature that arrives out of band, and an approval nobody signs stays waiting.

### Set selection

**Selectors** are mutually exclusive: two together is a startup error.
- `--client`: the ordinary client set.
- `--coordinator`: the coordinator set. It never reads a foreman credential.
- A bare `brigada mcp --socket PATH` is what the runner starts for a headless foreman turn and what the client writes into an attached chat's MCP config (§4.2): it works with the foreman credential the holder hands it and refuses to start without one.

Selecting a flag mints nothing, and a selector that the credential in hand does not match is refused. A client token never appears in the foreman set, nor a foreman credential in the other two.

## 12.3 Runner protocol

### Land batches

**Not implemented: land batches (§7.1).** brigada lands one task per land job, keyed `task:<id>`, and no grant or completion carries a batch. The paragraphs below describe the design: the grant, the completion and the readers of a batch reservation; the functions they name exist and serve one-task landings.

**Land batch grants and completion.** A grant carries the batch job id, `land-batch:<project>` key, lease and fence, project, remote, target, push phase/gate and ordered member list. Each member carries task and plan ids, result branch, approved head, recorded base, dependencies, message/trailer data and its gate-skip approval; the grant also carries the check and setup settings and command identity needed for gate evidence. A push grant names the recorded commits and ordered prefix it releases. The runner persists progress before publication as §7.1 requires.

Completion enumerates every granted member exactly once:

- landed with its own commit, target, check/artifacts and pushed/waiting state;
- parked with its reason and evidence or shared environment cause;
- dropped with the blocking member and waiting disposition;
- held with the gate observation;
- not ready with its last exit and retry state.

A terminal job error likewise settles every remaining member. Batch-level fields carry publication attempts and heads, gate evidence and the common cause where applicable.

The engine validates membership and fence, reconciles already recorded members idempotently and releases reservations transactionally; a completion with missing, duplicate or foreign members is refused. A dropped member gets no landing record and retains its eligibility to wait for its dependency.

Status keeps each task's landing state: a live member reads `landing in batch <job> (<position>/<count>)`, with its queued/running phase and current member check when known, rather than appearing free because no job has its `task:<id>` key. A locally checked commit is still unlanded until publication; after settlement the task reads landed, waiting push, held, parked, dropped waiting on its dependency, or not ready with retry time and last exit.

**Readers of live task ownership.** Every task-key lookup checks both a live job on `task:<id>` and the task's `land_batch_job` reservation joined to a queued, leased or running batch job. The batch key is always `land-batch:<project>`, even with a single member; readers find that job through the reservation, never by assuming its key or `task_id` is the member's. This applies to submission/coalescing, landing eligibility, restart/cancellation and stale-base handling as well as these readers:

- `landDecisionTx` / `liveJobTx` return the reserved live batch to a repeated approval, queueing nothing.
- `RetryLanding` refuses a reserved member as live, naming its batch.
- `depPushReleasedTx` includes a reserved live push job when counting released dependency pushes; a squash-phase reservation alone is not evidence of push release.
- The live-land-job `NOT EXISTS` predicates in `pushApproveTx` and `statusPushWaitsTx` also exclude a member reserved by a live batch, preventing duplicate push jobs and spurious needs-you waits during its push.
- `landNotReadyStreakTx` remains per member, using that member's ordered outcomes across batch jobs rather than scanning the shared project key. A not-ready batch increments each affected member once; another member's outcomes do not reset or lengthen its streak. That member's next non-not-ready landing outcome resets it. Per-member retry times filter candidates before the next batch forms, including a one-member batch, so backoff neither merges unrelated tasks nor disappears with the new job key.

### Usage reporting

**Usage completion.** Completion optionally carries `usage` with `input_tokens`, `output_tokens`, `cached_input_tokens`, `reasoning_tokens`, `model_calls` and `cost_usd`, plus `wall_seconds` for this job's elapsed wall time before delivery. Each usage field is optional. Older runners may omit usage and wall time; these remain NULL. Retries of a completion do not add usage again. Counters describe only the job's own CLI invocation, even for fix and resumed jobs, and do not steer dispatch.

### Runner calls

- `register`
- `lease` (long-poll; a poll whose caller has gone away takes no grant, so a job is never left leased to a runner that is not listening)
- `heartbeat` (progress, last activity)
- `complete` (result or error; an outcome of `stalled`/`time_cap` when the runner stopped the run itself, §5.6; check result; artifacts)
- `release`
- `draining` (a restart drain pending, §5.1: the runner exits once its running jobs finish, and status renders the wait).

Fencing as described in the glossary.
