-- 0068_job_session_cost: jobs.cost_usd is the cost of that job alone.
-- The Claude CLI's total_cost_usd is the running total of a resumed
-- session, so the figure the adapter reports now lives in
-- session_cost_usd and cost_usd holds the delta from the session's
-- previous job (issue #125). Jobs of adapters whose cost is per
-- invocation never set it.
ALTER TABLE jobs ADD COLUMN session_cost_usd REAL;

-- Historical rows: every Claude job that recorded a cost stored the raw
-- total. Keep it as session_cost_usd, then charge each job the difference
-- from the latest earlier job (by id) of the same result session_id that
-- has a total, never below 0. A first job of a session keeps its value.
-- Jobs of other adapters, and jobs whose result carries no session_id
-- (failed runs), are left as they are.
UPDATE jobs SET session_cost_usd = cost_usd
 WHERE cost_usd IS NOT NULL
   AND json_extract(result, '$.session_id') IS NOT NULL
   AND agent_id IN (SELECT id FROM agents WHERE adapter = 'claude');

UPDATE jobs SET cost_usd = MAX(0, session_cost_usd - COALESCE((
        SELECT p.session_cost_usd FROM jobs p
         WHERE p.id < jobs.id AND p.session_cost_usd IS NOT NULL
           AND json_extract(p.result, '$.session_id') = json_extract(jobs.result, '$.session_id')
         ORDER BY p.id DESC LIMIT 1), 0))
 WHERE session_cost_usd IS NOT NULL;
