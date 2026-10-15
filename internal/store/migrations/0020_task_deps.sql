-- 0020_task_deps: plans grow dependencies (§6.1, §11).
--
-- The edge table of 0001 was never written to; it is replaced by one that
-- also names the plan an edge belongs to, so a plan's whole graph reads
-- in one query. task_id depends on depends_on_task_id. As in the schema
-- header, the edges carry no ON DELETE action: a task something depends
-- on cannot be deleted from under it. The CHECK refuses self-edges and the
-- primary key duplicates; acyclicity is the engine's to check.
--
-- A task's name is the short handle its plan's author gives it so other
-- tasks can name it as a dependency; '' = unnamed. Names are unique per
-- plan only when set.

DROP TABLE task_deps;

CREATE TABLE task_deps (
    plan_id            INTEGER NOT NULL REFERENCES plans(id),
    task_id            INTEGER NOT NULL REFERENCES tasks(id),
    depends_on_task_id INTEGER NOT NULL REFERENCES tasks(id),
    PRIMARY KEY (task_id, depends_on_task_id),
    CHECK (task_id <> depends_on_task_id)
);

CREATE INDEX task_deps_by_dependency ON task_deps (depends_on_task_id);

ALTER TABLE tasks ADD COLUMN name TEXT NOT NULL DEFAULT '';
