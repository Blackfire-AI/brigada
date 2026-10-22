# Security Policy

## Supported versions

The latest release of brigada gets security fixes. Upgrade to it to receive them.

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

- Workers run in git worktrees, not containers, each under its CLI's own permission scope ([GUIDE section 8](docs/GUIDE.md#scope-per-harness)). They inherit your environment minus the credential scrub, so a check command, a test run or a kimi worker can still reach your credentials and network. Do not point brigada at a repository you do not trust ([design §5.7](docs/design.md#57-an-environment-for-checks-and-reviews)).
- Checks and setup commands run as you, in your account, though without your forge and cloud credentials (`GH_TOKEN`, `SSH_AUTH_SOCK` and the like) and without brigada's own variables. The runner's own git commands keep the credentials they need to fetch and push.

The README carries the same warning. Vulnerabilities in the vendor CLIs themselves belong to their vendors, and should be reported to them.
