-- The project's setup step (§5.7), copied into the task record when the
-- task is created, as the check is: a JSON object with the command, the
-- cache key files, the timeout, and where the setting came from. NULL =
-- the project had no setup then.
ALTER TABLE tasks ADD COLUMN setup TEXT;
-- The setup log is an artifact of its own kind (§5.7, §9). The artifacts
-- CHECK is widened in place by schemaEdits[35].
