-- 0063_land_ready_skip: the land_ready gate's skip mark on a land job
-- (issue #87). skip_land_ready is set on the land job a `brigada land
-- approve` queues to release the gate's exit-1 hold, so the runner lands
-- without re-running the command. False on every other job.

ALTER TABLE jobs ADD COLUMN skip_land_ready INTEGER NOT NULL DEFAULT 0;
