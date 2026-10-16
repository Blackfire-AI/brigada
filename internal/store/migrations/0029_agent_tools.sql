-- 0029_agent_tools: the tool scope a claude worker agent runs under, as
-- the runner advertised it: the Claude Code patterns joined by '+', or
-- 'none' for file tools only. The foreman's capacity read shows it per
-- agent; '' for a harness whose sandbox brigada does not scope.
ALTER TABLE agents ADD COLUMN tools TEXT NOT NULL DEFAULT '';
