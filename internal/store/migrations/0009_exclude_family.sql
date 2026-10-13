-- 0009_exclude_family: the family rule for reviews (§7, §8). A job may
-- exclude one model family: the review job the engine queues after an
-- implement task's work passed its check excludes the author's family —
-- the family of the agent the author job ran on — so the reviewer
-- always comes from another family. '' (the default) excludes nothing.
ALTER TABLE jobs ADD COLUMN exclude_family TEXT NOT NULL DEFAULT '';
