# Compatibility

What brigada 1.0 promises until 2.0.

## What is covered

**The CLI commands and flags.** The top-level commands are `init`, `remove`, `attach`, `ask`, `hold`, `review`, `engine`, `runner`, `start`, `config`, `stop`, `mcp`, `status`, `task`, `project`, `history`, `stats`, `cancel`, `agent`, `plan`, `budget`, `recover`, `diff`, `land`, `logs`, `version` and `help`. Their names, their subcommands and their documented flags keep working.

**The project file, `.brigada/project.yaml`, schema 1.** The file starts with `schema: 1`. The engine reads only that version. A file with any other schema number is refused with an error that names the version, so an old binary never guesses at a newer file. Within schema 1, new keys are added as optional keys. A file that worked before keeps working, and a key that is absent means the same as before.

**The foreman's MCP tools.** The `brigada mcp` bridge offers these tools to the foreman, with these arguments:

- `decision_note`: `text` (required).
- `plan_create`: `title` and `tasks` (both required). Each task takes `spec` (required), and may take `title`, `name`, `deps`, `base`, `continue`, `check`, `class`, `tier`, `effort`, `caps` and `extra_repos`.
- `plan_amend`: `plan` (required). It may take `add`, `cancel`, `retier` and `retarget`, which take the same task fields as `plan_create` or small lists of task references.
- `answer`: `task` and `text` (both required).
- `task_restart`: `task` (required) and `note`.
- `status`: `instructions`, a boolean.
- `history`: `task` and `limit`.
- `events`: `wait_seconds`.
- `capacity`: no arguments.

An interactive foreman session also gets these tools:

- `hold_acquire`: no arguments.
- `hold_yield`: no arguments.
- `task_cancel`: `task` (required).
- `ask_user`: `text` (required).
- `stats`: no arguments.

**The engine's on-disk store.** The store is SQLite and is migrated forward only. A newer binary opens an older store and applies the missing migrations on start. An older binary refuses a store that a newer binary has migrated, with an error that says to upgrade.

## What is not covered

These may change in any release, including a patch release:

- the engine's internal HTTP API;
- the format of log output and status output;
- the text of the prompts and the foreman's instructions;
- how the adapters handle the versions of the vendors' CLIs. The vendors own those, and brigada follows them.

## Breaking changes

Before 2.0, a breaking change is a bug, unless a security fix requires it. Deprecations are announced in CHANGELOG.md first.
