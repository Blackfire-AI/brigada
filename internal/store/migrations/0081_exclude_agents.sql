-- 0081_exclude_agents: the retry rule's exclusion becomes a set (§7).
-- exclude_agents holds a JSON array of agent ids — the agents excluded
-- for the job's current logical attempt, the ones that already failed
-- it, carried forward and appended on each retry and effective for the
-- whole attempt: an exclusion never lifts while the attempt lasts. It
-- replaces 0011's single exclude_agent column, whose one in-flight
-- exclusion backfills as a one-element array; '[]' (the default)
-- excludes nothing. require_agent (0011) stays as it is: the fix
-- round's pin (§3) names one agent, not a set.
ALTER TABLE jobs ADD COLUMN exclude_agents TEXT NOT NULL DEFAULT '[]';
UPDATE jobs SET exclude_agents = json_array(exclude_agent) WHERE exclude_agent IS NOT NULL AND exclude_agent <> 0;
ALTER TABLE jobs DROP COLUMN exclude_agent;
