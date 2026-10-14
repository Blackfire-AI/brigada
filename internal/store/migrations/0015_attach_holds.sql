-- 0015_attach_holds: the attach hold (§4.2 item 1) — the user's own
-- interactive session with the foreman. A hold is lease-like: the client
-- heartbeats it, a second live attach is refused, and it expires if the
-- terminal is killed. state is 'live' while the attached session runs,
-- 'released' by the client's detach, 'expired' by the sweep; the row is
-- kept either way, the events name the hold and the inbox rows it
-- carried keep their reference, as a returned row keeps its job.

CREATE TABLE attach_holds (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    state      TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

-- The hold's two readers: the wake's "is a session attached?" and the
-- sweep's expiry pass.
CREATE INDEX idx_attach_holds_project ON attach_holds(project_id, state, expires_at);

-- The hold's foreman token (§12.1): the same discipline as the
-- lease-scoped token — random secret, hash alone stored, expiry checked
-- at authentication, revoked with the hold — but scoped to the hold, not
-- a turn's lease. A hold has no job, and lease_tokens.job_id is NOT NULL
-- for the turns' own lifecycle (a fence bump, a job's ending, all name a
-- job), so the hold's tokens live in a table of their own rather than by
-- widening that column into NULLs.

CREATE TABLE hold_tokens (
    id         INTEGER PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE, -- sha256 hex of the secret; the secret itself is stored nowhere
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hold_id    INTEGER NOT NULL REFERENCES attach_holds(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,        -- tracks the hold's expiry: a heartbeat extends both
    created_at TEXT NOT NULL
);

CREATE INDEX idx_hold_tokens_expiry ON hold_tokens(expires_at);

-- An inbox row an attach took into its opening prompt reads delivered to
-- the hold: job_id stays NULL, hold_id names what carried it. Neither
-- ending counts an attempt — the client's detach is not a failure to
-- answer — so a hold's rows move between delivered and pending with
-- attempts untouched.
ALTER TABLE foreman_inbox ADD COLUMN hold_id INTEGER REFERENCES attach_holds(id);
