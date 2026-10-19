-- CLI-reported usage is per job. NULL means unknown, distinct from zero.
ALTER TABLE jobs ADD COLUMN input_tokens INTEGER;
ALTER TABLE jobs ADD COLUMN output_tokens INTEGER;
ALTER TABLE jobs ADD COLUMN cached_input_tokens INTEGER;
ALTER TABLE jobs ADD COLUMN reasoning_tokens INTEGER;
ALTER TABLE jobs ADD COLUMN cost_usd REAL;
ALTER TABLE jobs ADD COLUMN wall_seconds REAL;
