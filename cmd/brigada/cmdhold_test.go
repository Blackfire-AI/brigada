package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestHoldRelease covers `brigada hold release --repo URL` (§4.6): the
// usage errors, a project with no hold to release, and an unknown repo.
func TestHoldRelease(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	repo := "https://example.invalid/hold.git"
	// Flags precede positionals, so the state directory goes right after
	// the command words.
	do := func(cmd []string, rest ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		args := append(append(append([]string{}, cmd...), "--state-dir", state), rest...)
		code := run(ctx, args, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	hold := []string{"hold", "release"}

	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"hold"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: brigada hold release --repo URL") {
		t.Errorf("hold alone = %d %q, want usage", code, stderr.String())
	}
	stderr.Reset()
	hookState := t.TempDir()
	if code := run(ctx, []string{"hold", "yield", "--state-dir", hookState}, &stdout, &stderr); code != 0 {
		t.Errorf("hold yield = %d %q, want hook-safe exit zero", code, stderr.String())
	}
	if data, err := os.ReadFile(filepath.Join(hookState, "hold-yield.log")); err != nil || len(data) == 0 {
		t.Errorf("failed hook must log locally: %s %v", data, err)
	}
	if code, _, stderr := do(hold); code != 2 || !strings.Contains(stderr, "needs --repo URL") {
		t.Errorf("hold release without a repo = %d %q, want a usage error", code, stderr)
	}
	if code, _, stderr := do(hold, "--repo", repo); code == 0 || !strings.Contains(stderr, "no project for repo "+repo) {
		t.Errorf("hold release of an unknown repo = %d %q, want not found", code, stderr)
	}
	if code, _, stderr := do([]string{"project", "set"}, "--repo", repo, "hold_idle_minutes=20"); code != 0 {
		t.Fatalf("project set = %d %q", code, stderr)
	}
	if code, stdout, stderr := do(hold, "--repo", repo); code != 0 || !strings.Contains(stdout, "no interactive hold on "+repo) {
		t.Errorf("hold release with nothing held = %d %q (stderr %q), want the none-held line", code, stdout, stderr)
	}
}

// This path must fail before consulting any bridge bindings, while preserving
// the Stop hook's exit-zero contract and recording why no yield was attempted.
func TestHoldYieldMissingExplicitConfigLogsAndStops(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	missing := filepath.Join(t.TempDir(), "missing-config")
	var stderr bytes.Buffer
	code := runHoldYield(context.Background(), []string{"--state-dir", state, "--config", missing}, &stderr)
	if code != 0 {
		t.Fatalf("hook exited %d: %s", code, stderr.String())
	}
	want := "read config " + missing
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("missing config was not rejected before bridge discovery: %s", stderr.String())
	}
	log, err := os.ReadFile(filepath.Join(state, "hold-yield.log"))
	if err != nil || !strings.Contains(string(log), want) {
		t.Fatalf("missing config failure was not logged: %s %v", log, err)
	}
}
