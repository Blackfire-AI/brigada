-- 0041_task_tier_effort: the foreman's model tier and task effort (§6.1).
--
-- effective_tier records the tier used after escalation; initially it is
-- the foreman's tier. An empty effort uses the agent's own level.
ALTER TABLE tasks ADD COLUMN tier TEXT NOT NULL DEFAULT 'standard';
ALTER TABLE tasks ADD COLUMN effective_tier TEXT NOT NULL DEFAULT 'standard';
ALTER TABLE tasks ADD COLUMN effort TEXT NOT NULL DEFAULT '';
