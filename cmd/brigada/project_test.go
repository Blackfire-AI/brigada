package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestProjectForemanGolden runs the command against a real engine
// started in-process (§13), the same lifecycle a user's engine sees:
// choosing the foreman's harness and model prints the choice, reading it
// back prints the stored values, and the engine's refusal is the
// command's failure.
func TestProjectForemanGolden(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lifecycle.Run(ctx, lifecycle.Config{StateDir: state, Idle: time.Minute}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the engine stopped with an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the engine did not stop")
		}
	})
	socket := lifecycle.PathsOf(state).Socket
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	repo := "https://example.invalid/golden.git"

	// Choosing prints the choice, exactly, one line.
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"project", "foreman", "--repo", repo, "--harness", "claude", "--model", "opus", "--state-dir", state},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("choose exit = %d, stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "foreman set: harness=claude model=opus\n"; got != want {
		t.Fatalf("choose printed %q, want %q", got, want)
	}

	// Reading back prints the stored values, harness first.
	stdout.Reset()
	code = run(ctx, []string{"project", "foreman", "--repo", repo, "--state-dir", state}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("read exit = %d, stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "harness=claude model=opus\n"; got != want {
		t.Fatalf("read printed %q, want %q", got, want)
	}

	// Codex is resume-capable and can carry the foreman's MCP bridge.
	stdout.Reset()
	stderr.Reset()
	code = run(ctx, []string{"project", "foreman", "--repo", repo, "--harness", "codex", "--model", "opus", "--state-dir", state},
		&stdout, &stderr)
	if code != 0 || stdout.String() != "foreman set: harness=codex model=opus\n" {
		t.Fatalf("codex choose exit = %d, stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
	}

	// The list: one line per project, the foreman a project has not
	// chosen shown as "-".
	other := "https://example.invalid/other.git"
	if code := run(ctx, []string{"project", "foreman", "--repo", other, "--harness", "claude", "--model", "opus", "--state-dir", state},
		&stdout, &stderr); code != 0 {
		t.Fatalf("second project exit = %d, stderr = %q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = run(ctx, []string{"project", "list", "--state-dir", state}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("list exit = %d, stderr = %q", code, stderr.String())
	}
	wantList := repo + "  foreman=codex/opus  active_plans=0  needs_you=0\n" +
		other + "  foreman=claude/opus  active_plans=0  needs_you=0\n"
	if got := stdout.String(); got != wantList {
		t.Fatalf("list printed %q, want %q", got, wantList)
	}

	// An ask for a repo with no project names the known ones.
	stdout.Reset()
	stderr.Reset()
	code = run(ctx, []string{"ask", "--repo", "https://example.invalid/nope.git", "--state-dir", state, "hello"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "known projects: "+repo+", "+other) {
		t.Fatalf("ask refusal = code %d, stderr=%q", code, stderr.String())
	}

	// Kimi still has no way to carry the bridge, so it remains refused.
	stdout.Reset()
	stderr.Reset()
	code = run(ctx, []string{"project", "foreman", "--repo", repo, "--harness", "kimi", "--model", "opus", "--state-dir", state},
		&stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "cannot resume headless") {
		t.Fatalf("kimi refusal = code %d, stderr=%q", code, stderr.String())
	}
}
