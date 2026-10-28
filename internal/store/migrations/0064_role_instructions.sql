-- 0064_role_instructions: the per-role instruction texts (design §5.9).
-- One row per project and role — foreman, plan_review, worker,
-- reviewer — holding the effective text and its provenance, a JSON
-- {"source": ..., "version": ..., "digest": <sha256 of the text>,
-- "bytes": N}. The Go half of this migration (seedRoleInstructions)
-- seeds every project's rows: the embedded default of each role, except
-- that a project whose instructions setting already holds text keeps it
-- as its worker text and gains the other three roles' defaults, the
-- seeded provenance reading 'generated from <version>, by upgrade'.
CREATE TABLE role_instructions (
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('foreman', 'plan_review', 'worker', 'reviewer')),
    text        TEXT NOT NULL,               -- '' = the role's text is empty by choice
    meta        TEXT NOT NULL DEFAULT '',    -- JSON: source, version, digest, bytes
    PRIMARY KEY (project_id, role)
);
