-- 0021_plan_mirror: which plans the plan-file mirror owes a push (§6.4).
--
-- The engine mirrors each plan as .brigada/plans/<plan-id>.yaml on a git
-- branch of the project's remote. The push is best-effort and outside
-- the store transaction, so the store itself remembers what the mirror
-- has not caught up with: a plan listed here changed since its file was
-- last pushed. The triggers below mark a plan on every write to the plan,
-- its tasks or its edges, so no engine path has to remember to; the
-- mirror clears a mark once the file is pushed.
--
-- seq counts the changes marked since the row appeared. The mirror
-- clears a row only if seq is still the value it rendered at, so a change
-- that lands while the push runs keeps its mark.

CREATE TABLE plan_mirror_dirty (
    plan_id  INTEGER PRIMARY KEY,
    seq      INTEGER NOT NULL DEFAULT 1
);

CREATE TRIGGER plan_mirror_plan_insert AFTER INSERT ON plans
BEGIN
    INSERT INTO plan_mirror_dirty (plan_id) VALUES (NEW.id)
        ON CONFLICT (plan_id) DO UPDATE SET seq = seq + 1;
END;

CREATE TRIGGER plan_mirror_plan_update AFTER UPDATE ON plans
BEGIN
    INSERT INTO plan_mirror_dirty (plan_id) VALUES (NEW.id)
        ON CONFLICT (plan_id) DO UPDATE SET seq = seq + 1;
END;

CREATE TRIGGER plan_mirror_task_insert AFTER INSERT ON tasks
BEGIN
    INSERT INTO plan_mirror_dirty (plan_id) VALUES (NEW.plan_id)
        ON CONFLICT (plan_id) DO UPDATE SET seq = seq + 1;
END;

CREATE TRIGGER plan_mirror_task_update AFTER UPDATE ON tasks
BEGIN
    INSERT INTO plan_mirror_dirty (plan_id) VALUES (NEW.plan_id)
        ON CONFLICT (plan_id) DO UPDATE SET seq = seq + 1;
END;

CREATE TRIGGER plan_mirror_dep_insert AFTER INSERT ON task_deps
BEGIN
    INSERT INTO plan_mirror_dirty (plan_id) VALUES (NEW.plan_id)
        ON CONFLICT (plan_id) DO UPDATE SET seq = seq + 1;
END;
