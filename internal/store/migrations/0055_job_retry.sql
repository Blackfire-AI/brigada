-- 0055_job_retry: marks a fix round's retry after a transient CLI
-- failure (§3, issue #41). The retry re-runs the same round — the same
-- session on the author's agent when it can still take it, else a fresh
-- session on another agent of the tier — so it is not a restart: it
-- keeps the task's branch and spends no fix round, the round having
-- never been attempted. The flag is what lets a second transient
-- failure park the task instead of retrying again.
ALTER TABLE jobs ADD COLUMN retry INTEGER NOT NULL DEFAULT 0 CHECK (retry IN (0, 1));
