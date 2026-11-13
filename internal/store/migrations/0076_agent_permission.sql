-- 0076_agent_permission: the permission mode a worker agent runs under, as
-- the runner advertised it (§5.2): the one resolved permission= value, which
-- the job grant carries to the harness. '' for a harness with no permission
-- mode to set, and for an agent registered before the key existed.
ALTER TABLE agents ADD COLUMN permission TEXT NOT NULL DEFAULT '';
