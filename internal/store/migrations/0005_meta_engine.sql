-- 0005_meta_engine: which process is the engine, and since when (§13).
-- The lock file owns the single-instance rule; these columns are what the
-- engine tells the world about itself — `brigada status` reads them
-- through the API. pid NULL means no engine of this binary recorded
-- itself yet (a store seeded by hand, or an engine from before §13).
ALTER TABLE meta ADD COLUMN engine_pid INTEGER;
ALTER TABLE meta ADD COLUMN engine_started_at TEXT;
