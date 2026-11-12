-- 0075_approval_requests: the user's pending approvals (§12.2). One row
-- per challenge issued for one act — a plan, task, push or import — that
-- the user answers with a signature from an enrolled approval key,
-- beyond any API token (internal/approval). The row is the request's
-- whole storage and the authority's memory of what it already spent, so
-- an engine restart cannot make a signature verify twice: a request
-- leaves `waiting` once, by an UPDATE that only a waiting, unexpired row
-- takes. Times are UTC, RFC 3339 to the second; expires_at is the
-- challenge's own expiry, bound into the signed message. A waiting
-- request past its expiry is settled to `expired` the next time it is
-- read. The table is new: there is no old data to backfill.

CREATE TABLE approval_requests (
    id           INTEGER PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('plan', 'task', 'push', 'import')), -- the act's kind
    identity     TEXT NOT NULL CHECK (identity <> ''),                              -- the plan, task, push or import it names
    nonce        TEXT NOT NULL CHECK (length(nonce) = 32),                          -- the challenge's nonce, 32 hex characters
    expires_at   TEXT NOT NULL,                                                     -- the challenge's expiry
    created_at   TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'waiting'
                 CHECK (state IN ('waiting', 'signed', 'refused', 'expired')),
    signature    TEXT,                                                              -- the armored signature that satisfied it; set exactly when signed
    key_label    TEXT,                                                              -- the label of the enrolled key that signed; set exactly when signed
    decided_at   TEXT,                                                              -- when it left waiting
    CHECK ((state = 'signed') = (signature IS NOT NULL AND key_label IS NOT NULL)),
    CHECK ((state = 'waiting') = (decided_at IS NULL))
);

-- A request is found by its act, and the spent ones are read back at a restart.
CREATE INDEX idx_approval_requests_act ON approval_requests(kind, identity);
CREATE INDEX idx_approval_requests_state ON approval_requests(state, expires_at);
