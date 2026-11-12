-- 0075_agent_permission: the permission mode an agent runs under, as the
-- runner advertised it resolved (permission=, its deprecated tools= alias
-- or the harness default). The lease copies it into the job grant, and the
-- runner hands it to the adapter, which passes it to its CLI. '' = the
-- harness's own behaviour.
ALTER TABLE agents ADD COLUMN permission TEXT NOT NULL DEFAULT '';
