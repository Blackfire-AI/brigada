-- 0078_task_settled_at: the engine clock when a task settled into a
-- state its branch can never land from again (§13.2) — cancelled by a
-- plan amendment or a plan's close, or retired. The reconcile sweep
-- reads it to hold the task's branch family for branch_retention_days
-- before deleting it. NULL for every task that has not settled.
ALTER TABLE tasks ADD COLUMN settled_at TEXT;
