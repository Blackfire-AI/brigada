-- 0080_landing_trailer: a landing recorded from a Brigada-Task trailer
-- keeps the commit's Brigada-Branch trailer (§7.1): the result branch
-- and approved head the landing claims, which the engine's start-up
-- repair pass validates against the task's own recorded branch and
-- approved head. Both are NULL on land-job rows and on rows recorded
-- before the trailer was stored; a NULL a repair pass reads is a record
-- it cannot validate, so it drops it and re-runs the task's land rule.
ALTER TABLE landings ADD COLUMN trailer_branch TEXT;
ALTER TABLE landings ADD COLUMN trailer_head TEXT;
