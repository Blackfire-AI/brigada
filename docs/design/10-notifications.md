# 10. Notifications

Local OS notifications plus a "needs you" queue in `brigada status`: a plan, a landing or a push held by a rule at its gate (§6.3), a worker's question, a plan parked on a `changes` verdict a done foreman turn swallowed — the entry names the turn and keeps the findings (§6.2) — the foreman's limit, the foreman's budget out, a broken agent, a task parked with no eligible agent, a plan done.

A broken agent's entry carries its reason; for an `auth` break on a harness whose credential comes from the environment (dsh: `DEEPSEEK_API_KEY`), the reason names the variable and the time the runner process started, which the engine keeps as the runner's `started_at`: the runner sends its process start time with every register, and the engine moves `started_at` only when that time differs from the stored one, so a restart sets it and a re-poll does not; a runner that has reported none gets the reason `the runner has not reported its start` in place of the time. A harness with another kind of login keeps the bare `auth`.

The queue holds only what a rule or the foreman gave the user; a dispatch pause, which the user set, is shown beside it and is never an entry (§8.3). The foreman reaches the user through its report, not through a task state: `ask_user` records a question and parks nothing. An attached chat asks it there; a headless turn carries it in the report, and the user answers with `brigada ask`.

## Parked tasks and plans

A task parked at `needs_foreman` is the foreman's while it will handle it: its parking event has an inbox row, the foreman is not limited, and either a live attach hold sits on the project or a live foreman turn carries the row (pending or delivered). Then the task stays out of the queue of everyone but the foreman — the server moves it from `needs_you` to a separate `foreman_holds` list, so the plan tree still shows the worker's question beside it, `project list` does not count it, and the foreman's own `status` keeps the full list — and its park raises no notification.

Any other park at `needs_foreman` with an inbox row is the user's: no foreman harness, a limited foreman, a spent budget, a stalled wake, a spent outage, a cancelled turn, or a done turn that consumed the event and left the task parked. It is listed, and one `task_unattended` event announces it — a plain log record in no inbox that wakes no turn, appended when a wake settles, once per park, carrying the queue's reason and the parking event's kind; its notification reads `task #N needs you: <reason>`, and the webhook's text for a failed check never carries the check's output.

A park with no inbox row (a plan rule's hold) is the user's as before; `needs_user` is reserved, with no path into it today.

A plan parked at `needs_foreman` follows the same hold rule, using its latest `plan_reviewed` inbox row. When the foreman will not handle it, one `plan_unattended` event per park announces the queue reason, including the swallowed-turn heading when applicable; it wakes no turn. A plan awaiting approval remains the user's.

A task parked by an environmental land failure, the gate and push holds, stranded tasks and the foreman and agent entries are listed for everyone. A landing or a push that went through is a report in the history, not an entry.

## Desktop delivery

Desktop delivery is asynchronous: the engine's tick hands the log's pending events to a delivery that runs off the main loop, at most one in flight. A tick that finds a delivery still running skips the flush — nothing queues, nothing runs concurrently; the cursor is what remembers, so a skipped flush delays only the next tick's catch-up.

Each banner tool invocation is bounded twice — by its context's kill and by the exec's own wait delay past the kill — so a tool that ignores the kill, or parks a grandchild holding the pipe open, still returns the attempt within seconds.

## Webhook

`notify.webhook` in the user config file adds one optional webhook URL — never `.brigada/project.yaml` or the KB, which others may read, for the URL may carry a token. The same moments the desktop notification fires are also one small JSON POST: project, kind, the banner's one-line text, the task and plan ids, the time. The JSON is plain, so ntfy, Slack or a Telegram bot relay can take it; there are no per-service adapters.

The URL must be https; plain http is accepted only for a loopback host (`localhost`, `127.0.0.0/8`, `::1`), since the URL may carry a token. A redirect is a failed delivery and is never followed.

A failed check's text reads "its check failed" without the check's output, which stays on the desktop banner and in `brigada status`.

Delivery is fire-and-forget: a short timeout bounds each attempt, a failed attempt is retried once, and a failing webhook is logged — never with the URL — and never blocks the engine: the posts leave the engine's tick on their own bounded queue, so a slow relay delays nothing but its own backlog. The URL appears nowhere else: not in the status, the history or an error.
