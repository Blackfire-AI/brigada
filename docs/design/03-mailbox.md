# 3. Communication: the engine as a mailbox

```
 you ⇄ foreman ──tools──▶ engine ◀──lease/result── runner ──▶ worker CLI
            ▲                │
            └──── events ────┘  (judgment events wake the foreman)
```

- **Down:** the foreman calls tools (create a plan, add tasks, answer, restart). The engine turns ready tasks into jobs; a runner leases a job and starts the worker.
- **Up:** a worker can't message anyone mid-run. It ends its job with a structured result (§5.4). The runner adds the check result and progress data. The engine records it as events.
- **Routine events are handled by rules, judgment events go to the foreman:**

| Event | Handled by | Action |
|---|---|---|
| check failed, rounds left | rule | fix round: same session, template + check output |
| review "changes", rounds left | rule | fix round: same session, template + review text |
| transient error (CLI crash, server error, lost runner) | rule | retry the job, capped; a fetch that could not reach the remote arrives as error kind `infra` (§5.1), a failed setup as `setup` (§5.7). A `setup` retry waits a growing not-before delay before it may be leased (5s, 15s, 45s, 2m, capped): the network is not up in the second after a wake, and the delay grows per consecutive failure. An `infra` failure is retried on the backoff of §5.1, whose minutes-scale steps and hours-scale window wait an outage out rather than hammering it every second. A work job's first transient failure restarts once on another ready agent (§5.6); a fix round's transient failure retries the round once |
| a dependency is done | rule | the dependent task becomes ready |
| quota limit hit by a worker | rule | cool the account (§8); re-dispatch to another eligible agent as a **fresh** attempt (on the task's next `-r<n>` branch, §6.4) carrying the previous job's summary if it produced one, else its last progress events (a session can't move between agents), counting toward the task's attempt caps; with no eligible agent in any tier (§8), after the queue timeout the task parks and the user is notified when notifications are enabled (§10) |
| worker `needs_help` | **foreman** | answer → next job on the same session, or re-plan |
| task out of rounds or retries | rule, then **foreman** | the tier escalation of §8 first, while the task is below heavy and its attempts are not spent; then the foreman: re-plan, switch agent, split, or ask the user |
| worker stalled (§5.6) | **foreman** | restart with a new instruction, switch agent, or ask the user |
| plan held by a rule at the plan gate | **user** | approve, edit or reject (§6.3) |
| task approved, dependencies landed | rule | a `land` job, unless a rule holds the task at the land gate → **user** (§6.3, §7.1) |
| landed, check on the result passed | rule | push, unless the push gate is `ask` → **user**, or `never` (§6.3, §7.1) |
| land conflict, or the check failed on the landed result | **foreman** | nothing is pushed; restart the task with a note (rebase onto the new target head, resolve), re-plan, or ask the user (§7.1) |
| foreman limit hit (§4.4) | **user** | wait, switch foreman, or continue without it |
| job finished with `discoveries` (§5.4) | **foreman** | read them on the next turn as verbatim data in delimiters (§1.4); then `plan_amend`, put the fact into the spec of a task that hasn't started, or ignore it |
| plan done | rule | notify the user directly (§10); the foreman may add a summary on its next turn, skipped when it's `limited` or out of budget |

The transient retry is once per failure, and the one set of transient error kinds — `limit`, `auth`, `model`, `output_limit`, `other` (a server error), `runner_lost` — is read in one place by both jobs. The `output_limit` kind is a run whose last model step the CLI cut off at its output token limit (opencode reads it from the `length` finish reason of `step_finish`) and whose final message holds no readable result block; the runner completes it with the text `output limit reached: N tokens, M of them reasoning` (output plus reasoning tokens). A run that was cut off and still ended with a result block stands, a CLI error recorded in the run wins over the cut-off, and a later step that stops normally clears it. It is not a quota `limit`, so no account cools, and its completion carries an error, so escalation (§8) is skipped and the effort does not rise; where no retry applies the task parks at `needs_foreman` with that text as the reason. A work job's first transient failure restarts the task once on another ready agent: a fresh attempt on a new `-r<n>` branch carrying the failed job's last progress (§5.6). An agent whose model or account is capped at 0 (§8.1) is never that agent, however the draw would rank it: with no other agent that can take the job the restart is not spent and the task takes the ordinary failure path. The fix-round retry below picks its other agent the same way. A fix round's transient failure retries the round once, resuming the author's session on the author's agent when that agent can still take it, else on a fresh session on another agent of the tier with the round's feedback — the check's output or the review's findings — in its prompt and the task's branch under it. The retry spends no fix round (the round was never attempted), is gated by the same restart budget the work retry uses, and waits out the same not-before backoff's first step (5 s) before it leases. A second transient failure parks the task at `needs_foreman`, as a non-transient failure does at once; `task_auto_retry = false` parks transient failures at once.
