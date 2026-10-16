-- 0025_runner_labels: what the queue TTL's busy-runner rule (§8.1) reads.
--
-- runners.labels: the labels a runner offered on its latest lease poll
-- (§8), a JSON array. The rule needs to know which runners could take a
-- job that asks for labels; '' means the runner has not polled since
-- this column existed, its labels unknown.
ALTER TABLE runners ADD COLUMN labels TEXT NOT NULL DEFAULT '';

-- jobs.slot_hold: 1 while the sweep holds the queued job at the runner
-- layer because every runner that could take it is at capacity. Only a
-- hold the sweep stamped is lifted by it; a hold a lease poll stamped is
-- the poll's own.
ALTER TABLE jobs ADD COLUMN slot_hold INTEGER NOT NULL DEFAULT 0 CHECK (slot_hold IN (0, 1));
