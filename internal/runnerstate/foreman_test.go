package runnerstate_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

func TestSavedForeman(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runner")
	if got, err := runnerstate.LoadForeman(dir); got != "" || err != nil {
		t.Fatalf("missing foreman: %q, %v", got, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("reading the foreman created directory: %v", err)
	}
	for _, want := range []string{"claude/sonnet@high", "codex/default"} {
		if err := runnerstate.SaveForeman(dir, want); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, "foreman"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("saved mode: %v, %v", info, err)
		}
		if got, err := runnerstate.LoadForeman(dir); got != want || err != nil {
			t.Fatalf("loaded foreman: %q, %v; want %q", got, err, want)
		}
	}
}
