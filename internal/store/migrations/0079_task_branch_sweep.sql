-- 0079_task_branch_sweep: the engine-start reconcile sweep (§13.2) is a
-- task-less job of kind 'sweep'. SQLite cannot alter a CHECK in place; Go
-- edits the jobs kind and pairing CHECKs in the migration's transaction,
-- leaving every row, index and foreign key reference untouched.
SELECT 1;
