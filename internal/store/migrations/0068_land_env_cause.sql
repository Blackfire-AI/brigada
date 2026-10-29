-- 0068_land_env_cause: the cause key an environmental land failure parked
-- a task on (issue #116). A landing that fails on the environment — a
-- fetch or clone that cannot reach the remote, refused credentials, a
-- lost runner — parks its task under one cause key shared by every task
-- the same cause fails, so the needs-you queue and the foreman's wake
-- name the cause once, listing the tasks, instead of one line and one
-- wake per task. The column holds the key while the task is parked at
-- needs_foreman; the queries that group by it read the state with it.

ALTER TABLE tasks ADD COLUMN land_env_cause TEXT NOT NULL DEFAULT '';
