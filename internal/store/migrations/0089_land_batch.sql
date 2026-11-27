-- 0089_land_batch: batch landing's reservation (§7.1). tasks.land_batch_job
-- names the live batch job that holds the task's landing, or NULL when no
-- batch holds it; the batch job is a task-less job of kind 'land', keyed
-- land-batch:<project>. SQLite cannot alter a CHECK in place; Go edits the
-- jobs pairing CHECK in the migration's transaction (widenLandBatchCheck),
-- leaving every row, index and foreign key reference untouched, as 0084
-- did for 'sync'. Every task that predates the field is reserved by no
-- batch.
ALTER TABLE tasks ADD COLUMN land_batch_job INTEGER REFERENCES jobs(id) ON DELETE SET NULL;
