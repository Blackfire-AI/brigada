-- 0074_job_drop_waiting_ref: the stale-ref mark persisted on a land job
-- at its lease (issue #205). The engine marks a target's waiting ref
-- stale when a restart forgets the landings that sat on it, and the next
-- land job of that target must delete the ref before it stacks.
-- drop_waiting_ref records that the job's lease carried the mark: only
-- the landing of a job that carries it spends the mark, for a job of
-- the same target leased before the mark stacks on the ref as it finds
-- it and must not spend it. False on every other job and lease.
ALTER TABLE jobs ADD COLUMN drop_waiting_ref INTEGER NOT NULL DEFAULT 0;
