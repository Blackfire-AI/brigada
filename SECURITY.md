# Security Policy

## Supported versions

brigada is alpha software. The latest release gets security fixes. Before 1.0, only the latest pre-release gets fixes, so upgrade to it to receive them.

## Reporting a vulnerability

Please report vulnerabilities privately, never in a public issue.

1. Open the repository's **Security** tab on github.com/Blackfire-AI/brigada.
2. Choose **Report a vulnerability**. This uses GitHub's private vulnerability reporting.
3. Include:
   - the brigada version (`brigada version`) and your OS;
   - the agent CLI and model involved, if any;
   - the steps to reproduce, and what you expected and what happened;
   - the impact you believe it has.

We aim to acknowledge reports within a few days.

## Scope and trust model

brigada runs agent CLIs (Claude Code, Codex, Kimi, opencode) with your own credentials.

- Workers run in git worktrees, not containers. A worker can do what your user can do.
- Checks run with your credentials.
- Do not point brigada at a repository you do not trust.

The README carries the same warning. Vulnerabilities in the vendor CLIs themselves belong to their vendors, and should be reported to them.
