-- 0028_effort: the reasoning-effort level a foreman or an agent runs at.
--
-- projects.foreman_effort is the user's choice for the project's foreman
-- (`brigada project foreman --effort`); agents.effort is the level a
-- runner advertised for the agent (`--agent ...,effort=<level>`). '' means
-- unset: the CLI's own default, as before this column existed. Effort is
-- not part of an agent's identity: outcomes still key by harness and model.
ALTER TABLE projects ADD COLUMN foreman_effort TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN effort TEXT NOT NULL DEFAULT '';
