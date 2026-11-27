-- 0093_land_batch_progress: a batch land job's per-member progress (§7.1
-- "A lost runner resumes by remote evidence"), written as it occurs by the
-- runner's progress reports so that a lost runner's next lease, or the
-- job's terminal failure, preserves what the run already decided.
--
-- One row per batch job and member. outcome is the member's settled
-- outcome in this batch once reached — 'parked' for its own content
-- failure, 'dropped' for a dependent skipped to wait — with the reported
-- member result in result (its conflict files, check log or reason, or
-- the member it waits on); '' while unsettled. A settled outcome is never
-- overwritten. Before the push the row also holds the member's input
-- (approved_head, base_ref), its local squash commit and check result
-- (commit_sha, check_passed) and the publication intent: intent is
-- 'target' or 'waiting', the ref the push is about to publish to, and
-- position the member's 1-based place in that stack. A local commit with
-- an intent is no landing record: only the remote's evidence or the
-- completion records it. The rows go with their job; existing jobs have
-- none, which is no progress.
CREATE TABLE land_batch_progress (
    job_id         INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    task_id        INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    outcome        TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('', 'parked', 'dropped')),
    result         TEXT NOT NULL DEFAULT '',
    approved_head  TEXT NOT NULL DEFAULT '',
    base_ref       TEXT NOT NULL DEFAULT '',
    commit_sha     TEXT NOT NULL DEFAULT '',
    check_passed   INTEGER NOT NULL DEFAULT 0,
    intent         TEXT NOT NULL DEFAULT '' CHECK (intent IN ('', 'target', 'waiting')),
    position       INTEGER NOT NULL DEFAULT 0,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (job_id, task_id)
);
