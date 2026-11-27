<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/logo/lockup-on-dark.svg">
    <img src="docs/brand/logo/lockup-on-light.svg" alt="brigada by Blackfire AI" width="300">
  </picture>
</h1>

**Talk to the foreman. It splits the work into tasks; brigada gives each task to the agent with the best measured record on your repo among those with quota left, checks and cross-reviews the result, and learns from the outcome.**

## Get started

You need macOS or Linux, a git repository with a remote, and a supported agent CLI signed in with your own account. Two model families enable cross-family reviews.

1. Install brigada with Homebrew ([other installation options](docs/GUIDE.md#1-install-and-brigada-init)):

   ```sh
   brew install blackfire-ai/tap/brigada
   ```

   On Linux, a release's `.deb` and `.rpm` packages install the same binary
   directly (`sudo apt install ./brigada_<version>_amd64.deb`,
   `sudo dnf install ./brigada-<version>.x86_64.rpm`); no apt or dnf
   repository is hosted, so they do not update on their own.

2. Initialize your repository. Init asks which agents to use and which checks to run, then starts brigada:

   ```sh
   cd your-repository
   brigada init
   ```

   Review, commit, and push the `.brigada/` files it writes so the foreman can read them; init never commits or pushes work for you — aside from its quick push probe, which pushes a scratch ref to your remote and deletes it to verify the push path.

3. Install the operator skill into your coding agent (the installer requires Node.js 22.20 or newer):

   ```sh
   npx skills add Blackfire-AI/brigada --skill brigada-operator
   ```

4. Open your coding agent in the repository and give it an ordinary request:

   ```text
   Ask brigada to fix #12.
   ```

The operator writes the spec, asks brigada to implement it, watches the work, verifies the result, and handles follow-ups. Watch live with `brigada top`; plan, landing, and push approvals stay yours. To talk to the foreman without an operator, run `brigada attach`.

![Terminal recording: brigada ask starts a plan of two tasks, claude/sonnet works on them, glm-5.3 reviews them, both land, and git log shows the task trailers.](docs/assets/demo-run.gif)

*Real run, cut from 35 minutes to its state changes.*

brigada drives the official coding-agent CLIs you already use, each signed in with your own account. The foreman plans the work, the dispatcher chooses an available agent, checks run against the result, a reviewer inspects it, and brigada lands approved tasks. It records every outcome locally so later choices improve.

Workers run locally in git worktrees under your harness permissions. Use brigada only with repositories you trust; agents and checks can reach the credentials and network access you give them. See [SECURITY.md](SECURITY.md) and [the guide](docs/GUIDE.md) for permissions, supported harnesses, and configuration.

## Configure with `brigada init`

Sign in to each agent CLI with its own login flow, then run `brigada init` inside your repository. Init asks you to choose the foreman, which agents may run tasks, how many tasks run at once, and the project check.

It shows the complete plan before changing anything. After you confirm, it verifies the push path with a quick probe (a scratch ref pushed to your remote and deleted), writes the project settings, role instructions, and task skills under `.brigada/`, then starts the engine and runner. Review, commit, and push those files so the foreman can read them; init never commits or pushes work for you.

Init also prints the operator-skill installation command without running it or writing harness configuration.

Use `brigada init --probe` to preview the plan. Rerun `brigada init` to change the setup, `brigada start` after a reboot, and `brigada config` to inspect the settings. The [guide](docs/GUIDE.md#1-install-and-brigada-init) covers every flag, cleanup, and removal.

## Watch and approve

Watch `brigada top`: your live view of the meters, what needs you, plans, agents and events. The operator reports what finishes and what waits.

Approve what the needs-you queue holds. A plan over your size limit, a landing a rule holds, and a push wait for you, with `brigada plan approve`, `brigada land approve` and `brigada push approve`. The decision is yours.

```sh
brigada status              # engine, jobs, plans, what needs you
brigada diff <task-id>      # what a task changed
brigada land <task-id>...   # land approved tasks by hand (the land job does it by rule)
```

## Talking to the foreman yourself

To skip the operator, run `brigada attach` inside the repository. Your attached conversation and the foreman's headless turns share one session, so you can rejoin the same work later. See [the guide](docs/GUIDE.md#2-talking-to-the-foreman-brigada-attach) for this and multi-repository sessions.

`brigada help` lists the commands; `brigada help <command>` prints one command's usage and flags.

## No telemetry

brigada sends no usage data, analytics, or crash reports anywhere and runs no hosted service. The agent CLIs still send prompts and code to their model providers under your account. The runner pushes work to your git remote, and GitHub issue reads and configured notification webhooks make network calls.

[docs/TERMS.md](docs/TERMS.md) lists everything that leaves your machine and how credentials are handled.

## License

Apache-2.0. See [LICENSE](LICENSE).

What 1.0 promises until 2.0: [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).

Changes: [CHANGELOG.md](CHANGELOG.md).
