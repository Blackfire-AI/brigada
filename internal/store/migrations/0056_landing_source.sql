-- 0056_landing_source: how a landing was made (§7.1). The land job
-- writes 'job'; the runner's detection of a Brigada-Task trailer on the
-- project's default branch — a hand squash, or brigada land in another
-- clone — writes 'trailer'. Either is the engine's "landed" mark, read
-- wherever a landing is looked up, so a landed record from either
-- source counts.

ALTER TABLE landings ADD COLUMN source TEXT NOT NULL DEFAULT 'job' CHECK (source IN ('job', 'trailer'));
