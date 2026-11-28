-- 0091_land_skip_ready: a batch member's released land_ready gate (§7.1
-- "Deciding a batch hold"). tasks.land_skip_ready is 1 once the user's
-- approval of the member's land_ready hold, or the engine's passing
-- re-check of it, released the member into one next batch with its gate
-- skipped: that batch's grant marks the member, the runner exempts it from
-- the command, and the batch's completion spends the mark on every member
-- it settled — a member dropped to wait for a dependency keeps it for the
-- batch it lands in. 0 for every task that predates the column: none
-- carries a released gate.
ALTER TABLE tasks ADD COLUMN land_skip_ready INTEGER NOT NULL DEFAULT 0;
