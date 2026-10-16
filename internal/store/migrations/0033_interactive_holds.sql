-- 0033_interactive_holds: the mode A hold (§4.6) — the user's own harness
-- session holding the foreman's live slot. One row per hold, kept after it
-- ends for the record. state is 'live' while the hold stands and 'ended'
-- after yield, expiry, a missed lease or a forced release; end_reason says
-- which. The credential is never stored: credential_hash is its sha256,
-- and the row outlives the hold so that a revoked credential is
-- recognised, and answered stale_hold, instead of looking unknown.
--
-- Two clocks (§4.6): lease_expires_at is liveness, moved by the bridge's
-- heartbeat; idle_expires_at is activity, moved only by a call that
-- changes or consumes engine state, and max_at is the absolute bound from
-- acquire. Neither activity bound is moved by a heartbeat.
--
-- The harness session fields are separate on purpose: harness_session_id,
-- its source and observed time are filled only from a documented harness
-- channel; session_hint is an id first seen in a tool call, for display
-- and diagnostics, never proof of ownership.

CREATE TABLE interactive_holds (
    id                          INTEGER PRIMARY KEY,
    project_id                  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    holder_kind                 TEXT NOT NULL DEFAULT 'interactive',
    bridge_instance             TEXT NOT NULL,
    label                       TEXT NOT NULL DEFAULT '',
    credential_hash             TEXT NOT NULL UNIQUE,
    fence                       INTEGER NOT NULL,
    state                       TEXT NOT NULL CHECK (state IN ('live', 'ended')),
    end_reason                  TEXT,
    lease_expires_at            TEXT NOT NULL,
    activity_at                 TEXT NOT NULL,
    idle_expires_at             TEXT NOT NULL,
    max_at                      TEXT NOT NULL,
    harness_session_id          TEXT,
    harness_session_source      TEXT,
    harness_session_observed_at TEXT,
    session_hint                TEXT,
    created_at                  TEXT NOT NULL,
    ended_at                    TEXT
);

-- At most one live hold per project: the slot has one holder.
CREATE UNIQUE INDEX idx_interactive_holds_live ON interactive_holds(project_id) WHERE state = 'live';

-- The sweep's expiry pass.
CREATE INDEX idx_interactive_holds_state ON interactive_holds(state, lease_expires_at);

-- The project's monotonic hold fence (§4.6): bumped at every acquire and
-- at every hold end, so any credential or fence older than the latest
-- is dead.
ALTER TABLE projects ADD COLUMN hold_fence INTEGER NOT NULL DEFAULT 0;
