# 11. Data model (SQLite, one file)

- `projects` (canonical repo URL, default branch, settings with the provenance of each (§13.1) — including `branch_retention_days` (§13.2) — foreman harness and model)
- `plans`
- `tasks` (+ `task_deps`; `settled_at`, added by migration 0078, the engine clock when the task settled as `cancelled` or `retired`, NULL otherwise; `tool_grants`, added by migration 0080, a JSON array of allowlisted Claude Code permission patterns (§6.1), `''` for none)
- `jobs` (task, kind: `work`|`fix`|`review`|`foreman`|`land`|`plan_review`|`sweep`, agent, mode, prompt, lease and fence, status, result, check result, last progress, transcript ref)
- `sessions` (key, agent, runner, harness session id, state)
- attach holds (§4.2)
- pauses (§8.3: scope, the engine or a project; start; origin)
- stale land refs (project and target whose waiting ref `refs/brigada/land/<target>` the next landing deletes before it stacks)
- `runners`
- `agents` (runner, adapter, harness, model, family, account, classes, permission, health, cooling until)
- `outcomes`
- `events` (append-only)
- `messages`
- `questions`
- `tokens` (hashed)
- `artifacts` (transcripts, check logs).
