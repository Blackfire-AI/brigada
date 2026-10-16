package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestSetupFirstRunStoresOnlyForWorkJobs pins §5.7's rule that a worker
// cannot spoil the next job's cache: a fix or review worktree sits on a
// branch a worker wrote, so its successful first run never replaces the
// shared entry.
func TestSetupFirstRunStoresOnlyForWorkJobs(t *testing.T) {
	for kind, wantEntries := range map[string]int{"work": 1, "fix": 0, "review": 0} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "lock"), []byte("v1"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := &Config{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			r := &jobRun{
				cfg:    cfg,
				client: api.NewClient(filepath.Join(t.TempDir(), "none.sock"), "x"),
				grant: &api.JobGrant{
					JobID: 1, Attempt: 1, Kind: kind, RepoURL: "file:///repo.git",
					Setup: &engine.SetupSpec{Command: `: > "$BRIGADA_CACHE_DIR/stamp"`, CacheKeyFiles: []string{"lock"}, TimeoutMinutes: 1},
				},
			}
			if why := r.runSetup(context.Background(), dir, true); why != "" {
				t.Fatalf("setup failed: %s", why)
			}
			entries, _ := os.ReadDir(r.setupRepoDir())
			if len(entries) != wantEntries {
				t.Errorf("%s job left %d cache entries, want %d", kind, len(entries), wantEntries)
			}
		})
	}
}
