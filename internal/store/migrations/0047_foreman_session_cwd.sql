-- 0047_foreman_session_cwd: the foreman's harness now runs in the reading
-- checkout (<scratch>/repo) instead of its scratch parent, headless and
-- attached alike (§4, §4.2). A harness keys its sessions by working
-- directory, so every session recorded under a 'foreman:<project>' key was
-- made in the old directory and cannot be resumed from the new one. The
-- ids are cleared once: the next turn or attach starts a fresh session
-- and writes its id back as usual. Plans, tasks and the inbox hold the
-- foreman's state (§1.3); only the conversation starts over.

UPDATE sessions SET harness_session_id = NULL WHERE key LIKE 'foreman:%';
