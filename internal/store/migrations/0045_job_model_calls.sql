-- Model requests the CLI reported per job. NULL means unknown, distinct from zero.
ALTER TABLE jobs ADD COLUMN model_calls INTEGER;
