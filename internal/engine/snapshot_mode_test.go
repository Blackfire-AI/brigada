package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshotIsOwnerOnly: a snapshot holds the whole store, so the file
// that lands in the snapshot directory is readable by its owner alone and
// no temp file is left beside it.
func TestSnapshotIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snaps")
	e := newEnvCfg(t, func(c *Config) { c.SnapshotDir = dir })

	path, err := e.eng.SnapshotIfDue(e.ctx)
	if err != nil || path == "" {
		t.Fatalf("SnapshotIfDue = %q, %v; want a snapshot", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("snapshot has mode %04o, want 0600", mode)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}
