-- 0016_job_branch: the branch the engine names each job (§5.1, §5.6).
--
-- Set at a work or fix job's first lease and kept by every later lease
-- of the same job, so a re-leased attempt keeps its own branch name
-- instead of taking a fresh one per lease. A work job is named for its
-- task and attempt (brigada/task-<id>, -r<n> after the task's n-th
-- restart); a fix job continues the task's current branch. Reviews and
-- foreman turns name none: a review reads the task's branch without
-- moving it, and a foreman turn belongs to no task.

ALTER TABLE jobs ADD COLUMN branch TEXT;
