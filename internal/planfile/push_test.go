package planfile

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// remoteGit runs git against the bare remote and returns its output.
func remoteGit(t *testing.T, remote string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", remote}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, remote, err, out)
	}
	return string(out)
}

func TestPushCreatesTheBranchAndTheFile(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	p := goldenPlan()
	target := Target{URL: remote, Branch: "brigada/plans"}
	if err := Push(context.Background(), target, p); err != nil {
		t.Fatalf("push: %v", err)
	}
	want, _ := Render(p)
	if got := remoteGit(t, remote, "show", "brigada/plans:"+Path(7)); got != string(want) {
		t.Errorf("the file on the plans branch:\n%s\nwant:\n%s", got, want)
	}
	if got := strings.TrimSpace(remoteGit(t, remote, "log", "--format=%an <%ae> | %cn | %s", "brigada/plans")); got !=
		"brigada <brigada@brigada.invalid> | brigada | plan 7: created: active, 3 tasks" {
		t.Errorf("commit = %q", got)
	}
	// The default branch is untouched (§6.4: the engine never commits there on its own).
	if got := strings.TrimSpace(remoteGit(t, remote, "log", "--format=%s", "main")); got != "the base commit" {
		t.Errorf("main's log = %q, want the base commit alone", got)
	}
}

func TestPushUpdatesAndSkipsAnIdenticalFile(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	ctx := context.Background()
	target := Target{URL: remote, Branch: "brigada/plans"}
	p := goldenPlan()
	if err := Push(ctx, target, p); err != nil {
		t.Fatal(err)
	}
	if err := Push(ctx, target, p); err != nil {
		t.Fatalf("push of the same file: %v", err)
	}
	if n := strings.Count(remoteGit(t, remote, "log", "--format=%h", "brigada/plans"), "\n"); n != 1 {
		t.Errorf("commits after an identical push = %d, want 1", n)
	}

	p.Tasks[1].State = "running"
	p.Tasks[1].Question = ""
	if err := Push(ctx, target, p); err != nil {
		t.Fatalf("push of a change: %v", err)
	}
	log := remoteGit(t, remote, "log", "--format=%s", "brigada/plans")
	if !strings.HasPrefix(log, "plan 7: task api needs_foreman -> running\n") {
		t.Errorf("log = %q, want the change named first", log)
	}

	// A second plan lands beside the first on the same branch.
	other := goldenPlan()
	other.ID = 8
	if err := Push(ctx, target, other); err != nil {
		t.Fatal(err)
	}
	files := remoteGit(t, remote, "ls-tree", "-r", "--name-only", "brigada/plans")
	if files != Path(7)+"\n"+Path(8)+"\n" {
		t.Errorf("files on the branch = %q", files)
	}
}

func TestPushToTheDefaultBranch(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	target := Target{URL: remote, Branch: "main", Default: true}
	if err := Push(context.Background(), target, goldenPlan()); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := remoteGit(t, remote, "ls-tree", "-r", "--name-only", "main"); got != Path(7)+"\nREADME.md\n" {
		t.Errorf("files on main = %q, want the plan file beside the README", got)
	}
}

func TestPushNeverCreatesTheDefaultBranch(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "empty.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	err := Push(context.Background(), Target{URL: remote, Branch: "main", Default: true}, goldenPlan())
	if !errors.Is(err, ErrNoDefaultBranch) {
		t.Errorf("push to a remote with no default branch = %v, want ErrNoDefaultBranch", err)
	}
	// A plans branch needs no default branch: it is an orphan.
	if err := Push(context.Background(), Target{URL: remote, Branch: "brigada/plans"}, goldenPlan()); err != nil {
		t.Errorf("push of the plans branch to an empty remote: %v", err)
	}
}

func TestPushFailuresAreErrors(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "nowhere.git")
	for name, target := range map[string]Target{
		"a remote that does not exist": {URL: missing, Branch: "brigada/plans"},
		"no URL":                       {Branch: "brigada/plans"},
		"no branch":                    {URL: missing},
		"a bad branch name":            {URL: missing, Branch: "bad..name"},
		"an option for a branch":       {URL: missing, Branch: "--force"},
	} {
		if err := Push(ctx, target, goldenPlan()); err == nil {
			t.Errorf("%s: push succeeded", name)
		}
	}
}
