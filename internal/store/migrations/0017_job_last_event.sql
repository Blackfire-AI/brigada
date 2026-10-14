-- 0017_job_last_event: the worker's latest progress event (§5.6, §10).
--
-- Each heartbeat carries the run's last few progress events — a tool
-- call, a word of the assistant's — and the engine kept only their
-- times. The newest one is kept here, flattened to one short line, so
-- `brigada status` and the attached foreman's status line can show
-- what a running worker is doing, not just that it is alive. It is a
-- display field: nothing the rules decide reads it.

ALTER TABLE jobs ADD COLUMN last_event TEXT;
