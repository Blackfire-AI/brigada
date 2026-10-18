-- The adapter's kind of a failed completion ('limit', 'auth', 'setup',
-- 'other', ...), kept so the review retry rules can tell a reviewer that
-- could not run from one that ran and gave an unusable answer without
-- parsing the error text. '' = no error, or a job that ended without a
-- runner's completion.
ALTER TABLE jobs ADD COLUMN error_kind TEXT NOT NULL DEFAULT '';
