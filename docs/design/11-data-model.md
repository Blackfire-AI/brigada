# 11. Data model (SQLite, one file)

- `projects` (canonical repo URL, default branch, settings with the provenance of each (§13.1) — including `branch_retention_days` (§13.2) — foreman harness and model)
- `plans`
- `tasks` (+ `task_deps`; `settled_at`, added by migration 0078, the engine clock when the task settled as `cancelled` or `retired`, NULL otherwise; `wait_landed`, added by migration 0086, whether a dependent's release waits for landing rather than mere completion (§6.1); `tool_grants`, added by migration 0087, a JSON array of allowlisted Claude Code permission patterns (§6.1), `''` for none)
- `jobs` (task, kind: `work`|`fix`|`review`|`foreman`|`land`|`plan_review`|`sweep`|`sync`, agent, mode, prompt, lease and fence, status, result, check result, last progress, transcript ref; `exclude_agents`, added by migration 0081 in place of `exclude_agent`: the JSON array of agent ids excluded for the job's current logical attempt (§7), carried forward and appended on each retry; `require_agent`, the fix round's pin (§3))
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
