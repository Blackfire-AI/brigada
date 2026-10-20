-- 0046_task_dep_continue: which dependency a task continues (§6.1). A
-- task with continue names one of its own dependencies: when that dep is
-- done and the task starts, its first work job resumes the session of
-- the agent that authored the dep's approved result, instead of starting
-- cold. The flag lives on the edge like is_base, so re-pointing a task's
-- dependencies rewrites its continued dep with them. 0 = not named.

ALTER TABLE task_deps ADD COLUMN is_continue INTEGER NOT NULL DEFAULT 0 CHECK (is_continue IN (0, 1));
