package runner

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
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

// TestLandBaseTaskID covers the branch names the landing reads a
// dependency's task id from: the engine's first-attempt, restart and
// re-lease names, and the names that are none of them.
func TestLandBaseTaskID(t *testing.T) {
	for _, tc := range []struct {
		ref string
		id  int64
		ok  bool
	}{
		{"brigada/task-12", 12, true},
		{"brigada/task-12-r1", 12, true},
		{"brigada/task-12-r3", 12, true},
		{"brigada/task-12-a2", 12, true},
		{"main", 0, false},
		{"brigada/landed", 0, false},
		{"brigada/task-x", 0, false},
		{"task-12", 0, false},
		{"", 0, false},
	} {
		id, ok := landBaseTaskID(tc.ref)
		if id != tc.id || ok != tc.ok {
			t.Errorf("landBaseTaskID(%q) = %d, %t; want %d, %t", tc.ref, id, ok, tc.id, tc.ok)
		}
	}
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

// TestLandDropMarkedWaitingDropsLiveRef (issue #205): the engine's
// record marks the waiting ref stale after a restart forgot its
// landings, so the ref goes even though main still fast-forwards to it,
// the drop is reported, and a retried attempt drops a ref pushed again
// since.
func TestLandDropMarkedWaitingDropsLiveRef(t *testing.T) {
	ctx := context.Background()
	r, dir, remote, _ := staleWaitFixture(t)
	r.grant = &api.JobGrant{LandTarget: "main", DropWaitingRef: true}
	if err := r.landFetchWaiting(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := r.landDropMarkedWaiting(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if got := remoteRefCommit(t, remote, landWaitRef("main")); got != "" {
		t.Errorf("the marked waiting ref = %q, want it dropped", got)
	}
	if r.landHasCommit(ctx, dir, landWaitRef("main")) {
		t.Error("the local copy of the marked ref remains")
	}
	if r.staleDropped != "main" {
		t.Errorf("staleDropped = %q, want the drop of main reported", r.staleDropped)
	}

	// Between attempts a landing leased before the mark pushes the ref
	// again, its stack still on the forgotten commit: the retry drops it
	// too, before anything stacks on it.
	git(t, dir, "checkout", "-q", "--detach", "main")
	commitFile(t, dir, "unmarked.txt")
	git(t, dir, "push", "-q", "origin", "HEAD:"+landWaitRef("main"))
	if err := r.landFetchWaiting(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := r.landDropMarkedWaiting(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if got := remoteRefCommit(t, remote, landWaitRef("main")); got != "" {
		t.Errorf("the waiting ref re-pushed between attempts = %q, want the retry to drop it", got)
	}
	if r.landHasCommit(ctx, dir, landWaitRef("main")) {
		t.Error("the local copy of the re-pushed ref remains after the retry")
	}
}

// TestLandDropMarkedWaitingKeepsUnmarkedRef: without the engine's mark a
// waiting ref main still fast-forwards to stays, and no drop is reported.
func TestLandDropMarkedWaitingKeepsUnmarkedRef(t *testing.T) {
	ctx := context.Background()
	r, dir, remote, waiting := staleWaitFixture(t)
	r.grant = &api.JobGrant{LandTarget: "main"}
	if err := r.landFetchWaiting(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := r.landDropMarkedWaiting(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := r.landDropStaleWaiting(ctx, dir, "main"); err != nil {
		t.Fatal(err)
	}
	if got := remoteRefCommit(t, remote, landWaitRef("main")); got != waiting {
		t.Errorf("the unmarked waiting ref = %q, want it kept at %s", got, waiting)
	}
	if r.staleDropped != "" {
		t.Errorf("staleDropped = %q, want no drop reported", r.staleDropped)
	}
}
