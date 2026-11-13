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

brigada runs agent CLIs (Claude Code, Codex, Kimi, opencode, dsh, qwen-code) with your own credentials. What each one may do in its own CLI is owned by that CLI's own permission mechanism, as your harness configuration sets it, and passed through per agent with the `permission=` key ([GUIDE section 8](docs/GUIDE.md#scope-per-harness)); brigada sets no tool allowlist or sandbox mode of its own to replace it.

- Workers run in git worktrees, not containers. They inherit your environment minus the credential scrub, so a check command, a test run or a kimi worker can still reach your credentials and network. Do not point brigada at a repository you do not trust ([design §5.7](docs/design/05-workers.md#57-an-environment-for-checks-and-reviews)).
- Checks and setup commands run as you, in your account, though without your forge and cloud credentials (`GH_TOKEN`, `SSH_AUTH_SOCK` and the like) and without brigada's own variables. The runner's own git commands keep the credentials they need to fetch and push.
- **A task's worktree carries no push URL.** A worker cannot push, or force-push, its own branch — there is no remote to push it to. Only the runner pushes, from its own clone under the state directory with its own git credentials, and only at landing. A worker whose harness grants it a shell can still run `git push` as you, against some other, explicit URL; that push reaches the network with whatever credentials your account holds, the same as if you ran it yourself — a worktree with no push URL closes the task's own branch to it, not every path to the network.

### Approval by signature

Approving or rejecting a plan at its version, approving or rejecting a landing, and approving a push each need, beyond whatever API token made the call, a signature over a one-time challenge the engine issues for that one act. The engine stores public keys only; your private key stays yours, held outside brigada's state directory and never in it, so reading that directory yields nothing that signs. A signature covers its one challenge alone, so it cannot be replayed for another act, another plan version or a later moment, and the mechanism fails closed: with no enrolled key there is no signature, and the act is refused; a headless run never prompts for one, and an approval nobody signs stays waiting.

**The residual risk is plain.** A private key that is an ordinary file the same OS user can read lets an agent with a shell sign on your behalf — such a key defends nothing against a worker or a foreman whose harness grants it a shell. Only a key that needs a human at the moment of signing defends against that: a hardware token that asks for a touch, an agent key constrained to ask for one on each use, or a key kept off the machine entirely. Treat your approval key with the same care as a production deploy credential, not as a convenience token.

**Host requirement.** brigada signs and verifies approval challenges by running `ssh-keygen -Y sign`/`-Y verify`; the host that runs the engine's API must have `ssh-keygen` on `PATH`. The suites that exercise a gated act end to end fail, never skip, when that binary is absent on the host that runs them.

The README carries the same warning. Vulnerabilities in the vendor CLIs themselves belong to their vendors, and should be reported to them.
