-- 0035_task_instructions: the project instructions a task was created
-- with (design §5.9). Resolved once, at task creation, as the default
-- check is: the effective text is copied here with its provenance, a
-- JSON {"source": "local"|"imported"|..., "hash": <import hash>,
-- "digest": <sha256 of the text>}. A later change to the project setting
-- never alters a recorded task. '' = the task has no instructions.
ALTER TABLE tasks ADD COLUMN instructions TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN instructions_meta TEXT NOT NULL DEFAULT '';
