package main

import (
	"bytes"
	"strings"
	"testing"
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
	if code := run(ctx, []string{"hold", "yield"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), `unknown hold subcommand "yield"`) {
		t.Errorf("hold yield = %d %q, want a usage error", code, stderr.String())
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
