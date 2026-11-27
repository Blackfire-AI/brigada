-- 0086_task_wait_landed: the landing-aware verification barrier's flag
-- (§6.1): whether a dependent task's release waits for landing rather
-- than mere completion. 0 (the default) releases a dependent as soon as
-- its dependencies are done; 1 holds it pending until every implement
-- dependency has actually landed on the target. The flag is set in
-- plan_create and plan_amend, shown in status, and carried in the plan
-- file (§6.4). The release rule that reads it is the wl-release change.
ALTER TABLE tasks ADD COLUMN wait_landed INTEGER NOT NULL DEFAULT 0 CHECK (wait_landed IN (0, 1));
