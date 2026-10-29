-- 0070_task_skills_dir: a task records the project's skills_dir beside
-- its skill snapshot (design §5.10 'Resolved once'), so the directory
-- line of its prompts is the one the author was first given, whatever a
-- later import sets. Every task that exists now gets its value written
-- once: its project's skills_dir as it stands at this migration, or
-- '.brigada/skills' when the project sets none. The engine reads only
-- the recorded value afterwards.
ALTER TABLE tasks ADD COLUMN skills_dir TEXT NOT NULL DEFAULT '';

UPDATE tasks SET skills_dir = COALESCE((
    SELECT CASE WHEN json_valid(p.settings) AND json_type(p.settings, '$.skills_dir') = 'text'
                THEN NULLIF(json_extract(p.settings, '$.skills_dir'), '') END
    FROM plans pl JOIN projects p ON p.id = pl.project_id
    WHERE pl.id = tasks.plan_id), '.brigada/skills');
