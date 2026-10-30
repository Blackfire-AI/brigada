# Compatibility

What brigada 1.0 promises until 2.0.

**Experimental** marks a surface that is not frozen yet: until 2.0 it may change in any release, including a patch release, and what is promised about it is that it is there, not how it works.

## What is covered

**The CLI commands and their subcommands.** The top-level commands are `init`, `remove`, `instructions`, `attach`, `ask`, `review`, `engine`, `runner`, `start`, `config`, `stop`, `mcp`, `status`, `task`, `project`, `history`, `stats`, `capacity`, `top`, `cancel`, `agent`, `agents`, `plan`, `budget`, `recover`, `diff`, `land`, `push`, `logs`, `version` and `help`. The subcommands are `instructions diff`; `task submit`; `project foreman`, `project list`, `project set`, `project show` and `project import`; `agent reset`; `agents add`; `plan approve` and `plan reject`; `land approve` and `land reject`; `push approve`; and `hold yield` and `hold confirm`, which stay as no-ops that always exit 0. A bare `brigada` is `attach`. Their names and subcommand names keep working.

**The CLI flags.** Every command takes `--state-dir DIR` and `--config PATH`. A command's other flags are frozen only where they are listed here; anything else its `--help` prints may change. The frozen flags:

- `init`: `--repo`, `--remote`, `--yes`, `--probe`, `--verbose`, `--accept-limited`, `--check`, `--tools`, `--foreman`, `--agent` (repeatable), `--no-wiring`, `--parallel`, `--all`, `--overwrite-config`, `--signed-in`, `--now`, `--take-default`.
- `remove`: `--yes`, `--cancel`, `--delete-config`.
- `attach`: `--force`, `--no-start`, `--model`, `--effort`.
- `ask`: `--repo`.
- `review`: `--branch` (required), `--repo`, `--no-start`.
- `engine`: `--idle-minutes`.
- `runner`: `--harness`, `--model`, `--worker-script`, `--agent-file`, `--classes`, `--labels`, `--max-parallel`, `--agent` (repeatable), `--no-start`.
- `start`, `config`, `version`, `agents`, `hold yield`, `hold confirm`: no flags beyond the common two.
- `stop`: `--engine`.
- `mcp`: `--coordinator`, `--client`, `--socket`.
- `status`: `--line`, `--all`, `--no-start`.
- `task submit`: `--repo`, `--base`, `--spec`, `--check`, `--title`, `--harness`, `--no-start`.
- `project foreman`: `--repo`, `--harness`, `--model`, `--effort`, `--no-start`.
- `project list`: `--no-start`.
- `project set` and `project show`: `--repo`, `--no-start`.
- `project import`: `--repo`, `--file`, `--yes`, `--no-start`.
- `history`: `--task`, `--job`, `--limit`, `--no-start`.
- `stats`: `--no-start`.
- `capacity`: `--project`, `--no-start`.
- `top`: `--interval`, `--project`, `--no-start`.
- `cancel`: `--no-start`.
- `agent reset`: `--no-start`.
- `agents add`: `--classes`, `--effort`, `--now`.
- `plan approve`: `--and-stop-asking`, `--no-start`; `plan reject`: `--no-start`.
- `budget`: `--repo`, `--wakes`, `--no-start`.
- `recover`: `--repo` (repeatable), `--snapshot-dir`.
- `diff`: `--stat`, `--repo`, `--no-start`.
- `land`: `--onto`, `--repo`, `--landed` (repeatable), `--no-check`, `--no-start`; `land approve`: `--and-stop-asking`, `--no-start`; `land reject`: `--no-start`. The command and flag names are frozen; the land gate's holds and the land job's internals are experimental.
- `push approve`: `--repo`, `--no-start`. The push gate's mechanics are experimental.
- `logs`: `--check`, `--follow`, `--tail`, `--task`, `--no-start`.

**The project file, `.brigada/project.yaml`, schema 1.** The file starts with `schema: 1`. The engine reads only that version. A file with any other schema number is refused with an error that names the version, so an old binary never guesses at a newer file. The format is the flat subset: top-level `key: value` entries whose value is a scalar, a flow list or a block list; the `instructions` role map is the supported nested-map exception. Schema 1's keys, and nothing else:

- `schema` (required): `1`.
- `repo` (required): the repository's canonical URL.
- `instructions_file`: the compatibility alias for `instructions.worker`, a repo-relative markdown file. Setting both keys is refused.
- `instructions`: a map from `foreman`, `plan_review`, `worker` and `reviewer` to repo-relative files; an empty path uses `.brigada/instructions/<role>.md`.
- `skills_dir`: the repo-relative skill directory, default `.brigada/skills`.

The role map, `skills_dir`, and optional `skills` tool arguments are additive 1.x extensions. Existing files using only `instructions_file` remain valid.
- the project settings: `approval_max_tasks`, `approval_budget` (experimental), `approval_classes`, `approval_protected_paths`, `gate_plan`, `gate_land` (experimental), `gate_push` (experimental), `approval_tiers`, `approval_restarted`, `land_target` (experimental), `after_land` (experimental), `land_ready` (experimental), `default_check`, `worker_tools`, `setup` (experimental), `setup_cache_key_files` (experimental), `setup_timeout_minutes` (experimental), `extra_repos`, `foreman_rotate_tokens` and `plan_files` (experimental).

Within schema 1, new keys are added as optional keys. A file that worked before keeps working, and a key that is absent means the same as before. An unknown key is refused with the key named. The plan review is experimental: `plan_review` is a local-only setting, so the file refuses it by name, and the dotted `plan_review.tier` and `plan_review.effort` cannot be written in the flat schema at all — a key with a dot is refused as a malformed line; both are set with `brigada project set`. `foreman_wakes_per_day` is local-only and set with `brigada budget --wakes`. The local setting `instructions` remains the worker-text override; the committed `instructions` key takes a role map, not inline text.

**The config file** — `$XDG_CONFIG_HOME/brigada/config`, else `~/.config/brigada/config`, or the path `--config` names. One `key = value` per line; an unknown key is refused, so a typo cannot silently tune nothing. Its keys are `state_dir`, `idle_minutes`, `notify`, `notify.webhook`, `snapshot_dir`, `headroom`, `default_reviewer`, `task_auto_retry`, `foreman_wakes_per_day`, `rounds.fix`, `rounds.review` and `rounds.restart` (all three experimental), `review.tier`, `review.effort`, the concurrency layers `caps.global`, `caps.plan`, `caps.runner`, `caps.account.<label>` and `caps.model.<model>`, `cost.<harness>/<model>`, `tier.<harness>/<model>`, `tools.claude/<model>` and `env.<harness>.<VAR>`.

**The foreman's MCP tools.** The `brigada mcp` bridge offers these tools to the foreman — the same set for a headless turn and an attached chat — with these arguments:

- `plan_create`: `title` and `tasks` (both required). Each task takes `spec` (required), and may take `title`, `name`, `deps`, `base`, `continue`, `check`, `class`, `tier`, `effort`, `review_tier`, `review_effort`, `caps`, `extra_repos` and `skills`.
- `plan_amend`: `plan` (required). It may take `add`, `cancel`, `retier`, `respec` and `retarget`. `add` takes the same task fields as `plan_create`; `cancel` is a list of task references (by name or id); `retier` (experimental) is a list of entries naming a `task`, each of which may set `tier`, `effort`, `review_tier`, `review_effort`, `caps` (merged key by key) and `skills`; `respec` (experimental) is a list of entries naming a `task` with a `spec`, a `mode` (`replace` or `append`) and `skills`; `retarget` is a list of entries naming a `task` with its new `deps`, and may set `base` and `continue`.
- `plan_accept` (experimental): `plan` and `reason` (both required). It accepts a plan the plan review parked at `needs_foreman` as reviewed, and the plan proceeds as a spent round's verdict does.
- `answer`: `task` and `text` (both required).
- `task_restart`: `task` (required) and `note`.
- `task_cancel`: `task` (required).
- `land_retry` (experimental): `task` (required).
- `ask_user`: `text` (required).
- `status`: `instructions` and `all`, both booleans.
- `history`: `task` and `limit`.
- `issue_read` (experimental): `number` (required) and `comments`, a boolean; it reads the project's own repository alone, never one the call names.
- `events` (experimental): `wait_seconds`.
- `capacity`: no arguments.
- `stats`: no arguments.

**The client's MCP tools.** `brigada mcp --client` offers these tools to any harness acting for the user — a coding session, a coordinator — with these arguments:

- `ask_foreman`: `text` (required) and `repo_url`.
- `status`: `all`, a boolean.
- `history`: `task` and `limit`.

**The coordinator's MCP tools** (experimental). `brigada mcp --coordinator` offers these tools over several projects, with these arguments:

- `list_projects`: no arguments.
- `ask_foreman`: `repo_url` and `text` (both required).
- `status`: `repo_url` and `all`.
- `history`: `repo_url`, `task` and `limit`.
- `plan_approve`: `repo_url`, `plan` and `version` (all required), and `and_stop_asking`.
- `plan_reject`: `repo_url`, `plan` and `version` (all required).

**The engine's on-disk store.** The store is SQLite and is migrated forward only. A newer binary opens an older store and applies the missing migrations on start. An older binary refuses a store that a newer binary has migrated, with an error that says to upgrade.

## What is not covered

These may change in any release, including a patch release:

- the engine's internal HTTP API;
- the format of log output and status output;
- the text of the prompts and the foreman's instructions;
- the dsh adapter, which is experimental and may change in any release;
- the time caps: the engine's hard caps on jobs by kind and the runner's per-class time and tool caps. They are experimental, not frozen until 2.0;
- how the adapters handle the versions of the vendors' CLIs. The vendors own those, and brigada follows them.

## Breaking changes

Before 2.0, a breaking change is a bug, unless a security fix requires it. Deprecations are announced in CHANGELOG.md first.
