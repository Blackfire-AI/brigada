-- Removed projects retain their identity, jobs and history. NULL means active.
ALTER TABLE projects ADD COLUMN archived_at TEXT;
