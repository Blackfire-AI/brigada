-- 0007_cap_hold: the queue's memory of a cap hold (§8.1). When a lease
-- poll reaches a queued job and a concurrency cap has no room for it,
-- the engine stamps the hold on the job: cap_held_at is when a runner
-- last found it blocked, and cap_held_layer names the layer that was
-- full. The queue TTL counts from the later of queued_at and cap_held_at,
-- so a job runners keep finding cap-blocked waits as long as it takes;
-- a job no runner can even try — a harness nobody offers — is never
-- held and still expires.
ALTER TABLE jobs ADD COLUMN cap_held_at TEXT;
ALTER TABLE jobs ADD COLUMN cap_held_layer TEXT;
