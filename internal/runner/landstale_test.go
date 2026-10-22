package runner

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commitFile makes one commit in dir and returns its hash.
func commitFile(t *testing.T, dir, file string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(file+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@brigada.invalid", "commit", "-qm", file)
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

// staleWaitFixture clones a fresh remote and pushes a waiting ref one
// commit ahead of main; it returns the clone, the remote and the waiting
// commit.
func staleWaitFixture(t *testing.T) (r *jobRun, dir, remote, waiting string) {
	t.Helper()
	remote = newRemote(t)
	dir = t.TempDir()
	git(t, "", "clone", "-q", "--branch", "main", remote, dir)
	git(t, dir, "checkout", "-q", "-b", "wait")
	waiting = commitFile(t, dir, "waiting.txt")
	git(t, dir, "push", "-q", "origin", "HEAD:"+landWaitRef("main"))
	git(t, dir, "checkout", "-q", "--detach", "main")
	r = &jobRun{cfg: &Config{Log: slog.Default()}}
	return r, dir, remote, waiting
}

// TestLandDropStaleWaitingKeepsLiveRef: a waiting ref the target can still
// fast-forward to is left on the remote.
func TestLandDropStaleWaitingKeepsLiveRef(t *testing.T) {
	r, dir, remote, waiting := staleWaitFixture(t)
	if err := r.landFetchWaiting(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := r.landDropStaleWaiting(context.Background(), dir, "main"); err != nil {
		t.Fatal(err)
	}
	if got := remoteRefCommit(t, remote, landWaitRef("main")); got != waiting {
		t.Errorf("the live waiting ref = %q, want it kept at %s", got, waiting)
	}
}

// TestLandDropStaleWaitingDropsMovedPast: main moves past the waiting
// landing by another commit, so the ref no longer fast-forwards the
// target and goes.
func TestLandDropStaleWaitingDropsMovedPast(t *testing.T) {
	r, dir, remote, _ := staleWaitFixture(t)
	pushToMain(t, remote, "other.txt", "other\n")
	git(t, dir, "fetch", "-q", "origin", "+refs/heads/main:refs/heads/main")
	if err := r.landFetchWaiting(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := r.landDropStaleWaiting(context.Background(), dir, "main"); err != nil {
		t.Fatal(err)
	}
	if got := remoteRefCommit(t, remote, landWaitRef("main")); got != "" {
		t.Errorf("the stale waiting ref = %q, want it dropped", got)
	}
	if r.landHasCommit(context.Background(), dir, landWaitRef("main")) {
		t.Error("the local copy of the stale ref remains")
	}
}
