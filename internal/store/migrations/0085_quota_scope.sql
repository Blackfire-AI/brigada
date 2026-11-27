-- 0085_quota_scope: the quota scope an agent cools under (§8). Cooling
-- groups by quota_scope, not account: the scope is the pool of agents
-- that cool together when one of them hits a quota limit. For opencode
-- the scope is "opencode/" plus the model's first path segment — the
-- provider — so opencode/zai-coding-plan, opencode/opencode and
-- opencode/deepseek cool apart where one opencode harness-wide account
-- did before; every other harness keeps its account label as its scope,
-- so single-provider behaviour is unchanged. An empty scope falls back
-- to the account: an old runner's re-register keeps the stored scope,
-- and an old runner's brand-new agent has none yet.
--
-- The backfill derives the same rule the runner derives when it
-- advertises (cmd/brigada/cmdrunner.go), so a re-register never flips
-- an agent's cooling group. Inherited cooling resets here too: an
-- opencode agent whose new scope splits the old account-wide pool —
-- more than one distinct new scope among the agents that shared its
-- account — had its cooling_until stamped on the whole pool, so it
-- clears. A single-provider opencode roster keeps its cooling, and so
-- does every non-opencode harness.
ALTER TABLE agents ADD COLUMN quota_scope TEXT NOT NULL DEFAULT '';

UPDATE agents SET quota_scope = CASE
    WHEN harness = 'opencode' THEN 'opencode/' || CASE
        WHEN instr(model, '/') > 0 THEN substr(model, 1, instr(model, '/') - 1)
        ELSE model
    END
    ELSE account
END;

UPDATE agents SET cooling_until = NULL
WHERE harness = 'opencode'
  AND account IN (
    SELECT account FROM agents
    GROUP BY account
    HAVING COUNT(DISTINCT quota_scope) > 1
  );
