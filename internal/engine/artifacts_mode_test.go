package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestArtifactBlobIsOwnerOnly: a transcript can hold anything a worker
// printed, so the blob is readable by its owner alone, in directories
// only its owner enters.
func TestArtifactBlobIsOwnerOnly(t *testing.T) {
	e, dataDir := newArtifactEnv(t, nil)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	_, info := e.upload(g.LeaseOwner, g.JobID, g.LeaseID, ArtifactTranscript, []byte("a transcript line\n"))
	st, err := os.Stat(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("blob has mode %04o, want 0600", mode)
	}
	for dir := filepath.Dir(info.Path); dir != dataDir; dir = filepath.Dir(dir) {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if mode := st.Mode().Perm(); mode != 0o700 {
			t.Errorf("%s has mode %04o, want 0700", dir, mode)
		}
	}
}
