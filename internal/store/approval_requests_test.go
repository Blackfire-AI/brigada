package store

import "testing"

// TestApprovalRequestsMigrationKeepsOldData: a store built from the
// migrations before 0075 opens with the approval_requests table empty
// and its existing rows untouched, and the table's checks hold.
func TestApprovalRequestsMigrationKeepsOldData(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if migs[74].version != 75 || migs[74].name != "approval_requests" {
		t.Fatal("the approval_requests table must be migration 0075")
	}
	path, db := openBeforeMigration(t, "approval_requests")
	project := insertProject(t, db)
	plan := insertPlan(t, db, project)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	if n := queryInt(t, s.db, "SELECT COUNT(*) FROM plans WHERE id = ?", plan); n != 1 {
		t.Fatalf("plans after the upgrade = %d, want the old plan kept", n)
	}
	if n := queryInt(t, s.db, "SELECT COUNT(*) FROM approval_requests"); n != 0 {
		t.Fatalf("approval_requests after the upgrade = %d, want none", n)
	}

	insert := `INSERT INTO approval_requests (kind, subject, nonce, challenge, created_at, expires_at, state, key_fingerprint, signature_sha256, decided_at)
		VALUES (?, ?, ?, 'text', ?, ?, ?, ?, ?, ?)`
	exec(t, s.db, insert, "plan", "plan 1", "n1", ts, ts, "pending", nil, nil, nil)
	exec(t, s.db, insert, "push", "push 1", "n2", ts, ts, "approved", "SHA256:x", "d1", ts)
	exec(t, s.db, insert, "land", "task 1", "n3", ts, ts, "expired", nil, nil, ts)
	for _, bad := range []struct {
		name string
		args []any
	}{
		{"unknown kind", []any{"merge", "x", "b1", ts, ts, "pending", nil, nil, nil}},
		{"empty subject", []any{"plan", "", "b2", ts, ts, "pending", nil, nil, nil}},
		{"unknown state", []any{"plan", "x", "b3", ts, ts, "refused", nil, nil, ts}},
		{"approved without a key", []any{"plan", "x", "b4", ts, ts, "approved", nil, "d2", ts}},
		{"pending with a key", []any{"plan", "x", "b5", ts, ts, "pending", "SHA256:x", "d3", nil}},
		{"decided while pending", []any{"plan", "x", "b6", ts, ts, "pending", nil, nil, ts}},
		{"reused nonce", []any{"plan", "x", "n1", ts, ts, "pending", nil, nil, nil}},
		{"reused signature", []any{"plan", "x", "b7", ts, ts, "approved", "SHA256:x", "d1", ts}},
	} {
		if _, err := s.db.Exec(insert, bad.args...); err == nil {
			t.Errorf("%s: insert succeeded, want a check to refuse it", bad.name)
		}
	}
}
