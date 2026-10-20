-- 0049_retire_mode_a: mode A is retired (decided 2026-10-20). The user's
-- own harness session no longer holds the foreman's slot: `brigada
-- attach` joins the brigada-owned session instead, and its hold
-- (attach_holds, hold_tokens) stays. This drops what 0033 and 0034 added
-- for mode A: the interactive holds (the credential was a column of
-- theirs, credential_hash, never a table of its own), the project's hold
-- fence, the decision-note cursor and the index that served it. The
-- decision notes themselves were journal messages; they stay in the
-- history for the record, like every other message.

DROP INDEX IF EXISTS idx_interactive_holds_live;
DROP INDEX IF EXISTS idx_interactive_holds_state;
DROP TABLE IF EXISTS interactive_holds;
DROP INDEX IF EXISTS idx_messages_decision_notes;
ALTER TABLE projects DROP COLUMN hold_fence;
ALTER TABLE projects DROP COLUMN decision_note_cursor;

-- The settings keys of mode A (hold_idle_minutes, hold_max_minutes,
-- interactive_quiet_minutes) are gone from the local store too.
UPDATE projects
SET settings = json_remove(settings, '$.hold_idle_minutes', '$.hold_max_minutes', '$.interactive_quiet_minutes')
WHERE settings != '' AND json_valid(settings);
