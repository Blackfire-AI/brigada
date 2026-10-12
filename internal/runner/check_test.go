package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunCheck(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// A passing check: exit 0, the tail carries the output.
	res := runCheck(ctx, dir, "echo all good", time.Second)
	if res.ExitCode != 0 || res.TimedOut || res.Failed() {
		t.Errorf("passing check = %+v, want exit 0 and no failure", res)
	}
	if !strings.Contains(res.Output, "all good") {
		t.Errorf("output tail = %q, want the check's output", res.Output)
	}

	// A failing check names its exit code.
	res = runCheck(ctx, dir, "echo boom >&2; exit 3", time.Second)
	if res.ExitCode != 3 || !res.Failed() {
		t.Errorf("failing check = %+v, want exit 3 and failure", res)
	}
	if !strings.Contains(res.Output, "boom") {
		t.Errorf("output tail = %q, want stderr in the combined tail", res.Output)
	}

	// A timeout cuts the check short and marks it.
	res = runCheck(ctx, dir, "sleep 5", 100*time.Millisecond)
	if !res.TimedOut || !res.Failed() {
		t.Errorf("timed-out check = %+v, want timed out and failed", res)
	}

	// No command runs nothing: the absence of a check is not a failure.
	if res := runCheck(ctx, dir, "", time.Second); res.ExitCode != 0 || res.Output != "" {
		t.Errorf("empty check = %+v, want a silent pass", res)
	}
}

func TestRunCheckTailCapped(t *testing.T) {
	dir := t.TempDir()
	res := runCheck(context.Background(), dir, "seq 1 100000", 30*time.Second)
	if len(res.Output) > checkTailBytes {
		t.Errorf("tail is %d bytes, capped at %d", len(res.Output), checkTailBytes)
	}
	if !strings.HasSuffix(strings.TrimSpace(res.Output), "100000") {
		t.Errorf("the tail lost the end of the output: %q", tailOf(res.Output, 40))
	}
	// The full log is kept for the artifact.
	if len(res.Log) <= checkTailBytes {
		t.Errorf("the log was not kept whole: %d bytes", len(res.Log))
	}
}

func TestRunCheckRunsInDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("here"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runCheck(context.Background(), dir, "test -f marker", time.Second)
	if res.ExitCode != 0 {
		t.Errorf("check in the worktree = %+v, want it to see the worktree's files", res)
	}
}

// TestRunCheckTimeoutKillsTree covers the case the old test could not
// see: a check whose child outlives its sh. `sleep 30 & wait` leaves
// the sleep holding the output pipe, so a timeout that killed only sh
// would block on the pipe until the sleep exited; killing the group
// must cut the run short and still report it.
func TestRunCheckTimeoutKillsTree(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	res := runCheck(context.Background(), dir, "sleep 30 & wait", 100*time.Millisecond)
	elapsed := time.Since(start)
	if !res.TimedOut || !res.Failed() {
		t.Errorf("check = %+v, want timed out and failed", res)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the timed-out check took %s; the tree was not killed", elapsed)
	}
}
