-- 0014_job_round: the rounds budget each job spent (§7, §14), as the
-- engine's own word for what a submit's Round named: 'fix_rounds' for a
-- fix round a failed check asked for, 'review_rounds' for one a review's
-- changes asked for — both the task's fix jobs, one count against two
-- caps — and '' for every job that spends none of them, the foreman's
-- answer among them: the job that carries an answer resumes the author's
-- session without spending a round (§3), so the caps must be able to
-- tell it from a fix round however many fix jobs the task carries. The
-- restart budget keeps its own marker, the restart flag, which also
-- drives the one-live-job replacement (§5.6); a restart's round stays
-- '' so it counts against no fix budget either way.
ALTER TABLE jobs ADD COLUMN round TEXT NOT NULL DEFAULT '';

-- The upgrade's backfill: every fix job the old count ruled a fix round
-- — the fix jobs that are not restarts, exactly the set the old query
-- counted — carries one from here on.
UPDATE jobs SET round = 'fix_rounds' WHERE kind = 'fix' AND restart = 0;
