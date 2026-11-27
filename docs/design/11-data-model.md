# 11. Data model (SQLite, one file)

- `projects` (canonical repo URL, default branch, settings with the provenance of each (§13.1) — including `branch_retention_days` (§13.2) — foreman harness and model)
- `plans`
- `tasks` (+ `task_deps`; `settled_at`, added by migration 0078, the engine clock when the task settled as `cancelled` or `retired`, NULL otherwise; `wait_landed`, added by migration 0086, whether a dependent's release waits for landing rather than mere completion (§6.1); `tool_grants`, added by migration 0087, a JSON array of allowlisted Claude Code permission patterns (§6.1), `''` for none; `note`, added by migration 0088, the task's pending note (§6.2), set by `plan_amend`'s `note` list for its next work or fix job's prompt and cleared at that job's first lease, NULL when none; `land_batch_job`, added by migration 0089, the id of the batch land job that reserves the task's landing, NULL when none does (§7.1): written by the formation in the batch's submit transaction, cleared when the batch completes, a trailer records the task's landing, or the sweep finds the job ended without a completion; a reservation naming a job that is no longer queued, leased or running is no ownership (§12.3); `stop_pending`, added by migration 0090, the pending stop-running restart (§5.6): the note of a `task_restart` with `stop_running` whose job a runner holds, `''` for no note, consumed as the fresh attempt when that job ends and cleared by a cancel or retirement, NULL when none is pending; `land_skip_ready`, added by migration 0091, 1 once the user's approval of the task's `land_ready` hold or the engine's passing re-check released it into one next batch with its gate skipped (§7.1 "Deciding a batch hold"), spent by the completion of the batch that settles it — kept by a member dropped to wait for its dependency — 0 otherwise)
- `jobs` (task — NULL for a foreman, review, plan_review, sweep or sync job, and allowed for the batch `land` job keyed `land-batch:<project>`, which names its lead member, the batch's first (§7.1, §12.3); kind: `work`|`fix`|`review`|`foreman`|`land`|`plan_review`|`sweep`|`sync`, agent, mode, prompt, lease and fence, status, result, check result, last progress, transcript ref; `exclude_agents`, added by migration 0081 in place of `exclude_agent`: the JSON array of agent ids excluded for the job's current logical attempt (§7), carried forward and appended on each retry; `require_agent`, the fix round's pin (§3); `seed_branch`, added by migration 0090, the branch a stop-running restart's fresh work job starts its worktree from — the stopped run's pushed branch — while the task's `base_ref` stays its base, NULL for every other job (§5.6))
- `sessions` (key, agent, runner, harness session id, state)
- attach holds (§4.2)
- pauses (§8.3: scope, the engine or a project; start; origin)
- stale land refs (project and target whose waiting ref `refs/brigada/land/<target>` the next landing deletes before it stacks)
- land syncs (one row per project: the sync's state — current, waiting, conflict or behind_stack — the count of default-branch commits the target lacks, both heads, and the recorded merge with its push state, §7.1)
- `runners`
- `agents` (runner, adapter, harness, model, family, account, quota_scope, classes, permission, health, cooling until)
- `outcomes` (per project, agent, class and skill count: attempts and successes, the quality history of §8)
- `class_cooldown` (per agent and class, added by migration 0082: the consecutive contract-failure streak, the cooldown expiry and the backoff step of the class-scoped reliability cooldown (§8); a substantive verdict resets the row, and `agents.cooling_until` stays the quota-scope clock)
- `events` (append-only)
- `messages`
- `questions`
- `tokens` (hashed)
- `artifacts` (transcripts, check logs).
