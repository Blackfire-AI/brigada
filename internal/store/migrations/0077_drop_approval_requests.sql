-- 0077_drop_approval_requests: approval is the user's decision by
-- convention (§12.2), so nothing issues, signs or reads a request any
-- more. Drops the table 0075 created and its two indexes; the rows were
-- pending challenges and spent signatures, which nothing reads.

DROP INDEX IF EXISTS idx_approval_requests_state;
DROP INDEX IF EXISTS idx_approval_requests_act;
DROP TABLE IF EXISTS approval_requests;
