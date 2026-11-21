package engine

import "testing"

// TestParseTaskBranchRef covers the task-branch family's parser
// (§13.2): the exact forms the family names, and the names that must
// never parse — the land target, the plans branch, the default branch,
// and a user's branch.
func TestParseTaskBranchRef(t *testing.T) {
	for _, tc := range []struct {
		ref    string
		id     int64
		suffix string
		ok     bool
	}{
		{"brigada/task-12", 12, "", true},
		{"brigada/task-12-a2", 12, "-a2", true},
		{"brigada/task-12-r3", 12, "-r3", true},
		{"brigada/task-12-salvage-9", 12, "-salvage-9", true},
		{"brigada/task-12-r3-salvage-9", 12, "-r3-salvage-9", true},
		{"brigada/task-12-a2-salvage-40", 12, "-a2-salvage-40", true},
		{"main", 0, "", false},          // the default branch
		{"staging", 0, "", false},       // a land target
		{"brigada/plans", 0, "", false}, // the plans branch
		{"feature-x", 0, "", false},     // a user's branch
		{"brigada/land/main", 0, "", false},
		{"brigada/task-x", 0, "", false},
		{"brigada/task-", 0, "", false},
		{"brigada/task-0", 0, "", false},
		{"brigada/task-12x", 0, "", false},
		{"brigada/task-12-b1", 0, "", false},
		{"brigada/task-12-r", 0, "", false},
		{"brigada/task-12-rx", 0, "", false},
		{"brigada/task-12-salvage", 0, "", false},
		{"task-12", 0, "", false},
		{"", 0, "", false},
	} {
		id, suffix, ok := ParseTaskBranchRef(tc.ref)
		if id != tc.id || suffix != tc.suffix || ok != tc.ok {
			t.Errorf("ParseTaskBranchRef(%q) = %d, %q, %t; want %d, %q, %t",
				tc.ref, id, suffix, ok, tc.id, tc.suffix, tc.ok)
		}
	}
	// A task id that merely shares a numeric prefix parses as its own
	// task: pruning task 1 must never match task 12's refs, nor the
	// reverse.
	if id, _, ok := ParseTaskBranchRef("brigada/task-12"); !ok || id != 12 {
		t.Errorf("ParseTaskBranchRef(brigada/task-12) = %d, %t; want 12, true", id, ok)
	}
	if id, _, ok := ParseTaskBranchRef("brigada/task-1-r2"); !ok || id == 12 {
		t.Errorf("ParseTaskBranchRef(brigada/task-1-r2) = %d, %t; want 1, true", id, ok)
	}
}
