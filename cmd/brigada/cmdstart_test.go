package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func startDo(args ...string) (int, string, string) {
	var out, errs bytes.Buffer
	code := run(context.Background(), append([]string{"start"}, args...), &out, &errs)
	return code, out.String(), errs.String()
}

func TestStartNoSavedAgents(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty-file=%t", empty), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			state := testsupport.ShortStateDir(t)
			if empty {
				if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), nil); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(state); err != nil {
				t.Fatal(err)
			}
			code, out, errs := startDo("--state-dir", state)
			if code != 1 || out != "" || errs != "brigada: no saved agents; run brigada init in a repository first\n" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
			}
			for _, name := range []string{"engine.log", "engine.lock", "runner.log"} {
				if _, err := os.Stat(filepath.Join(state, name)); !os.IsNotExist(err) {
					t.Errorf("start without saved agents created %s: %v", name, err)
				}
			}
		})
	}
}

func TestStartAlreadyRunning(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			state := testsupport.ShortStateDir(t)
			if saved {
				if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), []string{"harness=codex,model=gpt-5"}); err != nil {
					t.Fatal(err)
				}
			}
			pid := startStopHelper(t, "runner", state)
			code, out, errs := startDo("--state-dir", state)
			if code != 0 || out != fmt.Sprintf("runner already running (pid %d)\n", pid) || errs != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
			}
			if _, err := os.Stat(filepath.Join(state, "engine.log")); !os.IsNotExist(err) {
				t.Fatalf("already running started an engine: %v", err)
			}
		})
	}
}

func TestStartSavedAgents(t *testing.T) {
	binary := testsupport.BuildBrigada(t)
	old := initBinary
	initBinary = binary
	t.Cleanup(func() { initBinary = old })
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("relative-paths=%t", relative), func(t *testing.T) {
			state := testsupport.ShortStateDir(t)
			config := filepath.Join(state, "config")
			if err := os.WriteFile(config, []byte("notify = none\ntools.claude/sonnet = none\n"), 0600); err != nil {
				t.Fatal(err)
			}
			saved := []string{
				"harness=claude,model=sonnet,account=work,effort=high,classes=review",
				"harness=codex,model=gpt-5,effort=low,classes=implement+answer",
			}
			if err := runnerstate.SaveParallel(filepath.Join(state, "runner"), 3); err != nil {
				t.Fatal(err)
			}
			if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), saved); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if code, out, errs := stopDo(t, state, "--engine"); code != 0 {
					t.Errorf("cleanup: code=%d stdout=%q stderr=%q", code, out, errs)
				}
			})
			stateArg, configArg := state, config
			if relative {
				t.Chdir(state)
				stateArg, configArg = ".", "config"
			}
			code, out, errs := startDo("--state-dir", stateArg, "--config", configArg)
			if code != 0 || errs != "" {
				log, _ := os.ReadFile(filepath.Join(state, "runner.log"))
				engineLog, _ := os.ReadFile(filepath.Join(state, "engine.log"))
				t.Fatalf("code=%d stdout=%q stderr=%q runner log=%s engine log=%s", code, out, errs, log, engineLog)
			}
			rec, running, err := runnerstate.Running(filepath.Join(state, "runner"))
			if err != nil || !running || len(rec.Agents) != 2 || rec.MaxParallel != 3 {
				t.Fatalf("runner=%+v live=%t err=%v", rec, running, err)
			}
			if out != fmt.Sprintf("runner started (pid %d); log: %s\n", rec.PID, filepath.Join(state, "runner.log")) {
				t.Fatalf("stdout=%q", out)
			}
			if !lifecycle.EngineRunning(context.Background(), lifecycle.PathsOf(state).Socket) {
				t.Fatal("engine not started on demand")
			}
			claude, codex := rec.Agents[0], rec.Agents[1]
			if claude.Harness != "claude" || claude.Model != "sonnet" || claude.Account != "work" || claude.Effort != "high" || claude.Tools != "none" || !slices.Equal(claude.Classes, []string{"review"}) ||
				codex.Harness != "codex" || codex.Model != "gpt-5" || codex.Effort != "low" || !slices.Equal(codex.Classes, []string{"implement", "answer"}) {
				t.Fatalf("saved choices not applied: %+v", rec.Agents)
			}
			got, err := runnerstate.LoadAgents(filepath.Join(state, "runner"))
			if err != nil || !slices.Equal(got, saved) {
				t.Fatalf("saved choices changed: %q, %v", got, err)
			}
			code, out, errs = startDo("--state-dir", stateArg, "--config", configArg)
			if code != 0 || out != fmt.Sprintf("runner already running (pid %d)\n", rec.PID) || errs != "" {
				t.Fatalf("repeat: code=%d stdout=%q stderr=%q", code, out, errs)
			}
		})
	}
}

func TestStartInvalidArgumentsAndAgents(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state := testsupport.ShortStateDir(t)
	code, out, errs := startDo("--state-dir", state, "extra")
	if code != 2 || out != "" || !strings.Contains(errs, "Run 'brigada start --help' for usage.") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
	for _, spec := range []string{"broken", "harness=codex,effort=bogus"} {
		if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), []string{spec}); err != nil {
			t.Fatal(err)
		}
		code, out, errs := startDo("--state-dir", state)
		if code != 1 || out != "" || !strings.Contains(errs, "saved agents:") {
			t.Fatalf("spec=%q code=%d stdout=%q stderr=%q", spec, code, out, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "engine.log")); !os.IsNotExist(err) {
		t.Fatalf("invalid agents started an engine: %v", err)
	}
}

// TestStartLiftsADrainPause: an interrupted `stop --engine --drain`
// leaves the engine up and paused with origin `drain`; `brigada start`
// lifts that pause through the engine's drain-only lift, with its wake
// evaluation, and says so (§8.3).
func TestStartLiftsADrainPause(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state, st, _ := drainTestEnv(t)
	client := api.NewClient(lifecycle.PathsOf(state).Socket, clientToken(t, state))
	if _, err := client.Pause(context.Background(), "", engine.PauseOriginDrain); err != nil {
		t.Fatalf("pause the engine for the drain: %v", err)
	}
	rec, running, err := runnerstate.Running(filepath.Join(state, "runner"))
	if err != nil || !running {
		t.Fatalf("the runner helper is not running: %v", err)
	}
	code, out, errs := startDo("--state-dir", state)
	want := fmt.Sprintf("engine resumed: the drain pause is lifted\nrunner already running (pid %d)\n", rec.PID)
	if code != 0 || out != want || errs != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q, want %q", code, out, errs, want)
	}
	if origin := drainPause(t, st); origin != "" {
		t.Fatalf("the engine pause still stands with origin %q, want it lifted", origin)
	}
}

// TestStartLeavesAUserPause: a user's pause is not start's to lift;
// start says the engine stays paused (§8.3).
func TestStartLeavesAUserPause(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state, st, _ := drainTestEnv(t)
	client := api.NewClient(lifecycle.PathsOf(state).Socket, clientToken(t, state))
	if _, err := client.Pause(context.Background(), "", engine.PauseOriginUser); err != nil {
		t.Fatalf("pause the engine: %v", err)
	}
	rec, running, err := runnerstate.Running(filepath.Join(state, "runner"))
	if err != nil || !running {
		t.Fatalf("the runner helper is not running: %v", err)
	}
	code, out, errs := startDo("--state-dir", state)
	want := fmt.Sprintf("the engine stays paused (a user's pause); `brigada resume` lifts it\nrunner already running (pid %d)\n", rec.PID)
	if code != 0 || out != want || errs != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q, want %q", code, out, errs, want)
	}
	if origin := drainPause(t, st); origin != engine.PauseOriginUser {
		t.Fatalf("the standing engine pause has origin %q, want the user's", origin)
	}
}

// TestStartWithNoPauseSaysNothing: a start over no pause prints only
// the runner's line — the lift is silent when there is nothing to lift.
func TestStartWithNoPauseSaysNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state, _, _ := drainTestEnv(t)
	rec, running, err := runnerstate.Running(filepath.Join(state, "runner"))
	if err != nil || !running {
		t.Fatalf("the runner helper is not running: %v", err)
	}
	code, out, errs := startDo("--state-dir", state)
	want := fmt.Sprintf("runner already running (pid %d)\n", rec.PID)
	if code != 0 || out != want || errs != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q, want %q", code, out, errs, want)
	}
}
