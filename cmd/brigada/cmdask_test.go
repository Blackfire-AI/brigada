package main

import (
	"os/exec"
	"testing"
)

// TestAskRepoFollowsTheWorkingDirectory: without --repo, ask names the
// project its working directory's origin names, as attach does; --repo
// wins; and a directory outside any repository names none, so the
// engine's only project is used.
func TestAskRepoFollowsTheWorkingDirectory(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	origin := "https://example.invalid/here.git"
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", origin).CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
	t.Chdir(repo)

	if got, err := askRepo(""); err != nil || got != origin {
		t.Fatalf("askRepo(\"\") in a repo = %q, %v; want %q", got, err, origin)
	}
	other := "https://example.invalid/other.git"
	if got, err := askRepo(other); err != nil || got != other {
		t.Fatalf("askRepo(other) = %q, %v; want %q", got, err, other)
	}

	t.Chdir(t.TempDir())
	if got, err := askRepo(""); err != nil || got != "" {
		t.Fatalf("askRepo(\"\") outside a repo = %q, %v; want empty", got, err)
	}
}
