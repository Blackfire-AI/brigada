-- 0058_plan_review: the plan review (§6.2). A plan_review job is a
-- job of kind 'plan_review' with no task, leased by an agent of a family
-- other than the foreman's; its completion carries the verdict of a
-- review of a plan (or of an amendment that adds or edits tasks) before
-- any of the plan's work is queued. The plans table records the review's
-- state: the round count (at most 2 per creation or amendment batch),
-- the latest verdict, its findings, and the task ids the round covers
-- (empty = the whole plan, as a creation's review does).

ALTER TABLE plans ADD COLUMN review_round INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plans ADD COLUMN review_verdict TEXT NOT NULL DEFAULT '';
ALTER TABLE plans ADD COLUMN review_findings TEXT NOT NULL DEFAULT '';
ALTER TABLE plans ADD COLUMN review_batch TEXT NOT NULL DEFAULT '';
