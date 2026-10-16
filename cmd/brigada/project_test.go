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
	// No runner has registered an agent for the pair yet: the choice
	// stands, and the command warns and names the runner flag that would
	// serve it. The warning is on stderr, so the choice's own line is the
	// same one.
	for _, want := range []string{"warning:", "claude/opus", "--agent harness=claude,model=opus"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("choose warned %q, want it to contain %q", stderr.String(), want)
		}
	}
	stderr.Reset()

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
	// Projects are listed by canonical repo URL (design §13.1).
	canonRepo, canonOther := "example.invalid/golden", "example.invalid/other"
	wantList := canonRepo + "  foreman=codex/opus  active_plans=0  needs_you=0\n" +
		canonOther + "  foreman=claude/opus  active_plans=0  needs_you=0\n"
	if got := stdout.String(); got != wantList {
		t.Fatalf("list printed %q, want %q", got, wantList)
	}

	// An ask for a repo with no project names the known ones.
	stdout.Reset()
	stderr.Reset()
	code = run(ctx, []string{"ask", "--repo", "https://example.invalid/nope.git", "--state-dir", state, "hello"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "known projects: "+canonRepo+", "+canonOther) {
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

	// An effort is shown where set, on the choice and on the read-back;
	// a choice without one clears it and prints as before.
	stdout.Reset()
	stderr.Reset()
	code = run(ctx, []string{"project", "foreman", "--repo", repo, "--harness", "claude", "--model", "opus", "--effort", "high", "--state-dir", state},
		&stdout, &stderr)
	if code != 0 || stdout.String() != "foreman set: harness=claude model=opus effort=high\n" {
		t.Fatalf("effort choose exit = %d, stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	code = run(ctx, []string{"project", "foreman", "--repo", repo, "--state-dir", state}, &stdout, &stderr)
	if code != 0 || stdout.String() != "harness=claude model=opus effort=high\n" {
		t.Fatalf("effort read exit = %d, stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	code = run(ctx, []string{"project", "foreman", "--repo", repo, "--harness", "claude", "--model", "opus", "--state-dir", state},
		&stdout, &stderr)
	if code != 0 || stdout.String() != "foreman set: harness=claude model=opus\n" {
		t.Fatalf("effort cleared exit = %d, stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
	}

	// Levels are validated per harness, and effort goes with a choice.
	for name, args := range map[string][]string{
		"unknown level":   {"--harness", "claude", "--model", "opus", "--effort", "turbo"},
		"effort alone":    {"--effort", "high"},
		"codex bad level": {"--harness", "codex", "--model", "opus", "--effort", "extreme"},
	} {
		stdout.Reset()
		stderr.Reset()
		full := append([]string{"project", "foreman", "--repo", repo}, args...)
		full = append(full, "--state-dir", state)
		if code := run(ctx, full, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "effort") {
			t.Errorf("%s: code %d, stderr=%q; want a usage error naming effort", name, code, stderr.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	code = run(ctx, []string{"project", "foreman", "--repo", repo, "--harness", "codex", "--model", "opus", "--effort", "xhigh", "--state-dir", state},
		&stdout, &stderr)
	if code != 0 || stdout.String() != "foreman set: harness=codex model=opus effort=xhigh\n" {
		t.Fatalf("codex effort exit = %d, stdout=%q, stderr=%q", code, stdout.String(), stderr.String())
	}
}
