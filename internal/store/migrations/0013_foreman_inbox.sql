-- 0013_foreman_inbox: the foreman's inbox (§4.2): one row per judgment
-- event — every event that parks a task at needs_foreman, plus plan_done,
-- so the plan's finish is reported too. The row is written in the same
-- transaction as its event and follows it through a turn: state is
-- pending while no turn has carried it, delivered once a foreman turn was
-- woken with it, and consumed when a turn completed done having carried
-- it; a failed, stalled or cancelled turn returns its rows to pending.
-- job_id names the turn a delivered row belongs to, and a row that
-- returns to pending keeps it — the last turn that carried the event.
-- attempts counts the failed and stalled turns that carried the row: a
-- cancel is the user's own stop and never counts, and the wake stalls
-- only when every pending row has been carried to failure three times,
-- so an event that arrives mid-run starts at zero and keeps the wake
-- answering it.
CREATE TABLE foreman_inbox (
    event_id   INTEGER PRIMARY KEY REFERENCES events(id),
    project_id INTEGER NOT NULL REFERENCES projects(id),
    state      TEXT NOT NULL,
    job_id     INTEGER REFERENCES jobs(id),
    attempts   INTEGER NOT NULL DEFAULT 0
);

-- The wake's two reads: a project's pending rows, and the live turn that
-- answers for them.
CREATE INDEX foreman_inbox_pending ON foreman_inbox(project_id, state);

-- The upgrade's backfill: every task a binary before this table already
-- parked at needs_foreman gets one pending row for its latest parking
-- event, so the first engine start after the upgrade wakes for the parks
-- the old log holds. A parked task with no parking event of its own —
-- none the engine writes parks without one — simply gets no row.
INSERT INTO foreman_inbox (event_id, project_id, state)
SELECT e.id, pl.project_id, 'pending'
FROM tasks t
JOIN plans pl ON pl.id = t.plan_id
JOIN events e ON e.id = (
    SELECT MAX(e2.id) FROM events e2
    WHERE e2.task_id = t.id AND e2.kind IN (
        'task_check_failed', 'task_needs_help', 'task_failed',
        'task_out_of_rounds', 'review_no_verdict'))
WHERE t.state = 'needs_foreman';
