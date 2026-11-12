# 11. Data model (SQLite, one file)

- `projects` (canonical repo URL, default branch, settings with the provenance of each (§13.1), foreman harness and model)
- `plans`
- `tasks` (+ `task_deps`)
- `jobs` (task, kind: `work`|`fix`|`review`|`foreman`|`land`|`plan_review`, agent, mode, prompt, lease and fence, status, result, check result, last progress, transcript ref)
- `sessions` (key, agent, runner, harness session id, state)
- attach holds (§4.2)
- pauses (§8.3: scope, the engine or a project; start; origin)
- `approval_requests` (§12.2: the act's kind and identity, the challenge's nonce and expiry, state `waiting`|`signed`|`refused`|`expired`, and the signature and enrolled key label that satisfied it; a request leaves `waiting` once, so a signature cannot verify twice across a restart)
- `runners`
- `agents` (runner, adapter, harness, model, family, account, classes, health, cooling until)
- `outcomes`
- `events` (append-only)
- `messages`
- `questions`
- `tokens` (hashed)
- `artifacts` (transcripts, check logs).
