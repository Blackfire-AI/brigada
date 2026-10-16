-- 0030_job_model: the model a foreman turn is placed on.
--
-- A foreman turn runs on the harness AND model its project chose when the
-- turn was queued; jobs.model records the model beside jobs.harness. '' on
-- every other kind of job, and on a foreman turn submitted without one
-- (the admin job submit), which then follows the project's own choice.
ALTER TABLE jobs ADD COLUMN model TEXT NOT NULL DEFAULT '';

-- The foreman's session generation: projects.foreman_session_gen moves
-- every time a harness change drops the session, and a foreman turn's
-- lease stamps the generation it ran under on jobs.session_gen. A turn
-- that completes under an older generation leaves no session id behind,
-- so a completion from a pair the project has since left (even one it has
-- since returned to) cannot make the new foreman resume the old CLI's
-- session.
ALTER TABLE projects ADD COLUMN foreman_session_gen INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN session_gen INTEGER NOT NULL DEFAULT 0;

-- Turns that are live at the upgrade get the model they can be recovered
-- to: the project's chosen model, when the turn's harness is the project's
-- own harness (or the turn names none). A turn queued on another harness
-- than the project's current one has no recoverable model; it keeps '' and
-- is held by the engine, never run on whatever model a runner offers.
UPDATE jobs SET model = COALESCE((SELECT p.foreman_model FROM projects p WHERE p.id = jobs.project_id), '')
WHERE kind = 'foreman' AND status IN ('queued', 'leased', 'running') AND model = ''
  AND (harness = '' OR harness = (SELECT p.foreman_harness FROM projects p WHERE p.id = jobs.project_id));
