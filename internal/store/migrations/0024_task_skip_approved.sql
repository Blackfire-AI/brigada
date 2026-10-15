-- 0024_task_skip_approved: whether the user approved a task's skip
-- (§6.1, §12.2). A cancelled task counts toward the planner's own outcome
-- as a skip only when the user approved the cancellation; a cancellation
-- the foreman made alone is an abandoned task. 0 = not approved, or not
-- cancelled.

ALTER TABLE tasks ADD COLUMN skip_approved INTEGER NOT NULL DEFAULT 0 CHECK (skip_approved IN (0, 1));
