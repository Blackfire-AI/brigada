-- 0050_land_job: the landing of an approved task (§7.1). A land job is
-- a job of kind 'land' with no worker and no agent, leased by any runner
-- that offers git for the project's repo; its completion carries the
-- landing record. The landings table is that record, one row per landed
-- task — the engine's "landed" mark, read when a dependent's own landing
-- waits on it. land_holds records the tasks a rule held at the land gate
-- (§6.3), which the user, not the foreman, releases.

CREATE TABLE landings (
    id             INTEGER PRIMARY KEY,
    task_id        INTEGER NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,
    project_id     INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    target_branch  TEXT NOT NULL,                                  -- the branch the task was squashed onto (§6.3)
    commit_sha     TEXT NOT NULL,                                  -- the resulting commit
    pushed         TEXT NOT NULL CHECK (pushed IN ('pushed', 'waiting', 'local')),
    created_at     TEXT NOT NULL
);

CREATE INDEX idx_landings_project ON landings(project_id);

-- A task held at the land gate (§6.3): gate_land is ask, or a rule
-- tripped (a protected path, a tier, a spent restart). The user's
-- release deletes the row and re-evaluates the landing; the foreman has
-- no tool for either (§6.3). The task stays done: only its dependents'
-- landings wait on the hold (§7.1).
CREATE TABLE land_holds (
    task_id    INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    reason     TEXT NOT NULL, -- the rule's name, e.g. "heavy; touches infra/**"
    created_at TEXT NOT NULL
);
