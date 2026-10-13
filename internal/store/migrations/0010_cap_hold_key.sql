-- 0009_cap_hold_key: which instance a cap hold waits on (§8.1, §12.2).
-- cap_held_layer names the layer kind that had no room; cap_held_key
-- names the instance within it — the polling runner's id, the account
-- label, the model name, or the plan id — so the capacity snapshot
-- attributes the queued job to exactly the runner, account, model or
-- plan holding it, never to a guess from the job's harness. Global
-- holds name no instance and leave the key empty.
ALTER TABLE jobs ADD COLUMN cap_held_key TEXT;
