package main

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// --drain is the drain half of --engine: without it the flag is refused.
func TestStopDrainNeedsEngine(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	code, out, errs := stopDo(t, state, "--drain")
	if code != 2 || out != "" || !strings.Contains(errs, "--drain needs --engine") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

// The wait names the jobs it still waits on as the set changes, returns
// once none is left, and ends early when the pause is lifted or the
// wait is interrupted.
func TestWaitDrainedNamesTheJobs(t *testing.T) {
	old := drainPoll
	drainPoll = time.Millisecond
	t.Cleanup(func() { drainPoll = old })
	work := engine.PauseJob{ID: 7, Kind: "work", Status: "running"}
	review := engine.PauseJob{ID: 9, Kind: "review", Status: "leased"}
	steps := [][]engine.PauseJob{{work}, {work}, {work, review}, {}}
	n := 0
	var out bytes.Buffer
	err := waitDrained(context.Background(), &out, func(context.Context) ([]engine.PauseJob, bool, error) {
		jobs := steps[n]
		n++
		return jobs, true, nil
	})
	want := "waiting on job 7 (work, running)\nwaiting on job 7 (work, running), job 9 (review, leased)\n"
	if err != nil || out.String() != want || n != len(steps) {
		t.Fatalf("err=%v out=%q polls=%d, want %q after %d polls", err, out.String(), n, want, len(steps))
	}

	err = waitDrained(context.Background(), &out, func(context.Context) ([]engine.PauseJob, bool, error) {
		return nil, false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "pause was lifted") {
		t.Fatalf("lifted pause: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	err = waitDrained(ctx, &out, func(context.Context) ([]engine.PauseJob, bool, error) {
		cancel()
		return []engine.PauseJob{work}, true, nil
	})
	if err != context.Canceled {
		t.Fatalf("interrupted wait: %v, want context.Canceled", err)
	}
}

func runWithState(ctx context.Context, state string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(ctx, append([]string{args[0], "--state-dir", state}, args[1:]...), &out, &errb)
	return code, out.String(), errb.String()
}

// The drain pauses the engine with origin drain and returns once it is
// drained; over a user's pause the origin stays user.
func TestDrainEnginePausesWithOriginDrain(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	rt := &runtime{stateDir: state, paths: lifecycle.PathsOf(state)}
	var out, errb bytes.Buffer
	if code := drainEngine(ctx, rt, &out, &errb); code != 0 || errb.Len() != 0 ||
		!strings.Contains(out.String(), "engine paused to drain") || !strings.Contains(out.String(), "engine drained") {
		t.Fatalf("drain: code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	if got := pausesOf(t, ctx, state); strings.Join(got, ",") != "engine:drain" {
		t.Fatalf("pauses = %q, want the drain's", got)
	}

	// A user's pause outranks it, and a drain over it leaves it the user's.
	if code, _, errs := runWithState(ctx, state, "pause"); code != 0 {
		t.Fatalf("pause: %d %s", code, errs)
	}
	out.Reset()
	if code := drainEngine(ctx, rt, &out, &errb); code != 0 || !strings.Contains(out.String(), "already paused by you") {
		t.Fatalf("drain over a user pause: code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	if got := pausesOf(t, ctx, state); strings.Join(got, ",") != "engine:user" {
		t.Fatalf("pauses = %q, want the origin kept as the user's", got)
	}
}

// stop --engine --drain pauses, finds the engine drained, then stops the
// runner and the engine as stop --engine does.
func TestStopEngineDrainStopsAfterTheDrain(t *testing.T) {
	binary := testsupport.BuildBrigada(t)
	old := initBinary
	initBinary = binary
	t.Cleanup(func() { initBinary = old })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state := testsupport.ShortStateDir(t)
	paths := lifecycle.PathsOf(state)
	if err := lifecycle.EnsureEngine(context.Background(), lifecycle.StartOptions{
		Socket: paths.Socket, StateDir: state, Binary: binary,
	}); err != nil {
		t.Fatal(err)
	}
	pid := lifecycle.EnginePid(paths.Lock)
	code, out, errs := stopDo(t, state, "--engine", "--drain")
	want := "engine paused to drain; running jobs finish, queued jobs wait\nengine drained\nno runner running\nengine stopped (pid " + strconv.Itoa(pid) + ")\n"
	if code != 0 || out != want || errs != "" {
		t.Fatalf("code=%d out=%q err=%q, want %q", code, out, errs, want)
	}
	if lifecycle.EnginePid(paths.Lock) != 0 {
		t.Fatal("engine still holds its lock")
	}
}
