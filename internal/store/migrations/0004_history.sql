-- 0004_history: the indexes the history timeline reads through (§9) and
-- the uniqueness that makes an artifact upload idempotent by content.
--
-- Timeline reads walk events and messages by time and reference; the
-- artifact index refuses a second row for the same job, kind and
-- content, so a retried upload can only find the first row, never
-- duplicate it.

-- A task's story and a job's whole timeline (§9) read through these.
CREATE INDEX idx_events_task ON events(task_id);
CREATE INDEX idx_events_job ON events(job_id);

-- One artifact row per (job, kind, content): uploads are idempotent by
-- sha256 (§9).
CREATE UNIQUE INDEX idx_artifacts_job_kind_sha ON artifacts(job_id, kind, sha256);
