-- 0061_plan_amend_batch: an amendment accepted under a running review
-- round (§6.2). The plans table records the task ids an amendment added
-- or respecified while the plan's review round ran: they stay pending,
-- and the round's verdict — approve included — queues the next round
-- over them instead of starting their work.

ALTER TABLE plans ADD COLUMN amend_batch TEXT NOT NULL DEFAULT '';
