-- 0003_idempotency: the stored first responses behind the API's
-- Idempotency-Key (design §12.1). The callers are often LLMs whose tool
-- calls may repeat, so a mutating endpoint with an Idempotency-Key
-- replays the first response for a repeat of the same body, and refuses
-- the same key with a different body. Rows expire after 24 h and are
-- swept on access.
--
-- Timestamps follow 0001's convention: fixed-width UTC text, so the
-- expiry sweep compares lexicographically.

-- One stored response per (caller, key). The caller is the
-- authenticated token's name, so the same key from two callers cannot
-- collide.
CREATE TABLE idempotency (
    caller      TEXT NOT NULL,
    key         TEXT NOT NULL,
    request_sha TEXT NOT NULL, -- sha256 hex of the request body: the same key with a different body is a conflict
    status_code INTEGER NOT NULL CHECK (status_code >= 100 AND status_code < 600),
    response    TEXT NOT NULL, -- the first response's body, replayed verbatim
    created_at  TEXT NOT NULL,
    PRIMARY KEY (caller, key)
);

-- The expiry sweep scans by age.
CREATE INDEX idx_idempotency_created ON idempotency(created_at);
