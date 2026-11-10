-- 0073_stale_land_refs: the engine's record that a target's waiting
-- ref is stale (issue #205). A restart forgets the landing of the
-- restarted task when it waited at the push gate, or sat local under
-- gate_push never — the land rule must queue the new head's landing —
-- and with it every other waiting or local landing of the same target,
-- whose commits ride above the forgotten one on the same
-- refs/brigada/land/<target>. The ref is stale as a whole from that
-- moment; one row per project and target says the next land job of
-- that target deletes the ref before it stacks. A landing of the
-- target, or a runner's report that it dropped the ref itself, clears
-- the row.

CREATE TABLE stale_land_refs (
    project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    target_branch TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (project_id, target_branch)
);
