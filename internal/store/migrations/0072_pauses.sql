-- 0072_pauses: the dispatch pause (§8.3). One row per pause interval,
-- scoped to the engine itself (project_id NULL) or to one project. A
-- pause stands while ended_at is NULL — at most one live pause per
-- scope, the partial index below — and origin names who set it: `user`
-- (`brigada pause`) or `drain` (`brigada stop --engine --drain`), a
-- user's origin outranking a drain's. started_at and ended_at keep the
-- interval whole after the pause lifts, for the held time a queued job
-- accumulates as the union of the pauses that covered it while it
-- waited (§8.3 "Clocks"): a lifted pause must not lose its span. The
-- row is the pause's whole storage — it survives an idle exit, a crash
-- and a restart (§8.3 "Storage and stopping").

CREATE TABLE pauses (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE, -- NULL: the engine itself, the scope of every project
    origin     TEXT NOT NULL CHECK (origin IN ('user', 'drain')), -- §8.3: who set the pause
    started_at TEXT NOT NULL,                                     -- the interval's start
    ended_at   TEXT                                               -- NULL while the pause stands; the interval's end once lifted
);

-- At most one live pause per scope (§8.3): COALESCE folds the engine's
-- NULL scope to 0 so one index covers both scopes.
CREATE UNIQUE INDEX idx_pauses_live ON pauses(COALESCE(project_id, 0)) WHERE ended_at IS NULL;
