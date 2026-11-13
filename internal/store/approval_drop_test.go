package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestOpenDropsApprovalRequests builds the store a binary that stops at
// 0075 leaves behind, with a request row in it, and expects 0077 to drop
// the table and its two indexes.
func TestOpenDropsApprovalRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &n); err != nil || n > 75 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s by hand: %v", e.Name(), err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 75"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO approval_requests (kind, identity, nonce, expires_at, created_at)
		VALUES ('plan', 'approve:project=1:plan=1:v=1:stop=0', '0123456789abcdef0123456789abcdef', '2026-11-13T10:05:00Z', '2026-11-13T10:00:00Z')`); err != nil {
		t.Fatalf("seed a request: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	if got := queryStrings(t, s.db, "SELECT name FROM sqlite_master WHERE name LIKE '%approval_requests%'"); len(got) != 0 {
		t.Errorf("approval_requests objects after upgrade = %v, want none", got)
	}
}
