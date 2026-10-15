# brigada

**Run coding tasks on the best AI agent you already pay for, and learn which one that is for your repo.**

> Status: pre-alpha, private. Nothing here is usable yet.

You pay for more than one AI coding subscription: Claude, Codex, Gemini, Kimi, or others. brigada sits above their official CLIs:

- it **routes** each task to the agent with the best measured record for that kind of task on *your* repo, among those with quota left;
- it **reviews** every change with a model from a different family than the one that wrote it;
- it **checks** the work by running the task's own check command (tests, lint, a script), not by trusting "done";
- it **remembers** outcomes locally, so `brigada stats` shows which agent actually wins where.

```
brigada run "fix the flaky auth test" --check "go test ./auth/..."
brigada review --branch feat-x
brigada stats
```

brigada drives the vendors' **unmodified** CLIs, each signed in with your own account. It never pools or rotates credentials, and nothing leaves your machine.

## Status

Being built in Go: a single binary and a local SQLite store, with an MCP server so your harness can call it. The engine is moving over step by step, with its tests, from an earlier Python implementation. Plans are mirrored to `.brigada/plans/<plan-id>.yaml` on the `brigada/plans` branch. Set `snapshot_dir` in the config file (an absolute directory outside the state directory) and the engine snapshots its store daily, keeping the last seven; with the engine stopped, `brigada recover` restores the newest snapshot into a fresh store and rebuilds newer plans from their plan files. Without a snapshot, messages, transcripts and outcomes are not restored.

Transient worker CLI failures (including quota and authentication errors) and terminal runner loss get one automatic restart on a different ready agent when available. The restart uses a fresh session and branch and keeps the failure reason in its context. Set `task_auto_retry = false` in the config file to park these failures for the foreman immediately; the default is `true`. Failed checks continue through the fix-round policy.

## License

Apache-2.0. See [LICENSE](LICENSE).
