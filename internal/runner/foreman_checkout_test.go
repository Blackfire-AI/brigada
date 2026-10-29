package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareForemanCheckout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remote := newRemote(t)
	scratch := t.TempDir()
	first, err := PrepareForemanCheckout(ctx, scratch, remote, "", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path != filepath.Join(scratch, "repo") || first.Branch != "main" {
		t.Fatalf("checkout = %+v", first)
	}
	// Build a new default branch, while the remote HEAD still names main.
	seed := t.TempDir()
	git(t, "", "clone", remote, seed)
	git(t, seed, "checkout", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(seed, "code.go"), []byte("package example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", ".")
	git(t, seed, "-c", "user.name=test", "-c", "user.email=test@brigada.invalid", "commit", "-m", "new default")
	git(t, seed, "push", "origin", "trunk")
	second, err := PrepareForemanCheckout(ctx, scratch, remote, "", "trunk", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Commit == first.Commit || second.Commit != strings.TrimSpace(git(t, seed, "rev-parse", "HEAD")) {
		t.Fatalf("stale checkout: %+v -> %+v", first, second)
	}
	if got := strings.TrimSpace(git(t, second.Path, "branch", "--show-current")); got != "trunk" {
		t.Fatalf("branch = %s", got)
	}
	// Refresh the same branch after it moves, removing stale files only
	// inside repo while preserving session records in its parent.
	if err := os.WriteFile(filepath.Join(seed, "code.go"), []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", ".")
	git(t, seed, "-c", "user.name=test", "-c", "user.email=test@brigada.invalid", "commit", "-m", "advance trunk")
	git(t, seed, "push", "origin", "trunk")
	for _, path := range []string{filepath.Join(second.Path, "README.md"), filepath.Join(second.Path, "stale.txt"), filepath.Join(scratch, "session.txt")} {
		if err := os.WriteFile(path, []byte("keep only session"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	third, err := PrepareForemanCheckout(ctx, scratch, remote, "", "trunk", nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.Commit == second.Commit || third.Commit != strings.TrimSpace(git(t, seed, "rev-parse", "HEAD")) {
		t.Fatalf("stale second refresh: %+v", third)
	}
	if status := strings.TrimSpace(git(t, third.Path, "status", "--porcelain")); status != "" {
		t.Fatalf("refresh left changes: %s", status)
	}
	if _, err := os.Stat(filepath.Join(scratch, "session.txt")); err != nil {
		t.Fatalf("session lost: %v", err)
	}
	// A failed fetch cannot turn a stale snapshot into a successful
	// refresh, and completes as an infrastructure error (issue #46):
	// the network's, never the turn's own, failure.
	remotePath := strings.TrimPrefix(remote, "file://")
	if err := os.Rename(remotePath, remotePath+"-gone"); err != nil {
		t.Fatal(err)
	}
	_, err = PrepareForemanCheckout(ctx, scratch, remote, "", "trunk", nil)
	if err == nil || !strings.Contains(err.Error(), "fetch") || !strings.Contains(err.Error(), "does not appear to be a git repository") {
		t.Fatalf("fetch failure = %v", err)
	}
	var infra *infraError
	if !errors.As(err, &infra) {
		t.Errorf("foreman checkout fetch failure = %T, want an infrastructure error", err)
	}
	empty := t.TempDir()
	if _, err := PrepareForemanCheckout(ctx, empty, remote, "", "main", nil); err == nil || !strings.Contains(err.Error(), "clone") {
		t.Fatalf("clone failure = %v", err)
	} else if !errors.As(err, &infra) {
		t.Errorf("foreman checkout clone failure = %T, want an infrastructure error", err)
	}
	if _, err := os.Stat(filepath.Join(empty, "repo")); !os.IsNotExist(err) {
		t.Fatalf("partial clone left behind: %v", err)
	}
}
