package main

import (
	"fmt"
	"strings"
	"testing"
)

// An interrupted drain leaves the engine up and paused with origin
// drain: start lifts it, with the runner already running, and leaves a
// user's pause alone, saying the engine stays paused.
func TestStartLiftsADrainPauseAndKeepsAUsers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, state := startSettingsEngine(t)
	pid := startStopHelper(t, "runner", state)
	if _, err := dispatchClient(t, state).Pause(ctx, "", "drain"); err != nil {
		t.Fatal(err)
	}
	code, out, errs := startDo("--state-dir", state)
	want := fmt.Sprintf("runner already running (pid %d)\nengine resumed: the drain's pause is lifted\n", pid)
	if code != 0 || out != want || errs != "" {
		t.Fatalf("code=%d out=%q err=%q, want %q", code, out, errs, want)
	}
	if got := pausesOf(t, ctx, state); len(got) != 0 {
		t.Fatalf("pauses after start = %q, want none", got)
	}

	if code, _, errs := runWithState(ctx, state, "pause"); code != 0 {
		t.Fatalf("pause: %d %s", code, errs)
	}
	code, out, errs = startDo("--state-dir", state)
	if code != 0 || !strings.Contains(out, "engine stays paused") || errs != "" {
		t.Fatalf("start over a user pause: code=%d out=%q err=%q", code, out, errs)
	}
	if got := pausesOf(t, ctx, state); strings.Join(got, ",") != "engine:user" {
		t.Fatalf("pauses = %q, want the user's kept", got)
	}
}
