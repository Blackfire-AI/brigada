-- 0066_jobs_prompt_meta: what a job's prompt was composed from (design
-- §5.9 'Versioning'): the template's version beside the digests of the
-- instruction and skill texts the prompt carried, as a JSON
-- {"template": <int>, "instructions": <sha256>, "reviewer": <sha256>,
-- "skills": [<sha256>, ...]}, so a recorded prompt is reproducible.
-- '' = the job records none: a job whose prompt carries no guidance,
-- or one queued before this migration, whose prompt stays its own
-- record.
ALTER TABLE jobs ADD COLUMN prompt_meta TEXT NOT NULL DEFAULT '';
