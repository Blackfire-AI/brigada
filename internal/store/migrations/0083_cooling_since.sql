-- A queued job's cooling hold began at cooling_since (§8.1): the sweep
-- parks a job that has waited on its cooling agents past cooling_wait,
-- so a quota reset days away is a visible park, never a silent hold.
ALTER TABLE jobs ADD COLUMN cooling_since TEXT;
