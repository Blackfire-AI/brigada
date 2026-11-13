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

brigada runs agent CLIs (Claude Code, Codex, Kimi, opencode, dsh, qwen-code) with your own credentials.

- Workers run in git worktrees, not containers. What a worker may do is set by your own harness's permission configuration, not by a scope of brigada's: each agent carries a `permission=` value that brigada passes to its CLI as that CLI's own permission flag or argument, and the CLI's configuration decides the rest ([GUIDE section 8](docs/GUIDE.md#scope-per-harness)). The permission is yours to set per agent, and the read-scope hook that confines a claude foreman's file reads to its reading checkout is a default you can turn off per agent. Workers inherit your environment minus the credential scrub, so a check command, a test run or a worker whose harness grants a shell can still reach your credentials and network. Do not point brigada at a repository you do not trust ([design §5.7](docs/design/05-workers.md#57-an-environment-for-checks-and-reviews)).
- Checks and setup commands run as you, in your account, though without your forge and cloud credentials (`GH_TOKEN`, `SSH_AUTH_SOCK` and the like) and without brigada's own variables. The runner's own git commands keep the credentials they need to fetch and push.
- The runner is the only path that pushes. A task worktree carries no push URL, so a worker cannot push or force-push its own branch, and only the runner's credentials reach the remote. This is not a sandbox: git run as you against an explicit URL still pushes with whatever your account holds, and a worker whose harness grants a shell runs as you.
- Approval, landing and push decisions are yours by convention. The role checks keep the foreman token out of them, and the coordinator asks for your explicit yes before it acts. They are not a security boundary against a process running as the same OS user with a shell: such a process can change brigada's state directory, settings and binary as you can.

The README carries a shorter form of the warning. Vulnerabilities in the vendor CLIs themselves belong to their vendors, and should be reported to them.
