-- 0090_task_stop_pending: the pending stop-running restart (§5.6).
-- tasks.stop_pending holds the note of a task_restart called with
-- stop_running on a task whose live job was leased or running: the engine
-- answers that job's heartbeats stop_running, and the job's end — the
-- stopped run's stalled completion, or any other terminal end — consumes
-- the marker as the fresh attempt, the note riding its prompt. An empty
-- string is a pending restart with no note; NULL when none is pending.
-- task_cancel, task_retire and a cancel of the live job clear it.
-- jobs.seed_branch names the branch a restart's fresh work job starts its
-- worktree from instead of the task's base: the stopped run's pushed
-- branch, NULL for every other job. Every task and job that predates the
-- columns has no pending restart and no seed.
ALTER TABLE tasks ADD COLUMN stop_pending TEXT;
ALTER TABLE jobs ADD COLUMN seed_branch TEXT;
