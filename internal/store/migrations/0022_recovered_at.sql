-- 0022_recovered_at: when `brigada recover` last rebuilt the store (§6.4).
--
-- The engine holds dispatch for one lease-expiry window after this
-- moment, so the runners whose heartbeats now fail have killed their
-- workers before anything is leased again. NULL = never recovered.
ALTER TABLE meta ADD COLUMN recovered_at TEXT;
