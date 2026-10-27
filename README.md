<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/logo/lockup-on-dark.svg">
    <img src="docs/brand/logo/lockup-on-light.svg" alt="brigada by Blackfire AI" width="300">
  </picture>
</h1>

**Talk to the foreman. It splits the work into tasks; brigada gives each task to the agent with the best measured record on your repo among those with quota left, checks and cross-reviews the result, and learns from the outcome.**

You may pay for more than one AI coding subscription. brigada sits above their official command-line tools (Claude Code, Codex CLI, Kimi CLI, opencode, and dsh, DeepSeek's CLI) and uses them as workers:

- the **foreman**, one of those CLIs (claude or codex; [GUIDE section 11](docs/GUIDE.md#11-the-foremans-harness-claude-or-codex) lists what a foreman harness must do), turns what you ask for into a plan of tasks with dependencies between them. It lives in a brigada-owned session: you talk to it with `brigada attach`, and it acts headless while you are away;
- the **dispatcher** gives each task to the agent with the best measured record for that kind of task on *your* repo, among the agents that still have quota;
- every task's own **check** (tests, lint, any command) runs on the result, so "done" is not taken on trust;
- a **reviewer** from a different model family than the author reads the change;
- Independent review catches problems that checks can miss: in brigada's own development, reviewers asked for changes on 69 of 165 tasks, and every one had already passed its check. In a planted-bug exercise, reviewers caught 37 of 40 spec-breaking bugs, with each family missing exactly one; on a small sample of real backlog work, results showed no family advantage either way. Cross-family routing also spreads reviews across subscriptions, so they do not wait on or spend the author's quota.
- the **outcome** of every task is recorded locally, and `brigada stats` shows which agent actually wins where;
- **landing is brigada's**: each approved task is squashed onto the target branch, your check reruns on the result, and the push follows your push gate (`auto` by default); `brigada land` stays as the hand-driven form, and gates hold anything you want to see first.

dsh support is experimental: it was checked against one developer-preview release, needs an API key rather than a subscription, and may change in any brigada release ([docs/COMPATIBILITY.md](docs/COMPATIBILITY.md); the dsh section of [docs/GUIDE.md](docs/GUIDE.md#dsh-experimental)).

brigada drives the vendors' **unmodified** CLIs, each signed in with your own account. It uses the accounts you configure, one login per account, and when one is rate-limited it dispatches to another configured account. See [docs/TERMS.md](docs/TERMS.md).

> **Before you start.** brigada is local-first: one Go binary and a SQLite store on your machine. Workers run in git worktrees, not containers, each under its CLI's own permission scope: claude edits under `acceptEdits` with a tool allowlist (the file tools and the Go toolchain by default), codex and dsh run in an OS-level workspace-write sandbox, opencode is scoped by `OPENCODE_PERMISSION`, kimi work jobs carry no scope from brigada, and every review is read-only. The scope per harness is in [GUIDE section 8](docs/GUIDE.md#scope-per-harness). Workers inherit your environment minus the credential scrub, so a check command, a test run or a kimi worker can still reach your credentials and network. Do not point brigada at a repository you do not trust ([design §5.7](docs/design.md#57-an-environment-for-checks-and-reviews)).

## Install

Supported platforms are macOS and Linux, on amd64 and arm64. Windows is not supported. Homebrew, the install script, and prebuilt binaries need no Go toolchain.

With Homebrew:

```sh
brew install blackfire-ai/tap/brigada
brigada version
```

Or use the install script (requires curl or wget, tar, and sha256sum or shasum):

```sh
curl -fsSL https://raw.githubusercontent.com/Blackfire-AI/brigada/main/install.sh | sh
```

The script verifies the release tarball against `SHA256SUMS`, installs into `~/.local/bin` without sudo, and prints the installed version. It tells you if the directory is missing from `PATH`. Set `BRIGADA_VERSION` to pin a release (for example `1.0.0`) and `BRIGADA_INSTALL_DIR` to choose another directory; pass these variables to `sh` when using the pipeline.

To use `go install` or build from source, you need Go 1.27 or newer:

```sh
go install github.com/Blackfire-AI/brigada/cmd/brigada@latest
```

The main package lives in `cmd/brigada`, so that is the install path. Make sure `$(go env GOPATH)/bin` is on your `PATH`.

Prebuilt binaries and tarballs are also attached to each [GitHub release](https://github.com/Blackfire-AI/brigada/releases) with `SHA256SUMS` (see [Releasing](#releasing)). To download and verify a tarball manually instead of piping a script to `sh`, choose a release version and your platform (`darwin` or `linux`, `amd64` or `arm64`):

```sh
version=1.0.0
os=linux
arch=amd64
asset="brigada-${os}-${arch}.tar.gz"
base="https://github.com/Blackfire-AI/brigada/releases/download/v${version}"
curl -fsSL -o "$asset" "$base/$asset"
curl -fsSL -o SHA256SUMS "$base/SHA256SUMS"
grep "  ${asset}\$" SHA256SUMS > selected-checksum
# Stop if the checksum is missing or verification fails.
test -s selected-checksum && sha256sum -c selected-checksum &&
  tar -xzf "$asset" brigada &&
  mkdir -p "$HOME/.local/bin" &&
  mv brigada "$HOME/.local/bin/brigada" &&
  "$HOME/.local/bin/brigada" version
```

On macOS, use `shasum -a 256 -c selected-checksum` instead of `sha256sum -c selected-checksum`. Add `~/.local/bin` to `PATH` if it is not already there.

To check that a tarball was built by this repository's release workflow, install the [GitHub CLI](https://cli.github.com/) and run this in the directory with the downloaded tarball:

```sh
gh attestation verify brigada-<os>-<arch>.tar.gz --repo Blackfire-AI/brigada
```

Replace `<os>` and `<arch>` with your platform, for example `brigada-linux-amd64.tar.gz`.

To use brigada, you need:

- `git` and a repository with an `origin` remote;
- signed-in agent CLIs on `PATH`. Reviews need a model family other than the author's, so offer review-capable agents from at least two families.

### What `brigada version` prints

- A release binary from the GitHub release: the tag without its leading `v`, for example `1.0.0`.
- `go install github.com/Blackfire-AI/brigada/cmd/brigada@v1.0.0` (or `@latest` on a tagged release): the tag without its leading `v`, for example `1.0.0` — the same form a release binary prints.
- `go install ./cmd/brigada` of your checkout, or a plain `go build`: `0.0.0-dev+<rev12>` — `0.0.0-dev`, `+`, and the first 12 characters of the git commit — with `-modified` appended when the tree had uncommitted changes, for example `0.0.0-dev+3f9c2ab1d4e7-modified`.
- A build that carries no version information at all: `0.0.0-dev`.

## First run: `brigada init`

Sign in to each CLI with its own login flow, then run this inside your repository:

```sh
brigada init
```

Init walks you through four steps:

1. Pick the foreman model from the signed-in Claude and Codex CLIs, then how hard it should think. `--foreman claude/sonnet@medium` supplies both.
2. Choose which CLIs' models may run tasks. Opencode offers a menu with multiple picks. Reviews need two model families; Kimi reviews only unless you let it write code.
3. Set tasks at once on this machine (or pass `--parallel N`). Enter keeps the saved limit, else half the cores, at least 1 and at most 4.
4. Confirm the check suggested by your build files, type your own command, or enter `-` for none. Init takes worker tools from that choice and never runs the check.

Init then lists the agents, permissions and files, and shows a diff for every existing file it would change (`--verbose` also shows the contents of new files). Nothing is written or started until you confirm.

After you confirm, init:

- writes `.brigada/project.yaml`, which is yours to commit (brigada never commits or pushes it), `.brigada/operator.md`, a generated guide that tells your own coding agent how to work with brigada, and the same guide as a Claude Code skill, `.claude/skills/brigada-operator/SKILL.md` (init overwrites both on a rerun, `--no-wiring` does not skip them, and `brigada remove` deletes them). Unless `--no-wiring`, it also strips brigada's repository wiring (MCP and hook entries, the foreman files and the git exclude line); it does not touch `AGENTS.md` or `CLAUDE.md` except to remove an older init's marked block;
- starts the engine and a detached runner.

`brigada init --probe` shows the plan and writes nothing. Stop the runner with `brigada stop`; add `--engine` to stop the engine too. Every step and flag is in [the guide, section 1](docs/GUIDE.md#1-install-and-brigada-init).

Rerun `brigada init` to change agents, the foreman model and effort, or tasks at once; use `brigada start` after a reboot and `brigada config` to see every setting and how to change it.

`brigada remove` removes brigada's repository wiring, `.brigada/operator.md` and the operator skill, and archives the project, keeping its history; see [the guide](docs/GUIDE.md#removing-brigada-from-a-repository).

## The first five minutes

1. Install brigada (above) and run `brigada init` in your repository (above).
2. Open your coding agent in the repository and tell it to read `.brigada/operator.md`. Then tell it, for example, "ask brigada to fix #12". Your agent is the operator: it passes the request to the foreman through the CLI and reports back; it approves nothing.
3. Watch `brigada top`: your live view of agents, jobs, queue, plans and events.
4. Approve what the needs-you queue holds. A plan over your size limit, a landing a rule holds, and a push wait for you, with `brigada plan approve`, `brigada land approve` and `brigada push approve`; the operator tells you when something waits, and the decision is yours.

```sh
brigada status              # engine, jobs, plans, what needs you
brigada diff <task-id>      # what a task changed
brigada land <task-id>...   # land approved tasks by hand (the land job does it by rule)
```

## Talking to the foreman yourself

You can also skip the operator and talk to the foreman directly: run `brigada attach` inside the repository. The foreman is an agent in a brigada-owned session: your attached chat and the headless turns that wake on events while you are away are one transcript, running in a reading checkout of your repository with your own harness config. It plans through brigada's tools (`--model` and `--effort` override the project's foreman choice for one chat); the runner works each task on its own branch in a worktree, runs the check and pushes the branch; and a reviewer of another family reads it. See [section 2](docs/GUIDE.md#2-talking-to-the-foreman-brigada-attach).

The [guide](docs/GUIDE.md) covers the rest: a coordinator session over several repositories, watching work as it runs, configuration, and the full command reference.

`brigada help` lists the commands. `brigada land --help` (or `brigada help land`) prints a command's usage and flags, and `brigada project --help` lists its subcommands. Each subcommand has help too, for example `brigada project foreman --help`.

## No telemetry

brigada sends no usage data, analytics or crash reports anywhere. That is a promise about brigada, not about the CLIs it drives:

- each official CLI sends your prompts and the code it reads to its own model provider, as it does when you run it by hand, under your account and that vendor's terms;
- the runner pushes task branches to your repository's git remote with your git credentials;
- the engine mirrors plan files (titles, task states, dependencies, branches, open questions, extra repositories) to the `brigada/plans` branch of that remote; `brigada project set plan_files=off` turns that off.

brigada has no API-key setting of its own. Workers inherit your environment (minus brigada's own variables and your git-forge, cloud and registry credentials, such as `GH_TOKEN` or `AWS_SECRET_ACCESS_KEY`), so a CLI that you have set up with an API key the way its vendor documents is driven the same way. See [docs/TERMS.md](docs/TERMS.md).

## Releasing

Maintainers tag a version and push the tag:

```sh
git tag v1.0.0-rc.1
git push origin v1.0.0-rc.1
```

A tag with a suffix such as `-rc.1` is published as a prerelease and does not update Homebrew. A release tag without a suffix also updates `Formula/brigada.rb` in [Blackfire-AI/homebrew-tap](https://github.com/Blackfire-AI/homebrew-tap). The maintainer creates that repository and configures the `HOMEBREW_TAP_TOKEN` secret with write access to its contents before releasing.

The `release` workflow builds `brigada-<os>-<arch>` binaries (darwin and linux, amd64 and arm64) with the tag as the version, plus a `.tar.gz` of each and `SHA256SUMS`, and attaches them to a new GitHub release. For tags without a suffix, it then generates the complete Homebrew formula from the release checksums and commits and pushes it to the tap. Verify `brew audit` by hand after the release. `brigada version` prints the tag without its leading `v`; a source build prints `0.0.0-dev+<rev12>` (see [Install](#install)).

## License

Apache-2.0. See [LICENSE](LICENSE).

What 1.0 promises until 2.0: [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).

Changes: [CHANGELOG.md](CHANGELOG.md).
