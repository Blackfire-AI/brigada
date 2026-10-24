-- 0062_land_ready: the land_ready gate's marks on the task (issue #87).
-- When a task's landing last came back not ready, land_retry_at is when
-- its requeued land job may be leased again (the jobs' not-before
-- backoff) and land_ready_exit the exit the gate's command ended on;
-- the status reads both while the retry waits out the backoff.

ALTER TABLE tasks ADD COLUMN land_retry_at TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN land_ready_exit INTEGER;
