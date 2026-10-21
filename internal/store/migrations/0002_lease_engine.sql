-- 0002_lease_engine: the columns the lease engine needs (design §12.3,
-- §14, §6.4) on the jobs 0001 created. Every column carries a default,
-- so rows written by binaries that only know 0001 keep their meaning.
--
-- Timestamps follow 0001's convention: fixed-width UTC text.

-- The job's first lease (kept for the queue and history; the hard
-- caps measure from the current lease, last_leased below)...
ALTER TABLE jobs ADD COLUMN first_leased TEXT;
-- ...and wall time accrues only while a lease is in force: last_leased
-- is when the lease in force started, set at each lease and at each
-- revive, NULL once the lease has ended and its time is banked. The
-- hard caps measure from it (§14, issue #36), so a re-leased job runs
-- its next lease with the whole cap again.
ALTER TABLE jobs ADD COLUMN last_leased TEXT;
-- When the job last entered queued; the queue TTL measures from it, so
-- an expired or released job waits a fresh window, not the one from its
-- creation.
ALTER TABLE jobs ADD COLUMN queued_at TEXT;
-- meta.fence_epoch at lease time; heartbeat, complete and release from
-- an older epoch are refused (§6.4).
ALTER TABLE jobs ADD COLUMN lease_epoch INTEGER;
-- Leased wall time, banked whenever a lease ends (complete, release,
-- expiry, cap). The per-plan worker-minutes budget sums it (§14).
ALTER TABLE jobs ADD COLUMN leased_seconds INTEGER NOT NULL DEFAULT 0 CHECK (leased_seconds >= 0);
-- Placement constraints a runner must satisfy to lease the job (§8):
-- the one harness that must run it ('' = any), and the labels the
-- runner must offer in full (JSON array, '[]' = none).
ALTER TABLE jobs ADD COLUMN harness TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN labels TEXT NOT NULL DEFAULT '[]';
-- Marks jobs created by a restart (§5.6), which count toward the task's
-- restart cap.
ALTER TABLE jobs ADD COLUMN restart INTEGER NOT NULL DEFAULT 0 CHECK (restart IN (0, 1));
-- Release bookkeeping (§12.3 release): who gave the job back, until
-- when that runner doesn't see it again (the exclusion lapses), the
-- reason (bounded to 500 bytes by the engine), and the lease id of the
-- latest release, which makes release idempotent per lease id.
ALTER TABLE jobs ADD COLUMN released_by INTEGER REFERENCES runners(id) ON DELETE SET NULL;
ALTER TABLE jobs ADD COLUMN released_until TEXT;
ALTER TABLE jobs ADD COLUMN release_note TEXT;
ALTER TABLE jobs ADD COLUMN released_lease TEXT;
-- The lease id whose complete finished the job: complete is idempotent
-- per lease id, and a sweep failure must not pose as a completion.
ALTER TABLE jobs ADD COLUMN done_lease TEXT;

CREATE INDEX idx_jobs_released_until ON jobs(released_until);
