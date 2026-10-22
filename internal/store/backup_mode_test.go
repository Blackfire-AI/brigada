package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestBackupAndRestoreFilesAreOwnerOnly: a copy of the store holds all of
// it, so Backup and Restore leave their files readable by the owner alone,
// whatever the umask.
func TestBackupAndRestoreFilesAreOwnerOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	snap := filepath.Join(dir, "snap.db")
	if err := s.Backup(ctx, snap); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.db")
	if err := Restore(ctx, snap, restored); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{snap, restored} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s has mode %04o, want 0600", filepath.Base(p), mode)
		}
	}
}
