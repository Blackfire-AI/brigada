-- 0006_runner_cap: the runner's own cap on parallel jobs (§8.1). A
-- runner advertises max_parallel when it registers; 0 means it
-- advertised none. The effective runner cap is the smaller of this and
-- the config's caps.runner, ignoring whichever is unset.
ALTER TABLE runners ADD COLUMN max_parallel INTEGER NOT NULL DEFAULT 0 CHECK (max_parallel >= 0);
