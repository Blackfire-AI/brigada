-- 0088_task_note: the task's pending note (§6.2): tasks.note holds the
-- words the foreman or the user left on a task through plan_amend's note
-- list, for the task's next work or fix job to carry in its prompt; NULL
-- when none is pending. The first lease of a job that carries it clears
-- the column, and an empty note clears it by hand. Every task that
-- predates the column has no pending note.
ALTER TABLE tasks ADD COLUMN note TEXT;
