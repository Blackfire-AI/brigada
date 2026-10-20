# Changelog

The format of this file follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
The project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - Unreleased

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
