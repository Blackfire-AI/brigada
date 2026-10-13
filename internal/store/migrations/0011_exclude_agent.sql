-- 0011_exclude_agent: the agent rules a job's placement can carry (§7,
-- §8), each NULL (the default) when it does not apply. exclude_agent is
-- the reviewer retry's rule: a review job that retries after an
-- unusable verdict — no verdict, or an approve without substantive
-- findings — excludes the first reviewer's agent on top of the author's
-- family, so the retry goes to another agent of a non-author family
-- instead of the same agent reading its own output again. require_agent
-- is the fix round's rule (§3): the round resumes the author's session
-- on the author's own agent, and the harness alone does not name it —
-- another runner may offer the same harness, and the author's runner
-- may hold a second agent of it — so the column pins the one agent that
-- may run the job.
ALTER TABLE jobs ADD COLUMN exclude_agent INTEGER REFERENCES agents(id) ON DELETE SET NULL;
ALTER TABLE jobs ADD COLUMN require_agent INTEGER REFERENCES agents(id) ON DELETE SET NULL;
