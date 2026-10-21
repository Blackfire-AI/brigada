-- 0051_push_gate: the push gate's landing record (§6.3, §7.1). The
-- landings row gains the exact push command for gate_push never — printed
-- for the user, never run — and the after_land failure: reported in the
-- record and the history, never undone, and never a change to the task's
-- state. The push phase reuses the land job's kind and key (§7.1), so the
-- jobs table is unchanged. A land_holds row also gains a state: 'held'
-- while the user's release is awaited, 'rejected' once the user cancelled
-- the landing — the row stays, so the land rule never re-holds a landing
-- the user refused, and the branch is left alone.

ALTER TABLE landings ADD COLUMN push_cmd TEXT NOT NULL DEFAULT '';
ALTER TABLE landings ADD COLUMN after_land_error TEXT NOT NULL DEFAULT '';
ALTER TABLE land_holds ADD COLUMN state TEXT NOT NULL DEFAULT 'held' CHECK (state IN ('held', 'rejected'));
