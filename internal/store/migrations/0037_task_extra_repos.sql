-- The extra repositories of a task (§5.8), resolved when the task is
-- created: a JSON object with the canonicalized entries, where the list
-- came from (the task or the project default) and the hash of the
-- imported file it came from. NULL = none then.
ALTER TABLE tasks ADD COLUMN extra_repos TEXT;
