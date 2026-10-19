package planfile

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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

// TestPushAppliesConfigScopedByTheRemoteURL: includeIf "hasconfig:remote.*.url"
// config reaches the scratch repository, as it does a real clone.
func TestPushAppliesConfigScopedByTheRemoteURL(t *testing.T) {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Fatalf("git --version: %v", err)
	}
	m := regexp.MustCompile(`(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if m == nil {
		t.Skipf("cannot read the git version from %q", out)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 2 || (major == 2 && minor < 36) {
		t.Skipf("git %s.%s is older than 2.36, which introduced hasconfig:remote.*.url", m[1], m[2])
	}

	remote := testsupport.NewGitRemote(t)
	tmp := t.TempDir()
	hooks := filepath.Join(tmp, "hooks")
	marker := filepath.Join(tmp, "pre-push-ran")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\ntouch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	included := filepath.Join(tmp, "included.gitconfig")
	if err := os.WriteFile(included, []byte("[core]\n\thooksPath = "+hooks+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(tmp, "global.gitconfig")
	cfg := "[includeIf \"hasconfig:remote.*.url:" + remote + "\"]\n\tpath = " + included + "\n"
	if err := os.WriteFile(global, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	if err := Push(context.Background(), Target{URL: remote, Branch: "brigada/plans"}, goldenPlan()); err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the pre-push hook set through the URL-scoped config did not run: %v", err)
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
