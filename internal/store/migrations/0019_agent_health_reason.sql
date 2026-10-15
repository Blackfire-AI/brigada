-- 0019_agent_health_reason: why an agent is broken (§8 "Health").
--
-- Empty while the agent is ready. When the engine marks an agent broken
-- it names the reason: 'auth', 'repeated failures' or 'runner stale'.
-- The runner's coming back clears only 'runner stale'; the other
-- reasons wait for the user's reset.

ALTER TABLE agents ADD COLUMN health_reason TEXT NOT NULL DEFAULT '';
