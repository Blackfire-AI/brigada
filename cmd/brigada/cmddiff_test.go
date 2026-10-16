package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

type stubTasks struct {
	task engine.TaskInfo
	err  error
}

func (s stubTasks) Task(context.Context, int64) (engine.TaskInfo, error) { return s.task, s.err }

// gitIn runs git in dir for a test, failing it on error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=brigada test",
		"-c", "user.email=test@brigada.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (in %s): %v: %s", args, dir, err, out)
	}
	return string(out)
}

// diffFixture is a bare remote with main and a task branch that adds a
// file, and a clone of it whose origin is that remote.
func diffFixture(t *testing.T) (remote, clone string) {
	t.Helper()
	remote = testsupport.NewGitRemote(t)
	work := t.TempDir()
	gitIn(t, work, "clone", "-q", remote, ".")
	gitIn(t, work, "checkout", "-q", "-b", "brigada/task-5", "origin/main")
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("the new feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", "-A")
	gitIn(t, work, "commit", "-qm", "add the feature")
	gitIn(t, work, "push", "-q", "origin", "brigada/task-5")
	clone = t.TempDir()
	gitIn(t, clone, "clone", "-q", remote, ".")
	return remote, clone
}

func TestDiffPrintsTheTasksChange(t *testing.T) {
	remote, clone := diffFixture(t)
	stub := stubTasks{task: engine.TaskInfo{ID: 5, RepoURL: remote, BaseRef: "main", Branch: "brigada/task-5"}}

	var out, errb bytes.Buffer
	if code := printDiff(context.Background(), stub, clone, 5, "", false, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	for _, want := range []string{"+++ b/feature.txt", "+the new feature"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("diff = %q, want it to contain %q", out.String(), want)
		}
	}

	out.Reset()
	if code := printDiff(context.Background(), stub, clone, 5, "", true, &out, &errb); code != 0 {
		t.Fatalf("--stat exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "feature.txt") || strings.Contains(out.String(), "+the new feature") {
		t.Errorf("--stat = %q, want the diffstat only", out.String())
	}
}

func TestDiffRefusesOutsideTheRepo(t *testing.T) {
	_, clone := diffFixture(t)
	stub := stubTasks{task: engine.TaskInfo{ID: 5, RepoURL: "https://example.invalid/other.git", BaseRef: "main", Branch: "brigada/task-5"}}
	var out, errb bytes.Buffer
	if code := printDiff(context.Background(), stub, clone, 5, "", false, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "https://example.invalid/other.git") || !strings.Contains(errb.String(), "remote.git") {
		t.Errorf("stderr = %q, want both repos named", errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", out.String())
	}

	// A directory that is no repository at all is refused too.
	errb.Reset()
	if code := printDiff(context.Background(), stub, t.TempDir(), 5, "", false, &out, &errb); code != 1 {
		t.Errorf("outside any repo: exit %d, want 1", code)
	}
}

func TestDiffTaskWithoutBranch(t *testing.T) {
	stub := stubTasks{task: engine.TaskInfo{ID: 5, State: "ready", RepoURL: "r", BaseRef: "main"}}
	var out, errb bytes.Buffer
	if code := printDiff(context.Background(), stub, t.TempDir(), 5, "", false, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no result branch yet") {
		t.Errorf("exit %d, stderr %q, want the no-branch answer", code, errb.String())
	}
	stub.err = errors.New("engine down")
	errb.Reset()
	if code := printDiff(context.Background(), stub, t.TempDir(), 5, "", false, &out, &errb); code != 1 || !strings.Contains(errb.String(), "engine down") {
		t.Errorf("exit %d, stderr %q, want the engine's error", code, errb.String())
	}
}

func TestSameRepo(t *testing.T) {
	if !sameRepo("https://Example.invalid/a/b.git/", "https://example.invalid/a/b") {
		t.Error("case, .git and a trailing slash should not tell repos apart")
	}
	if sameRepo("https://example.invalid/a/b", "https://example.invalid/a/c") {
		t.Error("different repos compared equal")
	}
}

func TestDiffUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"diff"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage: brigada diff") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}
