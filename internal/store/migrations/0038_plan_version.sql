-- 0038_plan_version: a plan's version (design §4.6 mode B). An amendment
-- bumps it, so an approval that names the version the user was shown is
-- refused once the plan changed under them. 1 = never amended.
ALTER TABLE plans ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
