-- 0027_task_dep_base: which dependency a task stacks on (§6.1). A task
-- is based on the result branch of one of its own dependencies: the one
-- the plan's author named (is_base = 1), or the only one when it has
-- exactly one. With several dependencies and none named, the task bases
-- on the project's default branch. The flag lives on the edge, so
-- re-pointing a task's dependencies rewrites its base with them. The
-- engine resolves the base into tasks.base_ref when the task's work is
-- queued. 0 = not named as the base.

ALTER TABLE task_deps ADD COLUMN is_base INTEGER NOT NULL DEFAULT 0 CHECK (is_base IN (0, 1));
