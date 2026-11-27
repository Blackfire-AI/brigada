-- 0082_class_cooldown: the class-scoped reliability cooldown (§8).
-- Repeated contract failures on one class — a review's stall, missing or
-- malformed verdict, or an approve without substantive findings — cool
-- the agent on that class alone for a short window that backs off, and a
-- valid completion on the class resets it. agents.cooling_until stays
-- the account-quota clock and outcomes keeps only attempts/successes, so
-- the state this needs lives here: per agent and class, the consecutive
-- failure streak, the cooldown expiry (UTC text, '' = none) and the
-- backoff step the window doubles from. A new table: no earlier state to
-- backfill, and an agent with no row has never failed a contract.
CREATE TABLE class_cooldown (
    agent_id      INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    class         TEXT NOT NULL,
    streak        INTEGER NOT NULL DEFAULT 0 CHECK (streak >= 0),
    cooling_until TEXT NOT NULL DEFAULT '',
    step          INTEGER NOT NULL DEFAULT 0 CHECK (step >= 0),
    updated_at    TEXT NOT NULL,
    PRIMARY KEY (agent_id, class)
);
