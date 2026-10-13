-- 0008_lease_tokens: the lease-scoped foreman tokens (§12.1, §12.2).
-- When a lease grants a foreman turn, the engine mints a short-lived
-- foreman token scoped to the turn's project: the runner holds the
-- secret in memory alone — a runner must never hold a long-lived
-- foreman token, for it may become remote — and spends it through the
-- MCP bridge. Only the sha256 hex of the secret is stored, never the
-- secret; the row dies with the lease it was minted for (complete,
-- release, cancel, expiry, a hard cap, a fence bump), and
-- authentication checks the expiry itself, so a token never outlives
-- the turn even by the sweep's laziness.
--
-- This table is the engine's alone: the token file's sync (syncTokens)
-- rewrites the tokens table and never touches this one, so a restarted
-- engine cannot revoke a live turn's token, and a minted token cannot
-- leak into the file's long-lived set.

CREATE TABLE lease_tokens (
    id          INTEGER PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE, -- sha256 hex of the secret; the secret itself is stored nowhere
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE, -- the token's scope: the turn's project (§12.1)
    job_id      INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,     -- the foreman turn the token was minted for
    lease_id    TEXT NOT NULL,                                               -- the lease that minted it
    expires_at  TEXT NOT NULL,                                               -- tracks the lease's expiry: a heartbeat extends both
    created_at  TEXT NOT NULL
);

CREATE INDEX idx_lease_tokens_job ON lease_tokens(job_id);
CREATE INDEX idx_lease_tokens_expiry ON lease_tokens(expires_at);
