# 9. History

Everything is kept and readable.

- **Messages:** one table of conversation items per project: user ⇄ foreman turns (a user message, §4.2 item 3, is kept with author `user`), foreman tool calls and their results, worker task specs and results, questions and answers, reviews, notifications. A job's `discoveries` (§5.4) stay with that job's result message. A task's record carries its skills by name and digest and the digests of the role texts it was given (§5.9, §5.10), so a recorded prompt is reproducible. `brigada history [--task N|--job N]` shows them as one timeline.
- **Transcripts:** after every job, the runner uploads the session's full transcript (the CLI's own event log) to the engine as an artifact, linked from the job. The foreman's chat transcript is archived the same way after each attach and each headless turn.
- **Events:** an append-only event log is the backbone. Messages and transcripts reference events. A job's lease event carries the template version and the digests of the instruction and skill texts its prompt was composed from (§5.9, §5.10), and `brigada history --job N` shows them shortened.
- **Land batches:** batch events record formation and ordered membership, lease and fence, each publication attempt with predecessor and final heads, gate observations and baseline evidence, and completion. Per-member events link task, plan and batch: squash/check commit and artifact, landing recorded and push state, parking reason or environment cause, dropped dependency, hold, approval or rejection, and reservation release. A drop is a history outcome, never a landing record. Replayed reports do not duplicate events or records.

  Gate events preserve the verbatim first line, checked-head evidence and suspect list or unknown marker. Publication records retain the batch heads and members used for attribution (§7.1), including waiting prefixes released across batches; losing that evidence through retention makes suspects unknown, never reconstructed from git history.
- Retention is configurable; the default keeps everything.
