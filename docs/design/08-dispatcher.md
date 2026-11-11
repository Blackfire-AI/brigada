# 8. Dispatcher

## Job usage

**Per-job usage.** Each job records the usage its own CLI invocation reports: input, output, cached input and reasoning tokens, and US dollars when supplied. Input means all input, cache reads included, and cached input is the cached part of it, so the columns mean the same for every adapter: Claude's result usage is counted once, with cache reads and creation added to its input; Codex input already includes the cached part; opencode reports input without cache reads, so the adapter adds them.

Each job also records `model_calls`, the model requests the CLI reported: Claude's `num_turns`, one per Codex turn, one per opencode step and one per kimi assistant message with usage. Calls is the unit for plans that limit requests per time window. Codex turn usage and opencode step usage are summed.

Missing or malformed counters stay unknown (NULL), distinct from reported zero. A fix round and a resumed session are separate jobs: totals are never carried or subtracted across jobs. Wall time is recorded with completion.

Stats sum known counters per agent and class from every job, including work, fix and review, and show cost per success in dollars when any dollar cost is known, otherwise in tokens. Unknown jobs do not contribute to sums; no successes means no cost-per-success. These measurements do not steer dispatch; the plan budget (§6.3) is the one rule that reads them.

## Agent selection

- **Eligible:**
  - the adapter supports the class
  - family rule for reviews (`review.family`, §7: the three steps under `prefer` — another family, the author's family, the author's own agent — each eligible only when no agent of the step before it is available, or only another family under `require`)
  - agent `ready` (not `broken`, not `cooling`)
  - account not cooling
  - session affinity (a resumed session stays on its agent and runner — a continuing task's first work job (§6.1) is placed the same way a fix round is: session mode, pinned to the dep author's agent, its key's session row seeded with the dep's session id so the affinity holds the runner)
  - the agent's tier is the task's effective tier, with the fallback below; for a review job it is the review tier: by default one tier below the task's effective tier, per §7, and then the same fallback.
- **Cap room:** an eligible agent is picked only if every concurrency cap on its path has room (§8.1). Caps shrink the set of eligible agents with room, so a task whose best agent is cap-blocked goes to the next-best agent with room; it waits in the queue only when no eligible agent has room, and that never parks it. **Parking** means no agent is eligible with caps ignored.
- **Score:** per (repo, class, agent): a success rate with a prior, where success = check passed and review approved within the rounds. Defaults for a cold start. `stats` shows it with its confidence. The record is counted per skill count as well (§5.10): `stats` rows are per (agent, class, skills) and each row's rate comes from its own attempts, while the dispatcher ranks on the sum over skill counts.
- **Ranking.** Within the served tier, each eligible agent draws p from Beta(successes + 0.5, failures + 0.5), its outcome record for the job's (repo, class), and the agents rank by `weight / p`, lowest first, where weight is the configured `cost.<harness>/<model>` (default 1, §8.1). This is Thompson sampling with a cost: a thin record draws widely, so a weaker or newer agent keeps being tried without a separate exploration share, and a clearly better record beats a slightly cheaper agent most of the time.

  A weight of 0 ranks before every positive weight; among zero weights the higher p goes first. Exact ties break by a further draw, never by agent id alone. The draws come from the engine's injectable uniform source, so tests stay deterministic.

  Cap room then walks the ranked order and takes the first agent with room. A job pinned to an agent (a fix round) is not ranked, and the automatic restart's pick shares the same sampling. The decision record names the pick `sampled` and holds each candidate's weight and draw; a pinned lease draws nothing and is recorded as `pinned`. Sampling never sends a task to a lighter tier; only the last-resort fallback below may.
- **Quota:** limit-hit errors cool the whole account until its window resets; Codex's reset time is read from its own limit message (`try again at …` in the machine's local zone, or `try again in …`), and a reset beyond eight days is suspect and capped there; CodexBar's CLI JSON is an optional extra signal.
- **Health:** repeated failures, auth errors, a rejected model or a dead runner → `broken(reason)`. The failure streak counts terminal completions that failed on the agent whatever the job kind — work, fix, review or foreman; a failed setup (§5.7) or an infrastructure failure (§5.1) is the environment's or the runner's plumbing, not the agent's, and never advances the streak. The four reasons recover differently.
  - `model rejected` is the provider refusing the agent's model — a 400 `invalid_request_error` saying the model is not supported for the account or does not exist, as every adapter's classifier reads it into the `model` error kind: the agent stays out, is never probed, and its reason carries the provider's words. Only `brigada agent reset` or removing the agent clears it; its needs-you line names the model and the error and always points at `brigada agents`, where the user checks whether the model is still offered.
  - `repeated failures` is self-healing: a few minutes after the agent was marked broken the runner, on the engine's instruction over the runner protocol (§12.3), runs a cheap probe of the agent's CLI — the same kind of check `brigada init` uses to see that the CLI answers — and a passing probe clears the broken state with an event; a failing one waits longer before the next probe, the delay doubling and capped.
  - `auth` is never probed: it stays broken until the user fixes the login and `brigada agent reset`.
  - `runner stale` clears the moment the runner is seen again (a register or a lease poll). A missing heartbeat while the engine itself was asleep proves nothing: the sweep's suspension rule (§13) restarts the stale window from the wake, so a runner that slept with the engine keeps its agents.
- Every decision is recorded with its reason ("why this agent").

## Tiers and escalation

**Tiers.** Each agent serves one tier, from brigada's tier table by model name, a segment-prefix and whole-segment table beside the family table (§7's detection): `opus` and `fable` segment prefixes and whole `pro`, `max`, `ultra` and `astra` segments are heavy; the `haiku` segment prefix and whole `mini`, `nano`, `flash`, `lite`, `highspeed`, `free` and `luna` segments are light; any other model is standard.

The config override `tier.<harness>/<model>` (same shape as `cost.`, §8.1) corrects the table for every agent of that harness/model, as `cost.` does. Because the tier is an attribute of the agent, like effort, the outcome record keeps its key (repo, class, agent); records earned under one tier stay attached to the agent when an override changes its label. `stats` and `capacity` (§12.2) show the tier beside each agent.

- **Fallback** (cap-0 rule): a task whose tier has eligible agents that are all cap-blocked waits in the queue (§8.1): a cheap wait beats a heavier model. A cap that is merely full waits, but an agent whose model cap or account cap is exactly 0 can never run, so it counts as unavailable: a tier whose agents are all capped at 0 falls back as if it had no ready agent.

  Such an agent is never a placement candidate either: not for a ranked pick, an automatic retry's pin (§3), a review's family step or the cooling hold (§8.1), so a job is never cap-held behind it. An agent that is cooling is unavailable too, whatever the distance to its reset: a tier whose agents are all cooling — minutes or days alike — counts like one whose agents are all broken, and falls back as if it had none.

  A tier with no registered agent, or whose agents are all unavailable, climbs to the next tier up; from heavy, or when no higher tier has an agent, it steps down instead, and within the tier it lands on the usual cost/score ranking picks.

  This holds for work, fix and review jobs alike, with the family rule for reviews applied first, its steps outranking the tier: a review of a Claude-written task does not wait for a Codex reviewer while a standard reviewer of another family is ready, and under `prefer` a reviewer of another family on another tier is taken before a same-family reviewer on the tier asked for; the tier climb and step-down happen within the step that has an available agent.

  When no tier has an available agent but some are cooling, the job waits in the tier whose agent resets first. A job pinned to its agent (a fix round on the author's session) is not moved and waits for it. The decision record names the tier asked for and the tier served.
- **Escalation:** a task climbs a ladder of effort, then tier. The task carries an effective effort beside its effective tier, starting as its own effort (possibly empty); the lease resolves the job's effort from the effective effort, then the agent's explicit effort, then medium.

  When a task's check or review rounds are spent, or a worker ends with the result-block status `failed` (§5.4), and the retry rules of §3 (the transient retry, the quota re-dispatch, the automatic restart) have declined or are spent, the engine first retries on the same tier with the effective effort one step up — low → medium → high, starting from the resolved effort, never stepping to max — with the event `task_effort_bumped` (old and new effort, the reason). Only when the effort is already high (or max) does it raise the effective tier one step, with the effective effort reset to the task's own, and the event `task_tier_bumped`. Each step queues a fresh attempt as the quota re-dispatch does (§3: the next `-r<n>` branch, the previous job's summary carried, counted toward the restart cap on every step); neither event wakes the foreman.

  A run that ended without a readable result block may be the runner's fault, so it takes the effort steps like any failure but never raises the tier: a no-result at high effort goes to the foreman with the reason. A job that failed for a transient reason (quota, auth, a lost runner) belongs to those retry rules, never to escalation.

  The task reaches the foreman (`needs_foreman`) only at heavy and high effort, or when its attempts are spent. A stalled worker still always goes to the foreman.

  The foreman may change the tier or effort again with `plan_amend`'s `retier`: a new effort sets the effective effort with it, and a tier change resets the effective effort to the task's own.
- **Review tier and effort.** A review job asks for the review tier and runs at the review effort, not the task's: the task's `review_tier` and `review_effort` if it has them, else the engine default of `review.tier` (`below` the task's effective tier, or `same`) and `review.effort` (default `medium`), as §7 describes. The tier choice, the candidates and the lease's effort resolution read them for every job of kind review, the retries and resumed re-reviews included; the escalation ladder above moves only the task's own effective tier and effort and never a review's. The cooling fallback and the retry exclusion work as for any job, on the review tier asked.
- **Effort** (§6.1) is per job: the runner passes the task's effort to the adapter, so two jobs on one agent may run at different levels. It is not a cap layer (§8.1).

## 8.1 Concurrency caps

**Layered caps.** A job may start only if every layer has room; the effective capacity is the minimum across the layers:

- **Runner:** max parallel jobs per runner, for the host's CPU, memory and parallel builds and tests. The runner advertises its own cap when it registers; the effective runner cap is the smaller of that and the config's.
- **Account:** max parallel jobs per adapter account, for the vendor's session and rate limits; parallel sessions also drain the 5-hour window faster.
- **Model** (optional): a per-model cap, such as 1 for a heavy model.
- **Plan:** max parallel tasks per plan, because tasks touching the same files end in merge conflicts. A task counts while it has a live job (`running`, `checking` or `reviewing`, §6.1).
- **Global:** an overall ceiling for the engine.

A task job (a task's first run, a fix round or a review) that cannot start because some layer is full waits in the queue, oldest first, and starts when a slot frees; it does not park (§3) and does not notify the user.

**A cap of 0 is not a full cap.** A model or account capped at exactly 0 never has room, so waiting on it would wait forever: an agent on such a path is never a placement candidate — not for a ranked pick, an automatic retry's pin (§3), a review's family step under `review.family=prefer` (§7) or the cooling hold — and no cap hold is stamped for it. The exemptions are the cap gate's own: a foreman turn ignores the model cap, so a model capped at 0 still takes foreman turns, while an account capped at 0 takes none.

A job pinned to such an agent has no candidate, an unmet placement rather than a hold, so the queue TTL parks it with a reason that names the pin and the layer, for example `queue TTL exceeded: job pinned to claude/claude-fable-5-1[1m] (capped at 0 at the model layer); no runner leased the job within 10m`. A cap that is merely full still waits.

**The foreman is never starved.** Foreman turns count toward the runner, account and global caps (not the plan or model caps), and a pending foreman turn outranks every queued task job at each of those layers: the next slot that frees on its path goes to it. Its wait is therefore bounded by the running jobs' class time caps (§5.6). An attached chat (§4.2) is not a job, but it is a live session on the foreman's account: it counts toward that account's cap while held, without ever waiting for room, so the dispatcher starts no new task job there until there is room again.

Each agent also carries a **cost weight** (default 1), a relative cost the user sets, which the foreman sees through `capacity` (§12.2) when it chooses which tasks can wait for a cheaper agent. The dispatcher uses it too: agents of one tier rank by `weight / p` (§8), so a dearer agent is picked less often, and a weight of 0 makes an agent free, ranked before every paid one.

**Effort and caps.** A task's effort (§6.1) is not a layer: a `max` job and a `low` job on one agent share that agent's caps. A high effort drains the account's window faster per job.

**Defaults set by init.** Init asks one number, tasks at once (§13.1), and writes it as `caps.global` and the runner's `--max-parallel`; the account caps come from a per-harness default table (claude 3, codex 2, kimi 1, opencode 2, dsh 1), each clamped to that number and shown in init's plan block; `caps.plan` is set to 3. A cap key the config already names keeps its value — the plan names each kept and each changed value — unless the tasks-at-once answer was explicit or `--overwrite-config` is passed.

A saved agent line the run's answers do not replace stays and the runner serves it: an interactive run's questions replace every saved line of each CLI asked about, while `--agent` replaces the harness and model pairs it names. Unset `--max-parallel` still means one job at a time. `brigada config` shows the config file to edit afterwards; the engine reads caps when it starts.

**Config example.** Conservative per-adapter defaults are set in the engine's config file:

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

## 8.2 Foreman headroom

The foreman's session uses quota on its own account, so task workers must not drain that account and leave the foreman `limited` mid-plan (§4.4). A task job — work, fix or review — on the foreman's account sees an account cap one lower than the configured cap, never below 0 (`max(cap − 1, 0)`), so one slot stays reserved for the foreman even while no foreman turn runs. At an account cap of 1 that is 0: task workers never run on the foreman's account. The rule is on by default and can be turned off in the config (`headroom`).

## 8.3 Pause

**What a pause stops.** The user can pause dispatch for the whole engine or for one project: `brigada pause` with no flag pauses the engine, `brigada pause --repo URL` one project, as the other project-scoped actions name it, and `brigada resume` with the same scope lifts that scope's pause and no other. A job is paused when its project or the engine is.

While it is paused the lease hands out none of its jobs — work, fix, review, plan review, land and a foreman turn woken by judgment events alike, a turn already queued when the pause begins included — and the engine submits no new headless turn for a judgment event of a paused project: the events stay pending in its inbox.

A foreman turn that carries a user message (§4.2 item 3) still leases, and an attach still joins the session, for the user asking in person is not what a pause holds; such a turn and an attached chat read the pending events as they always do and may consume them, the user being present, and a plan they create is created and waits like any other work, its review job queued. Gate decisions are taken as usual, and the jobs they queue wait too.

Running jobs are not touched: each runs to its end, its result is recorded, and what it queues next — a review, a fix round, a landing — waits. That is the drain: a scope is drained when none of its jobs is leased or running. Runners keep polling and heartbeating and are offered nothing of a paused scope.

**Clocks.** A queued job of a paused scope stays queued — `brigada status` and `brigada top` count it under the pause, never as a `held:` wait for a slot (§8.1) — and the time it was paused does not elapse for it. That time is the union of the intervals in which its project or the engine was paused while the job was queued — each interval from the later of the job's queuing and the pause's start — so overlapping pauses count once and a job queued mid-pause counts only its own wait.

When the job is no longer paused by either scope, its queue TTL, a retry's `not_before` and the infrastructure-retry window of its streak (§5.1) are each shifted by that time, as the suspension rule shifts deadlines past a gap (§13), so a pause by itself never fails or parks a job. A running job's wall-time cap keeps running, and cooling, health probes and the runner-stale window go on as they do unpaused.

**Resume.** Resume lifts the pause and evaluates the held wakes at once, for the projects that neither scope pauses any longer — resuming one project under an engine pause, or the engine over a project's own pause, frees nothing of that project: per freed project, a foreman turn queued before the pause runs with the events it was given then, and the judgment events still pending coalesce into one turn — after that turn, as any event arriving during a live turn does (§4.2) — charged as one wake against the budget (§4.3), and the queued jobs lease in their usual order through the caps (§8.1).

**Storage and stopping.** A pause is stored with its scope, its start and its origin, `user` or `drain`, so it survives an idle exit, a crash and a restart. A paused engine with no live jobs exits on idle like any other: the wakes a pause holds are not pending wake-ups (§13).

`brigada stop --engine --drain` pauses the engine with origin `drain`, waits until the engine is drained, naming the jobs it still waits on, then stops the runner and the engine as `stop --engine` does; interrupting the wait stops nothing and leaves the pause in place.

A user's origin outranks a drain's: `stop --engine --drain` over a user's pause leaves its origin `user`, and `brigada pause` over a drain pause makes it the user's. `brigada start` lifts a pause of origin `drain` — a resume, with its evaluation of the held wakes, whether the engine starts then or is still up after an interrupted drain — and leaves a user's pause alone.

**What shows it.** The events `dispatch_paused` and `dispatch_resumed` record the scope, the origin and the caller, and wake no turn.

`brigada status` prints a `paused:` line per pause with its scope, its start, its origin when it is `drain` and the jobs still draining, and counts the scope's queued jobs under the pause — a foreman turn the lease gate lets through for a pending user message is not held by the pause and is not counted; `brigada top` shows it in a band of its own, apart from the view's own `p` pause, and does not count a paused queued job as held; a pause is not a needs-you entry and raises no notification: the queue holds what a rule or the foreman gives the user (§10), and the user set the pause.

Pause and resume are `client` and `admin` actions of the engine API, as plan approval is (§12.1): a `foreman` credential cannot call them.
