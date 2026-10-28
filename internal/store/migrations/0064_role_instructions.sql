-- 0064_role_instructions: one instruction text per role (design §5.9):
-- foreman, plan_review, worker and reviewer. text is the effective text
-- ('' = empty by choice: the row exists and suppresses the default);
-- source is 'generated' (seeded from the embedded defaults, with
-- settings_version the defaults version the text was generated from),
-- 'local' (set by the user) or 'imported' (set by a project.yaml
-- import, with settings_version the import hash). digest is the SHA-256
-- of the text. A missing row is not configured: the embedded default
-- applies. The seed itself runs in Go, beside this file, reading the
-- embedded defaults and each project's instructions setting.

CREATE TABLE role_instructions (
    project_id       INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    role             TEXT NOT NULL CHECK (role IN ('foreman', 'plan_review', 'worker', 'reviewer')),
    text             TEXT NOT NULL DEFAULT '',
    source           TEXT NOT NULL CHECK (source IN ('generated', 'local', 'imported')),
    settings_version TEXT NOT NULL DEFAULT '',
    digest           TEXT NOT NULL DEFAULT '',
    updated_at       TEXT NOT NULL,
    PRIMARY KEY (project_id, role)
);
