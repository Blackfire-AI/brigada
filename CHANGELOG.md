# Changelog

The format of this file follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
The project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - Unreleased

### Removed

- Mode A is retired: your own harness session no longer holds the foreman. The `hold` command, `brigada mcp --foreman`, the settings `hold_idle_minutes`, `hold_max_minutes` and `interactive_quiet_minutes`, and the tools `hold_acquire`, `hold_yield` and `decision_note` are gone; `brigada attach` is the interactive foreman, joining the brigada-owned session.
- `brigada hold yield` and `hold confirm` remain as no-ops so old hooks do not block.

### Fixed

- claude and opencode workers could not read a task's extra repositories (§5.8): their read and search tools were denied outside the worktree, and their scoped Bash could not echo the `BRIGADA_REPO_<NAME>` path. The runner now grants each checkout's absolute path as a read dir (claude `--add-dir`, opencode `external_directory` allow) and names the paths in the job's prompt.
- dsh jobs failed at start with "unknown option '--patch'"; the adapter now passes the launcher's `--patch` before the app's `--json`.

### Added

- Your own Claude Code or Codex session is the foreman after `brigada init` (mode A), wired through the `AGENTS.md` router block, `.brigada/foreman.md`, `.mcp.json` and `.codex/config.toml`.
- Mode B: `brigada init` in a parent folder registers several repositories under one coordinator session (`brigada-coordinator` in `.mcp.json`).
- Plans with dependencies, created and amended by the foreman, mirrored to the `brigada/plans` branch.
- Workers run in git worktrees on their own branches; the project's check runs after every result; fix rounds, and restarts with notes (`task_restart`).
- Cross-family review of each `implement` change, and `brigada review --branch` for any branch.
- Dispatch by outcome record and cost weight within a tier (Thompson sampling); concurrency caps per runner, account, model and plan.
- Quota cooling per account, until the vendor's reset time when the CLI gives one.
- Tiers (`light`, `standard`, `heavy`) and effort per task, with an escalation ladder that raises effort first, then the tier.
- A task may `continue` one of its deps: its first work job resumes the dep author's session on its own agent, saving the cold start; fresh whenever the session or its agent is not available.
- `brigada stats`: usage per job and cost per success.
- `brigada land` squashes approved tasks onto your branch locally; it never pushes.
- Adapters for Claude Code, Codex, Kimi and opencode.
- `brigada status`, `brigada history`, `brigada logs`, `brigada diff` and desktop notifications.
- `docs/COMPATIBILITY.md` states what 1.0 promises until 2.0.
- `brigada attach --model M --effort E` overrides the project's foreman model and effort for that chat only.

### Changed

- `brigada init` no longer wires the harness; a rerun or `brigada remove` strips an older init's wiring.
- The headless foreman and `brigada attach` now run in the foreman's reading checkout, so the repository's own `CLAUDE.md`, `AGENTS.md`, `.claude/` and project memory load as in any clone. A harness keys its sessions by working directory, so existing foreman sessions cannot be resumed from the new one: the next turn or attach starts a fresh foreman session once, and plans, tasks and pending events are unaffected. A resume the CLI rejects as unknown starts a fresh session instead of failing the turn.
- `brigada attach` no longer passes `--strict-mcp-config` to `claude`, so your own user-scope MCP servers load as in any session. A repository whose `.mcp.json` still has an older init's `brigada` entry (`brigada mcp --foreman`) must drop it (`brigada remove`), or the two `brigada` servers clash.
