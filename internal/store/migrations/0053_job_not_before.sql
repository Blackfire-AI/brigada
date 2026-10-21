-- A job queued for later (§3, §4.2, issue #46): the engine's retry and
-- re-wake delays write a not-before time here, and leasing skips the job
-- until the clock reaches it. NULL means the job is leasable at once.
ALTER TABLE jobs ADD COLUMN not_before TEXT;
