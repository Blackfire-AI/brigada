-- 0054_agent_probe: the broken-agent probe's schedule (§8 "Health").
--
-- An agent broken by repeated failures comes back by itself: a few
-- minutes after it was marked broken, the runner runs a cheap probe of
-- its CLI, and a passing probe clears the broken state. probe_at is when
-- the next probe is due (empty = none scheduled); probe_delay is the
-- delay that scheduled probe was given, in whole seconds, doubled on
-- each failing probe and capped — the growing, capped back-off. An agent
-- broken by an auth failure is never probed: probe_at stays empty and
-- it waits for the user's reset.

ALTER TABLE agents ADD COLUMN probe_at TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN probe_delay INTEGER NOT NULL DEFAULT 0;
