-- 0026_runner_harnesses: the harnesses a runner offered on its latest
-- lease poll (§8), a JSON array. A poll may narrow the offer below what
-- the runner registered, and the queue TTL's busy-runner rule (§8.1) must
-- judge a job's candidates by what is actually offered; '' means the
-- runner has not polled since this column existed, its offer unknown.
ALTER TABLE runners ADD COLUMN harnesses TEXT NOT NULL DEFAULT '';
