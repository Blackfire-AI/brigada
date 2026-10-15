-- 0018_foreman_limited: the foreman's subscription-limit state (§4.4).
--
-- NULL while the project's foreman is not limited. When its CLI hit the
-- limit, the column holds the reset time (RFC3339 UTC, the engine's
-- time layout) or the word 'unknown' when the error named none. The
-- wake reads it: a reset time still ahead, or 'unknown', keeps the
-- foreman's inbox pending; a passed one is cleared lazily by the next
-- wake. Choosing another harness or model clears it too.

ALTER TABLE projects ADD COLUMN foreman_limited TEXT;
