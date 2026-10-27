package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestProjectRemote covers the choice itself.
func TestProjectRemote(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	if _, err := projectRemote(ctx, dir, "example.com/team/repo"); err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Fatalf("no remote: err = %v, want one naming --repo", err)
	}
	gitIn(t, dir, "remote", "add", "a", "https://example.com/team/other.git")
	gitIn(t, dir, "remote", "add", "b", "https://example.com/team/repo.git")
	if got, _ := projectRemote(ctx, dir, "example.com/team/repo"); got != "b" {
		t.Errorf("matching b: got %q", got)
	}
	if got, err := projectRemote(ctx, dir, "example.com/team/none"); err == nil {
		t.Errorf("several unmatched, no origin: got %q, want an error", got)
	} else if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("err = %v, want it to name --repo", err)
	}
	gitIn(t, dir, "remote", "add", "origin", "https://example.com/team/other.git")
	if got, _ := projectRemote(ctx, dir, "example.com/team/none"); got != "origin" {
		t.Errorf("unmatched with origin: got %q", got)
	}
	gitIn(t, dir, "remote", "set-url", "origin", "https://example.com/team/repo.git")
	if got, _ := projectRemote(ctx, dir, "example.com/team/repo"); got != "origin" {
		t.Errorf("origin and b both match: got %q, want origin", got)
	}
}

// TestDiffFindsTheProjectsRemote: the fetch goes to the remote that
// stands for the project, whatever its name, and falls back to origin
// when the --repo escape names a project no remote matches.
func TestDiffFindsTheProjectsRemote(t *testing.T) {
	remote, clone := diffFixture(t)
	stub := stubTasks{task: engine.TaskInfo{ID: 5, RepoURL: remote, BaseRef: "main", Branch: "brigada/task-5"}}
	diffOK := func(t *testing.T, repo string, stub stubTasks) {
		t.Helper()
		var out, errb bytes.Buffer
		if code := printDiff(context.Background(), stub, clone, 5, repo, false, &out, &errb); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errb.String())
		}
		if !strings.Contains(out.String(), "+the new feature") {
			t.Errorf("diff = %q, want the task's change", out.String())
		}
	}

	// The only remote is not called origin.
	gitIn(t, clone, "remote", "rename", "origin", "upstream")
	diffOK(t, "", stub)

	// An unrelated origin beside the matching remote: --repo names the project.
	gitIn(t, clone, "remote", "add", "origin", testsupport.NewGitRemote(t))
	diffOK(t, remote, stub)

	// No remote matches the --repo identity: origin is used.
	gitIn(t, clone, "remote", "remove", "origin")
	gitIn(t, clone, "remote", "rename", "upstream", "origin")
	gitIn(t, clone, "remote", "add", "unused", "/nonexistent/example.com/unused.git")
	mirror := stubTasks{task: engine.TaskInfo{ID: 5, RepoURL: "https://example.com/team/repo.git", BaseRef: "main", Branch: "brigada/task-5"}}
	diffOK(t, "https://example.com/team/repo.git", mirror)
}

// TestLandUsesTheProjectsRemote: land fetches from, reads the default
// branch of, and names in its push command the remote that stands for
// the project, not a differently named or unrelated origin.
func TestLandUsesTheProjectsRemote(t *testing.T) {
	t.Run("only remote is not origin", func(t *testing.T) {
		_, clone, f := landFixture(t)
		gitIn(t, clone, "remote", "rename", "origin", "upstream")
		// The tracking ref names a branch the remote's own HEAD does not
		// (the remote's HEAD is main): the tracking ref is read first.
		gitIn(t, clone, "branch", "-q", "trunk", "main")
		gitIn(t, clone, "push", "-q", "upstream", "trunk")
		gitIn(t, clone, "remote", "set-head", "upstream", "trunk")
		gitIn(t, clone, "switch", "-q", "trunk")
		code, out, errs := land(t, f, clone, landOptions{ids: []int64{1}})
		if code != 0 {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
		}
		if !strings.Contains(out, "git push upstream trunk") {
			t.Errorf("the push command must name upstream:\n%s", out)
		}
	})
	t.Run("matching second remote beside an unrelated origin", func(t *testing.T) {
		remote, clone, f := landFixture(t)
		gitIn(t, clone, "remote", "rename", "origin", "upstream")
		gitIn(t, clone, "remote", "add", "origin", testsupport.NewGitRemote(t))
		// origin's own HEAD points elsewhere; the default branch must come from upstream.
		gitIn(t, clone, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/elsewhere")
		code, out, errs := land(t, f, clone, landOptions{ids: []int64{1}, repo: remote})
		if code != 0 {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
		}
		if !strings.Contains(out, "git push upstream main") {
			t.Errorf("the push command must name upstream:\n%s", out)
		}
	})
	t.Run("--repo matches no remote so origin is used", func(t *testing.T) {
		_, clone, f := landFixture(t)
		gitIn(t, clone, "remote", "add", "unused", "/nonexistent/example.com/unused.git")
		const project = "https://example.com/team/repo.git"
		for id, ti := range f.tasks {
			ti.RepoURL = project
			f.tasks[id] = ti
		}
		code, out, errs := land(t, f, clone, landOptions{ids: []int64{1}, repo: project})
		if code != 0 {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
		}
		if !strings.Contains(out, "git push origin main") {
			t.Errorf("the push command must name origin:\n%s", out)
		}
	})
}

func TestProjectRemoteSingleUnmatchedAndAlias(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "remote", "add", "mirror", "https://example.com/team/mirror.git")
	if got, err := projectRemote(ctx, dir, "example.com/team/repo"); err != nil || got != "mirror" {
		t.Errorf("single unmatched remote: got %q, %v, want mirror", got, err)
	}

	// An SSH alias resolves through the configured hostname before matching.
	old := initSSHConfig
	initSSHConfig = func(context.Context, string) ([]byte, error) {
		return []byte("hostname example.com\nport 22\n"), nil
	}
	t.Cleanup(func() { initSSHConfig = old })
	gitIn(t, dir, "remote", "add", "other", "https://example.com/team/other.git")
	gitIn(t, dir, "remote", "add", "build", "git@build:team/repo.git")
	if got, err := projectRemote(ctx, dir, "example.com/team/repo"); err != nil || got != "build" {
		t.Errorf("aliased remote: got %q, %v, want build", got, err)
	}
}

func TestRemovalRemotePlanNamesTheOnlyRemote(t *testing.T) {
	root := wireRepo(t)
	remote := t.TempDir()
	wireGit(t, remote, "init", "--bare", "-q")
	wireGit(t, root, "remote", "add", "upstream", remote)
	wireGit(t, root, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "test")
	wireGit(t, root, "push", "-q", "upstream", "HEAD:refs/heads/brigada/task-1")
	var out bytes.Buffer
	printRemovalRemote(context.Background(), &out, root)
	for _, want := range []string{"1 brigada/task-* branches", "git push upstream --delete 'brigada/task-1'"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "origin") {
		t.Errorf("origin named although no such remote exists:\n%s", out.String())
	}
}
