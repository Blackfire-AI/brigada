-- 0065_task_skills: the project's task skills and a task's snapshot of
-- what its prompts carry (design §5.9 'Bounds', 'Versioning'; §5.10
-- 'Import', 'Resolved once').
--
-- skills holds the last import of the project's skills directory: one
-- row per SKILL.md, its text as the client read it, the text's digest
-- and the tree digest of the skill's directory. The engine stores text,
-- never a path. for_role is the frontmatter's 'for:' (both by default).
CREATE TABLE skills (
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    for_role    TEXT NOT NULL DEFAULT 'both' CHECK (for_role IN ('worker', 'reviewer', 'both')),
    text        TEXT NOT NULL,
    digest      TEXT NOT NULL,               -- sha256 of text
    tree_digest TEXT NOT NULL DEFAULT '',    -- the client's digest of the skill's directory
    PRIMARY KEY (project_id, name)
);

-- A task's snapshot, resolved once at creation. The worker snapshot is
-- the existing tasks.instructions and tasks.instructions_meta (0035),
-- kept as they are, so every task recorded before this migration keeps
-- the guidance it was first given. The reviewer snapshot and the
-- ordered skills are new:
--   reviewer_instructions       the reviewer role text the task's
--                               reviews carry ('' = none);
--   reviewer_instructions_meta  its JSON provenance {"source", "version",
--                               "digest", "bytes"};
--   skills                      a JSON array, in the order given, of
--                               {"name", "for", "text", "digest",
--                               "tree_digest"} ('' = no skills).
ALTER TABLE tasks ADD COLUMN reviewer_instructions TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN reviewer_instructions_meta TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN skills TEXT NOT NULL DEFAULT '';

-- A task created before this migration gets an explicitly empty reviewer
-- snapshot: its reviews already show the worker text once, as the
-- convention the author followed, and a copy here would show it twice.
-- The marker tells it apart from a task that was never resolved.
UPDATE tasks SET reviewer_instructions_meta = '{"source":"legacy","bytes":0}';
