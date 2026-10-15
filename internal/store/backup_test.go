package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestBackupRestoreRoundTrip snapshots a store that is being written to,
// restores the snapshot into a fresh file, and opens it: the copy is a
// whole database that migrates and holds what the snapshot caught.
func TestBackupRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	insert := func(i int) error {
		return s.Update(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
				VALUES (?, 'main', '{}', 'claude', 'sonnet', 'x', 'x')`, fmt.Sprintf("https://example.invalid/%d.git", i))
			return err
		})
	}
	for i := 0; i < 50; i++ {
		if err := insert(i); err != nil {
			t.Fatal(err)
		}
	}

	// A writer keeps going while the snapshot is taken.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 50; i < 100; i++ {
			if err := insert(i); err != nil {
				t.Errorf("insert during the backup: %v", err)
				return
			}
		}
	}()
	snap := filepath.Join(dir, "snap.db")
	if err := s.Backup(ctx, snap); err != nil {
		t.Fatalf("backup: %v", err)
	}
	wg.Wait()

	if err := s.Backup(ctx, snap); err == nil {
		t.Error("a backup over an existing file succeeded")
	}

	restored := filepath.Join(dir, "restored.db")
	if err := Restore(ctx, snap, restored); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := Restore(ctx, snap, restored); err == nil {
		t.Error("a restore over an existing file succeeded")
	}
	r, err := Open(ctx, restored)
	if err != nil {
		t.Fatalf("open the restored store: %v", err)
	}
	defer r.Close()
	var n int
	if err := r.View(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM projects").Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n < 50 || n > 100 {
		t.Errorf("restored projects = %d, want between the 50 before and the 100 after the backup began", n)
	}
	if v, err := r.userVersion(ctx); err != nil || v != schemaVersion(t) {
		t.Errorf("restored user_version = %d, %v; want %d", v, err, schemaVersion(t))
	}
}

func TestRestoreMissingSnapshot(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "store.db")
	if err := Restore(context.Background(), filepath.Join(dir, "none.db"), dst); err == nil {
		t.Fatal("restore of a missing snapshot succeeded")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a failed restore left a store file behind")
	}
}
