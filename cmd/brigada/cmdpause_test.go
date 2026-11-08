package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
)

// dispatchClient is a client-role API client of the state directory's
// engine, to read what a command stored.
func dispatchClient(t *testing.T, state string) *api.Client {
	t.Helper()
	paths := lifecycle.PathsOf(state)
	_, secret, err := lifecycle.TokenForRole(paths.Tokens, "client")
	if err != nil {
		t.Fatal(err)
	}
	return api.NewClient(paths.Socket, secret)
}

// pausesOf lists the standing pauses as "scope:origin" strings.
func pausesOf(t *testing.T, ctx context.Context, state string) []string {
	t.Helper()
	st, err := dispatchClient(t, state).Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range st.Pauses {
		scope := "engine"
		if p.ProjectID != 0 {
			scope = p.RepoURL
		}
		out = append(out, scope+":"+p.Origin)
	}
	return out
}

// TestPauseResumeScopes covers `brigada pause` and `brigada resume` at
// each scope (§8.3): the engine, one project by --repo, an empty --repo
// (the engine, never the working directory's origin), a resume under a
// remaining scope, and the usage and not-found errors.
func TestPauseResumeScopes(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	repoA, repoB := "https://example.invalid/a.git", "https://example.invalid/b.git"
	a, b := "example.invalid/a", "example.invalid/b"
	do := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		full := append([]string{args[0], "--state-dir", state}, args[1:]...)
		code := run(ctx, full, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	for _, r := range []string{repoA, repoB} {
		if code, _, stderr := do("budget", "--repo", r, "--wakes", "5"); code != 0 {
			t.Fatalf("create project %s: %d %s", r, code, stderr)
		}
	}
	// The working directory's origin names project a; a bare pause must
	// still be the engine's.
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", repoA).CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
	t.Chdir(dir)

	expect := func(label string, code int, stdout string, want ...string) {
		t.Helper()
		if code != 0 {
			t.Fatalf("%s: exit %d", label, code)
		}
		for _, w := range want {
			if !strings.Contains(stdout, w) {
				t.Errorf("%s printed %q, want %q", label, stdout, w)
			}
		}
	}
	pauses := func(want ...string) {
		t.Helper()
		if got := pausesOf(t, ctx, state); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("pauses = %q, want %q", got, want)
		}
	}

	// Bare pause: the engine, even in a repository whose origin is a project.
	code, stdout, _ := do("pause")
	expect("pause", code, stdout, "dispatch paused for the engine (origin user)")
	pauses("engine:user")
	code, stdout, _ = do("pause")
	expect("second pause", code, stdout, "already paused for the engine")

	// Resume of a project under the engine's pause: it has no pause of its own.
	code, stdout, _ = do("resume", "--repo", repoA)
	expect("resume a", code, stdout, "project "+a+" has no pause of its own", "the engine's pause still holds it")
	pauses("engine:user")

	// A project's pause, lifted under the engine's, which remains.
	code, stdout, _ = do("pause", "--repo", repoB)
	expect("pause b", code, stdout, "dispatch paused for project "+b)
	pauses("engine:user", b+":user")
	code, stdout, _ = do("resume", "--repo", repoB)
	expect("resume b", code, stdout, "dispatch resumed for project "+b, "still paused by the engine's pause")
	pauses("engine:user")

	// The engine's resume lifts it alone, and a second one says nothing was paused.
	code, stdout, _ = do("pause", "--repo", repoB)
	expect("pause b again", code, stdout, "dispatch paused for project "+b)
	code, stdout, _ = do("resume")
	expect("resume engine", code, stdout, "dispatch resumed for the engine")
	pauses(b + ":user")
	code, stdout, _ = do("resume")
	expect("resume engine again", code, stdout, "nothing was paused for the engine")
	code, stdout, _ = do("resume", "--repo", repoB)
	expect("resume b last", code, stdout, "dispatch resumed for project "+b)
	pauses()

	// An empty --repo is the engine, not the working directory's origin.
	code, stdout, _ = do("pause", "--repo", "")
	expect("pause --repo \"\"", code, stdout, "dispatch paused for the engine")
	pauses("engine:user")
	code, stdout, _ = do("resume", "--repo", "")
	expect("resume --repo \"\"", code, stdout, "dispatch resumed for the engine")
	pauses()

	// Usage and not-found errors.
	if code, _, stderr := do("pause", "--repos", repoA); code != 2 || !strings.Contains(stderr, "repos") {
		t.Errorf("misspelled flag = %d %q, want a usage error", code, stderr)
	}
	if code, _, stderr := do("resume", "extra"); code != 2 || !strings.Contains(stderr, "takes no arguments") {
		t.Errorf("positional = %d %q, want a usage error", code, stderr)
	}
	if code, _, stderr := do("pause", "--repo", "https://example.invalid/none.git"); code != 1 || !strings.Contains(stderr, "no project") {
		t.Errorf("unknown project = %d %q, want not found", code, stderr)
	}
	pauses()
}
