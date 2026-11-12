-- 0075_approval_requests: the pending requests of the approval authority
-- (§12.2). One row per challenge the engine issues for one named act —
-- approving a plan, releasing a landing or a push, importing a project's
-- settings — that only the user's enrolled approval key can answer. The
-- row holds the exact challenge text the key signs (challenge), the act
-- it is bound to (kind and subject), its nonce and its short expiry. The
-- gates (gate_plan, gate_land, gate_push, approval_tiers) still decide
-- whether an act waits for the user; this row is the authority that
-- releases it, and no token of any role stands in for it.
--
-- state is `pending` until a signature over challenge by an enrolled key
-- answers it (`approved`, key_fingerprint the key's SHA256 fingerprint,
-- signature_sha256 the digest of the armored signature so the same
-- signature cannot answer a second request), or until expires_at passes
-- unanswered (`expired`). A row is never reopened: a refused or expired
-- act asks for a fresh challenge. The enrolled keys themselves live in
-- the state directory's approval-keys file, not here.

CREATE TABLE approval_requests (
    id               INTEGER PRIMARY KEY,
    kind             TEXT NOT NULL CHECK (kind IN ('plan', 'land', 'push', 'import')), -- the act's kind
    subject          TEXT NOT NULL CHECK (subject <> ''),  -- the act's identity: the plan, task, push or import it names
    nonce            TEXT NOT NULL UNIQUE,                 -- random, hex; makes every challenge one of a kind
    challenge        TEXT NOT NULL,                        -- the exact text the approval key signs
    created_at       TEXT NOT NULL,
    expires_at       TEXT NOT NULL,                        -- after this the challenge answers nothing
    state            TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'approved', 'expired')),
    key_fingerprint  TEXT,                                 -- the enrolled key that approved; set with state approved
    signature_sha256 TEXT UNIQUE,                          -- the approving signature's digest; one signature, one act
    decided_at       TEXT,                                 -- when the request left pending
    CHECK ((state = 'approved') = (key_fingerprint IS NOT NULL AND signature_sha256 IS NOT NULL)),
    CHECK ((state = 'pending') = (decided_at IS NULL))
);

CREATE INDEX idx_approval_requests_act ON approval_requests(kind, subject, state);
