-- 0048_task_review: a task's own review tier and review effort (§7, §8).
--
-- Empty = the engine default: a review runs one tier below the task's
-- effective tier at medium effort, as the config's review.tier and
-- review.effort say. A value here wins over that default.
ALTER TABLE tasks ADD COLUMN review_tier TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN review_effort TEXT NOT NULL DEFAULT '';
