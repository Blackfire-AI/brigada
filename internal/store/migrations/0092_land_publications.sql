-- 0092_land_publications: the land_ready gate's evidence (§7.1 "The
-- checked head and suspects").
--
-- land_publications records each push of brigada's that moved a land
-- target: the head it moved from (predecessor_head), the head it left
-- (final_head), the land job that pushed — a batch, a one-task landing or
-- a push phase releasing a waiting prefix — and the members it published,
-- a JSON array of {"task_id", "commit"} in landing order. The engine
-- follows these links, never git history, to name suspects.
--
-- land_baselines keeps, per project and target, the last verified green
-- baseline: the head the recognized shipped land_ready script passed for,
-- stable around the run, which is the final head of one of the recorded
-- publications (publication_id), with the remote and the command identity
-- (path and content digest) it was verified for. A hold whose evidence
-- names another remote or command identity has no baseline.
CREATE TABLE land_publications (
    id                INTEGER PRIMARY KEY,
    project_id        INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    target_branch     TEXT NOT NULL,
    predecessor_head  TEXT NOT NULL,
    final_head        TEXT NOT NULL,
    job_id            INTEGER,
    members           TEXT NOT NULL DEFAULT '[]',
    created_at        TEXT NOT NULL
);

CREATE INDEX idx_land_publications_final ON land_publications(project_id, target_branch, final_head);
CREATE INDEX idx_land_publications_predecessor ON land_publications(project_id, target_branch, predecessor_head);

CREATE TABLE land_baselines (
    project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    target_branch   TEXT NOT NULL,
    remote          TEXT NOT NULL,
    command_path    TEXT NOT NULL,
    command_digest  TEXT NOT NULL,
    head            TEXT NOT NULL,
    publication_id  INTEGER NOT NULL REFERENCES land_publications(id) ON DELETE CASCADE,
    verified_at     TEXT NOT NULL,
    PRIMARY KEY (project_id, target_branch)
);
