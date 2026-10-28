-- 0064_role_instructions: the per-role instruction texts (design §5.9).
-- One row per project and role (foreman, plan_review, worker,
-- reviewer): the text, its source, the settings version it was resolved
-- at ('' for a local value, the import hash for an imported one) and
-- its SHA-256 digest. The Go edit after this SQL seeds every project:
-- a role with no text takes the embedded default, provenance
-- 'generated from <version>, by upgrade'; a project whose instructions
-- setting holds a text keeps it as its worker text and gains the other
-- three. A project created after the migration writes no rows: a role
-- with no row reads as not configured and the embedded default applies.
CREATE TABLE role_instructions (
    project_id        INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    role              TEXT NOT NULL CHECK (role IN ('foreman', 'plan_review', 'worker', 'reviewer')),
    text              TEXT NOT NULL,
    source            TEXT NOT NULL,
    settings_version  TEXT NOT NULL DEFAULT '',
    digest            TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    PRIMARY KEY (project_id, role)
);
