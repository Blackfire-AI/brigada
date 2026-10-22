# Terms of use

brigada coordinates AI coding CLIs that you already have. These are the rules it is built around, in plain words. They describe how brigada is meant to be used and what its code does; they are not legal advice.

## The rules

1. **Official CLIs only.** brigada drives the command-line tools the vendors publish (today: Claude Code, Codex, Kimi, opencode). It does not call a vendor's private endpoints, and it does not talk to a model provider on its own.
2. **Unmodified.** The CLIs run as installed. brigada does not patch them, wrap their network traffic or change their login flow. It starts them as processes, with the flags and prompts each one documents, and reads their output.
3. **Your own accounts.** Each CLI is signed in by you, through the vendor's own sign-in, with an account that is yours. brigada never asks for, copies or stores your passwords, tokens or keys.
4. **No account rotation.** brigada does not switch accounts to get around a quota or a rate limit. When an agent is out of quota, work goes to another agent you have set up, or waits.
5. **No credential pooling.** One person's logins are not shared with other people. Do not run a brigada runner for people who are not you on your accounts.
6. **No reselling.** Do not offer access to your subscriptions, or to brigada driving them, to anyone else.
7. **No telemetry, no hosted service.** brigada sends no usage data and runs no service of its own. Its store, transcripts and outcome record stay in your state directory. Three things do leave your machine, and all are yours to decide on: the official CLIs send your prompts and the code they read to their model provider, as they do when you use them by hand; the runner pushes task branches to the git remote of your repo; and the engine pushes plan files (titles, task states, dependencies and open questions) to the `brigada/plans` branch of that remote.

## API-key fallback

If a vendor's terms do not allow what you want to do with a subscription login, the fallback is to use that vendor's API with your own key instead.

brigada has no API-key setting of its own yet. Today the adapters start each CLI with your environment (minus brigada's own variables and your git-forge, cloud and registry credentials; the vendors' own `ANTHROPIC_*` and `OPENAI_*` variables stay) and leave its authentication alone, so a CLI that you have configured for API-key use, the way its vendor documents, is driven just the same. A dedicated per-adapter API-key mode is planned, not shipped. API use is billed by the vendor under its own terms, separately from any subscription.

## Vendor terms

Each vendor has its own terms for its CLI, its subscriptions and automated or headless use. **You are responsible for following the terms of every vendor whose CLI you connect to brigada.** Those terms change; reread them before you rely on them, and again when they are updated. Start from the vendor's current legal and usage-policy pages for the product you use (for example, the legal pages of Claude Code, Codex, Kimi and opencode, and of the model providers you sign in to through opencode).

The maintainers' position: driving the official, unmodified CLIs with the user's own logins, one person per account, is the use brigada is designed for, and the rules above are meant to keep it there. That is a statement of intent and design, not a claim that any vendor has approved it, and it does not replace reading the vendor's current terms. If a vendor's terms do not permit your use, do not use that vendor with brigada.

## No warranty

brigada is licensed under the Apache-2.0 license (see [LICENSE](../LICENSE)). It runs agents that edit code and run commands with your user's permissions, in git worktrees and not in containers. Review what it changes before you merge it.
