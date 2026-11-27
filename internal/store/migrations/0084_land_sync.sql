-- 0084_land_sync: the land-target sync (§7.1) is a task-less job of kind
-- 'sync', one per project at a time, and its standing record lives in
-- land_syncs. SQLite cannot alter a CHECK in place; Go edits the jobs
-- kind and pairing CHECKs in the migration's transaction
-- (widenLandSyncChecks), leaving every row, index and foreign key
-- reference untouched, as 0079 did for 'sweep'.
CREATE TABLE land_syncs (
    project_id     INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    state          TEXT NOT NULL,                 -- current, waiting, conflict or behind_stack (§7.1)
    behind         INTEGER NOT NULL DEFAULT 0,    -- default-branch commits the remote land target does not hold
    default_branch TEXT NOT NULL,                 -- the branch the target is kept current with
    target_branch  TEXT NOT NULL,                 -- the land target the sync merges onto
    default_head   TEXT NOT NULL DEFAULT '',      -- the default head the last sync read
    target_head    TEXT NOT NULL DEFAULT '',      -- the target head the last sync read
    merge_commit   TEXT NOT NULL DEFAULT '',      -- the merge commit a waiting push records
    pushed         TEXT NOT NULL DEFAULT '',      -- a recorded merge's push state: pushed, waiting or local
    push_cmd       TEXT NOT NULL DEFAULT '',      -- gate_push never's exact hand push of the merge
    conflict_files TEXT NOT NULL DEFAULT '',      -- JSON array of the paths a conflicting merge names
    updated_at     TEXT NOT NULL,
    CHECK (state IN ('current', 'waiting', 'conflict', 'behind_stack'))
);
