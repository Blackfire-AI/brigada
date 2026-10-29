-- 0067_outcomes_skills: the outcome record splits by how many skills the
-- task carried (design §5.10 'Recorded', §8), so a one-skill and a
-- two-skill task are not pooled. skills joins the key; the dispatcher
-- still ranks on the sum over it. Every row recorded before this
-- migration is a task with no skills.
CREATE TABLE outcomes_new (
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    agent_id    INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    class       TEXT NOT NULL, -- tasks' open class set (§6.1)
    skills      INTEGER NOT NULL DEFAULT 0 CHECK (skills >= 0),
    attempts    INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    successes   INTEGER NOT NULL DEFAULT 0 CHECK (successes >= 0),
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (project_id, agent_id, class, skills)
);
INSERT INTO outcomes_new (project_id, agent_id, class, skills, attempts, successes, updated_at)
    SELECT project_id, agent_id, class, 0, attempts, successes, updated_at FROM outcomes;
DROP TABLE outcomes;
ALTER TABLE outcomes_new RENAME TO outcomes;
