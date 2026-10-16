package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	remote, clone := diffFixture(t)
	stub := stubTasks{task: engine.TaskInfo{ID: 5, RepoURL: "https://example.invalid/other.git", BaseRef: "main", Branch: "brigada/task-5"}}
	var out, errb bytes.Buffer
	if code := printDiff(context.Background(), stub, clone, 5, "", false, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	want := fmt.Sprintf("task 5 belongs to example.invalid/other, but this directory is file:%s; run diff inside that repository", remote)
	if !strings.Contains(errb.String(), want) {
		t.Errorf("stderr = %q, want %q", errb.String(), want)
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

func TestDiffCanonicalRepositoryURLs(t *testing.T) {
	remote, clone := diffFixture(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the real origin URL visible to gitOriginURL, but fetch from the
	// fixture instead of the network. All fetches and diffs still use git.
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$3\" = fetch ]; then\n" +
		"  exec " + shellQuote(git) + " -c \"url.$BRIGADA_DIFF_REMOTE.insteadOf=$BRIGADA_DIFF_ORIGIN\" \"$@\"\n" +
		"fi\nexec " + shellQuote(git) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BRIGADA_DIFF_REMOTE", remote)
	for _, tc := range []struct{ name, origin, project string }{
		{"file URL", "file://" + remote, "file:" + remote},
		{"absolute path", remote, "file:" + remote},
		{"https", "https://user:secret@Example.invalid:443/Team/repo.git/", "example.invalid/Team/repo"},
		{"scp", "git@Example.invalid:Team/repo.git", "https://example.invalid/Team/repo.git"},
		{"canonical git suffix", "https://example.invalid/Team/repo.git.git", "example.invalid/Team/repo.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitIn(t, clone, "remote", "set-url", "origin", tc.origin)
			t.Setenv("BRIGADA_DIFF_ORIGIN", tc.origin)
			stub := stubTasks{task: engine.TaskInfo{ID: 5, RepoURL: tc.project, BaseRef: "main", Branch: "brigada/task-5"}}
			for _, repo := range []string{"", tc.origin} {
				var out, errb bytes.Buffer
				if code := printDiff(context.Background(), stub, clone, 5, repo, false, &out, &errb); code != 0 {
					t.Fatalf("repo override %q: exit %d, stderr %q", repo, code, errb.String())
				}
				if !strings.Contains(out.String(), "+the new feature") {
					t.Errorf("diff = %q, want the task's change", out.String())
				}
			}
		})
	}
}

func TestDiffRefusesDifferentCanonicalRepositories(t *testing.T) {
	_, clone := diffFixture(t)
	for _, tc := range []struct{ name, origin, project, here, task string }{
		{"path", "https://user:secret@Example.invalid/Team/other.git/", "git@example.invalid:Team/repo.git", "example.invalid/Team/other", "example.invalid/Team/repo"},
		{"path case", "https://Example.invalid/team/repo.git", "example.invalid/Team/repo", "example.invalid/team/repo", "example.invalid/Team/repo"},
		{"port", "ssh://git@Example.invalid:2222/Team/repo.git", "https://example.invalid/Team/repo.git", "example.invalid:2222/Team/repo", "example.invalid/Team/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitIn(t, clone, "remote", "set-url", "origin", tc.origin)
			stub := stubTasks{task: engine.TaskInfo{ID: 5, RepoURL: tc.project, BaseRef: "main", Branch: "brigada/task-5"}}
			var out, errb bytes.Buffer
			code := printDiff(context.Background(), stub, clone, 5, "", false, &out, &errb)
			want := fmt.Sprintf("task 5 belongs to %s, but this directory is %s; run diff inside that repository", tc.task, tc.here)
			if code != 1 || !strings.Contains(errb.String(), want) || out.Len() != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q; want refusal %q", code, out.String(), errb.String(), want)
			}
			if strings.Contains(errb.String(), "secret") {
				t.Errorf("refusal leaks credentials: %q", errb.String())
			}
		})
	}
}

func TestDiffRefusesInvalidRepositoryURLs(t *testing.T) {
	_, clone := diffFixture(t)
	for _, tc := range []struct{ name, origin, project, reason string }{
		{"matching aliases", "git@myalias:Team/repo.git", "git@myalias:Team/repo.git", "SSH config alias"},
		{"ambiguous path", "git@example.invalid:/Team/repo.git", "example.invalid/Team/repo", "starting with '/'"},
		{"credential", "https://user:1234/secret@example.invalid/Team/repo", "example.invalid/Team/repo", "credential"},
		{"invalid project", "https://example.invalid/Team/repo.git", "relative/repo", "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitIn(t, clone, "remote", "set-url", "origin", tc.origin)
			stub := stubTasks{task: engine.TaskInfo{ID: 5, RepoURL: tc.project, BaseRef: "main", Branch: "brigada/task-5"}}
			for _, repo := range []string{"", tc.origin} {
				var out, errb bytes.Buffer
				code := printDiff(context.Background(), stub, clone, 5, repo, false, &out, &errb)
				if code != 1 || !strings.Contains(errb.String(), tc.reason) || out.Len() != 0 {
					t.Errorf("exit %d, stdout %q, stderr %q; want refusal with reason %q", code, out.String(), errb.String(), tc.reason)
				}
				if strings.Contains(errb.String(), "secret") {
					t.Errorf("refusal leaks credentials: %q", errb.String())
				}
			}
		})
	}
}

func TestDiffUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"diff"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage: brigada diff") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}
