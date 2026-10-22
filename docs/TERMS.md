# Terms of use

brigada coordinates AI coding CLIs that you already have. These are the rules it is built around, in plain words. They describe how brigada is meant to be used and what its code does; they are not legal advice.

## The rules

1. **Official CLIs only.** brigada drives the command-line tools the vendors publish (today: Claude Code, Codex, Kimi, opencode, and the experimental dsh adapter for DeepSeek's CLI; dsh always sends the code it reads to DeepSeek). It does not call a vendor's private endpoints, and it does not talk to a model provider on its own.
2. **Unmodified.** The CLIs run as installed. brigada does not patch them, wrap their network traffic or change their login flow. It starts them as processes and reads their output. To scope what they may do, it passes these settings: claude runs workers in `acceptEdits` with a tool allowlist, and headless runs ignore the repository's own settings and `.mcp.json`; codex runs with a `-s` sandbox, and its foreman with its own `approval_policy`; opencode is scoped by `OPENCODE_PERMISSION`; dsh gets `DSH_TELEMETRY_MODE=DISABLED` and a `DSH_PERMISSION_MODE`. Codex foreman MCP calls are approved by a model (`approvals_reviewer=auto_review`), not by a person. The scope per harness is in [GUIDE section 8](GUIDE.md#scope-per-harness).
3. **Your own accounts.** Each CLI is signed in by you, through the vendor's own sign-in, with an account that is yours. brigada never asks for, copies or stores your passwords, tokens or keys.
4. **Your configured accounts, one login each.** brigada uses the accounts you have configured, one login per account, and does not create or add logins of its own. When one account is rate-limited, brigada cools it and dispatches the work to another configured account, or waits.
5. **No credential pooling.** One person's logins are not shared with other people. Do not run a brigada runner for people who are not you on your accounts.
6. **No reselling.** Do not offer access to your subscriptions, or to brigada driving them, to anyone else.
7. **No telemetry, no hosted service.** brigada sends no usage data and runs no service of its own. Its store, transcripts and outcome record stay in your state directory. Four things do leave your machine, and all are yours to decide on: the official CLIs send your prompts and the code they read to their model provider, as they do when you use them by hand; the runner pushes task branches to the git remote of your repo; the engine pushes plan files (titles, task states, dependencies and open questions) to the `brigada/plans` branch of that remote; and, when you set `notify.webhook`, notifications go to the webhook URL you configured.

## API-key fallback

If a vendor's terms do not allow what you want to do with a subscription login, the fallback is to use that vendor's API with your own key instead.

brigada has no API-key setting of its own yet. Today the adapters start each CLI with your environment (minus brigada's own variables and your git-forge, cloud and registry credentials; the vendors' own `ANTHROPIC_*` and `OPENAI_*` variables stay) and leave its authentication alone, so a CLI that you have configured for API-key use, the way its vendor documents, is driven just the same. The dsh adapter takes `DEEPSEEK_API_KEY` from your environment (or dsh's own credential store), and opencode reads its provider keys from your environment or its own store; both pass through the same way. A dedicated per-adapter API-key mode is planned, not shipped. API use is billed by the vendor under its own terms, separately from any subscription.

## Vendor terms

Each vendor has its own terms for its CLI, its subscriptions and automated or headless use. **You are responsible for following the terms of every vendor whose CLI you connect to brigada.** Those terms change; reread them before you rely on them, and again when they are updated. Start from the vendor's current legal and usage-policy pages for the product you use (for example, the legal pages of Claude Code, Codex, Kimi and opencode, and of the model providers you sign in to through opencode). opencode can route to third-party model providers, and their terms are yours to check as well.

The maintainers' position: driving the official, unmodified CLIs with the user's own logins, one person per account, is the use brigada is designed for, and the rules above are meant to keep it there. That is a statement of intent and design, not a claim that any vendor has approved it, and it does not replace reading the vendor's current terms. If a vendor's terms do not permit your use, do not use that vendor with brigada.

## No warranty

brigada is licensed under the Apache-2.0 license (see [LICENSE](../LICENSE)). Its workers run in git worktrees, not containers, each under its CLI's own permission scope ([GUIDE section 8](GUIDE.md#scope-per-harness)). They inherit your environment minus the credential scrub, so a check command, a test run or a kimi worker can still reach your remaining credentials and the network. Do not point brigada at a repository you do not trust, and review what it changes before you merge it.
