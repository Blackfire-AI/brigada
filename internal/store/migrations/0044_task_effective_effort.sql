-- 0044_task_effective_effort: the effort used after escalation (§8).
--
-- effective_effort starts as the task's own effort (possibly empty, which
-- the lease resolves to the agent's own level, then medium). The
-- escalation ladder steps it up on the same tier before it raises the
-- tier, and a tier change resets it to the task's effort.
ALTER TABLE tasks ADD COLUMN effective_effort TEXT NOT NULL DEFAULT '';
UPDATE tasks SET effective_effort = effort;
