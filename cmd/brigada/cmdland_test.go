package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

type fakeLand struct {
	tasks map[int64]engine.TaskInfo
	land  map[int64]engine.TaskLandInfo
	check string
}

func (f fakeLand) Task(_ context.Context, id int64) (engine.TaskInfo, error) {
	return f.tasks[id], nil
}
func (f fakeLand) TaskLand(_ context.Context, id int64) (engine.TaskLandInfo, error) {
	return f.land[id], nil
}
func (f fakeLand) ProjectSettings(context.Context, string) (engine.ProjectSettings, error) {
	// The engine renders an unset check as "none", flagged default.
	if f.check == "" {
		return engine.ProjectSettings{Settings: []engine.SettingValue{{Key: engine.SettingDefaultCheck, Value: "none", Default: true}}}, nil
	}
	return engine.ProjectSettings{Settings: []engine.SettingValue{{Key: engine.SettingDefaultCheck, Value: f.check}}}, nil
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// landFixture is a bare remote with main, two stacked task branches
// (task 1 on main adds a.txt, task 2 on task 1's branch adds b.txt), and
// a clone of the remote to land in.
func landFixture(t *testing.T) (remote, clone string, f fakeLand) {
	t.Helper()
	remote = testsupport.NewGitRemote(t)
	work := t.TempDir()
	gitIn(t, work, "clone", "-q", remote, ".")
	gitIn(t, work, "checkout", "-q", "-b", "brigada/task-1", "origin/main")
	writeFile(t, work, "a.txt", "alpha\n")
	gitIn(t, work, "add", "-A")
	gitIn(t, work, "commit", "-qm", "worker commit one")
	gitIn(t, work, "push", "-q", "origin", "brigada/task-1")
	gitIn(t, work, "checkout", "-q", "-b", "brigada/task-2", "brigada/task-1")
	writeFile(t, work, "b.txt", "beta\n")
	gitIn(t, work, "add", "-A")
	gitIn(t, work, "commit", "-qm", "worker commit two")
	gitIn(t, work, "push", "-q", "origin", "brigada/task-2")
	clone = t.TempDir()
	gitIn(t, clone, "clone", "-q", remote, ".")
	gitIn(t, clone, "checkout", "-q", "-B", "main", "origin/main")
	gitIn(t, clone, "remote", "set-head", "origin", "main")
	gitIn(t, clone, "config", "user.name", "the user")
	gitIn(t, clone, "config", "user.email", "user@example.invalid")
	f = fakeLand{
		check: "true",
		tasks: map[int64]engine.TaskInfo{
			1: {ID: 1, State: "done", RepoURL: remote, BaseRef: "main", Branch: "brigada/task-1"},
			2: {ID: 2, State: "done", RepoURL: remote, BaseRef: "brigada/task-1", Branch: "brigada/task-2"},
		},
		land: map[int64]engine.TaskLandInfo{
			1: {ID: 1, Title: "Add a", Class: "implement", State: "done", Spec: "Make a.txt.\n\nMore detail.", Approved: true,
				Summary: "added a.txt\nCo-Authored-By: evil <e@x>", Family: "anthropic", Model: "sonnet"},
			2: {ID: 2, Title: "Add b", Class: "implement", State: "done", Spec: "Make b.txt.", Approved: true, Deps: []int64{1}},
		},
	}
	return remote, clone, f
}

func remoteRefs(t *testing.T, remote string) string {
	t.Helper()
	return gitIn(t, remote, "for-each-ref")
}

func land(t *testing.T, f fakeLand, clone string, opts landOptions) (code int, out, errs string) {
	t.Helper()
	var o, e bytes.Buffer
	code = landTasks(context.Background(), f, clone, opts, &o, &e)
	return code, o.String(), e.String()
}

func TestLandStackedTasksAsTwoCommits(t *testing.T) {
	remote, clone, f := landFixture(t)
	before := remoteRefs(t, remote)

	code, out, errs := land(t, f, clone, landOptions{ids: []int64{2, 1}})
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	log := gitIn(t, clone, "log", "--format=%s", "main")
	if !strings.HasPrefix(log, "Add b\nAdd a\n") {
		t.Fatalf("want task 1 then task 2 on top of main, got:\n%s", log)
	}
	// Task 2's commit holds only its own diff.
	if files := strings.TrimSpace(gitIn(t, clone, "show", "--format=", "--name-only", "HEAD")); files != "b.txt" {
		t.Fatalf("task 2's commit touches %q, want only b.txt", files)
	}
	if files := strings.TrimSpace(gitIn(t, clone, "show", "--format=", "--name-only", "HEAD~1")); files != "a.txt" {
		t.Fatalf("task 1's commit touches %q, want only a.txt", files)
	}
	msg := gitIn(t, clone, "log", "-1", "--format=%B", "HEAD~1")
	for _, want := range []string{"Brigada-Task: 1\n", "Brigada-Branch: brigada/task-1@", "Co-Authored-By: sonnet <noreply@anthropic.com>\n", "    Make a.txt.\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("task 1's message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "More detail") {
		t.Errorf("the spec summary is the first paragraph only:\n%s", msg)
	}
	// The worker's own "trailer" is indented text, and only one real
	// Co-Authored-By line exists.
	if n := strings.Count(msg, "\nCo-Authored-By:"); n != 1 {
		t.Errorf("want one real Co-Authored-By trailer, got %d:\n%s", n, msg)
	}
	if !strings.Contains(gitIn(t, clone, "log", "-1", "--format=%B", "HEAD"), "Brigada-Author: unknown") {
		t.Errorf("an unknown family must say Brigada-Author: unknown")
	}
	if a := strings.TrimSpace(gitIn(t, clone, "log", "-1", "--format=%an", "HEAD")); a != "the user" {
		t.Errorf("author = %q, want the user's own identity", a)
	}
	if !strings.Contains(out, "git push origin main") {
		t.Errorf("the push command must be printed:\n%s", out)
	}
	if !strings.Contains(out, "no approved head is recorded") {
		t.Errorf("the missing approved head must be warned about:\n%s", out)
	}
	if after := remoteRefs(t, remote); after != before {
		t.Fatalf("the remote's refs changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// A rerun finds both landed by their trailers.
	code, out, errs = land(t, f, clone, landOptions{ids: []int64{1, 2}})
	if code != 0 || !strings.Contains(out, "already landed") || !strings.Contains(out, "no commits made") {
		t.Fatalf("rerun: exit %d\n%s\n%s", code, out, errs)
	}
}

func TestLandRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, remote, clone string, f *fakeLand) landOptions
		want  string
	}{
		{"dirty tree", func(t *testing.T, _, clone string, _ *fakeLand) landOptions {
			writeFile(t, clone, "README.md", "changed\n")
			return landOptions{ids: []int64{1}}
		}, "working tree is dirty"},
		{"untracked file", func(t *testing.T, _, clone string, _ *fakeLand) landOptions {
			writeFile(t, clone, "scratch.txt", "x\n")
			return landOptions{ids: []int64{1}}
		}, "working tree is dirty"},
		{"unapproved", func(t *testing.T, _, _ string, f *fakeLand) landOptions {
			li := f.land[1]
			li.Approved = false
			f.land[1] = li
			return landOptions{ids: []int64{1}}
		}, "task 1: not approved"},
		{"not done", func(t *testing.T, _, _ string, f *fakeLand) landOptions {
			ti := f.tasks[1]
			ti.State = "reviewing"
			f.tasks[1] = ti
			return landOptions{ids: []int64{1}}
		}, "task 1: not approved"},
		{"other class", func(t *testing.T, _, _ string, f *fakeLand) landOptions {
			li := f.land[1]
			li.Class = "review"
			f.land[1] = li
			return landOptions{ids: []int64{1}}
		}, "only implement tasks land"},
		{"head moved after approval", func(t *testing.T, _, _ string, f *fakeLand) landOptions {
			li := f.land[1]
			li.ApprovedHead = strings.Repeat("a", 40)
			f.land[1] = li
			return landOptions{ids: []int64{1}}
		}, "changed after approval"},
		{"wrong repository", func(t *testing.T, _, _ string, f *fakeLand) landOptions {
			ti := f.tasks[1]
			ti.RepoURL = "https://example.invalid/other/repo"
			f.tasks[1] = ti
			return landOptions{ids: []int64{1}}
		}, "belongs to https://example.invalid/other/repo"},
		{"dependency neither landed nor named", func(t *testing.T, _, _ string, _ *fakeLand) landOptions {
			return landOptions{ids: []int64{2}}
		}, "depends on task 1, which is neither landed nor in this call"},
		{"no default check", func(t *testing.T, _, _ string, f *fakeLand) landOptions {
			f.check = ""
			return landOptions{ids: []int64{1}}
		}, "no default check"},
		{"not the target branch", func(t *testing.T, _, clone string, _ *fakeLand) landOptions {
			gitIn(t, clone, "switch", "-q", "-c", "side")
			return landOptions{ids: []int64{1}}
		}, "git switch main"},
		{"behind its remote", func(t *testing.T, remote, _ string, _ *fakeLand) landOptions {
			other := t.TempDir()
			gitIn(t, other, "clone", "-q", remote, ".")
			gitIn(t, other, "checkout", "-q", "-B", "main", "origin/main")
			writeFile(t, other, "later.txt", "later\n")
			gitIn(t, other, "add", "-A")
			gitIn(t, other, "commit", "-qm", "later work")
			gitIn(t, other, "push", "-q", "origin", "main")
			return landOptions{ids: []int64{1}}
		}, "behind or has diverged"},
		{"merge in progress", func(t *testing.T, _, clone string, _ *fakeLand) landOptions {
			head := strings.TrimSpace(gitIn(t, clone, "rev-parse", "HEAD"))
			writeFile(t, filepath.Join(clone, ".git"), "MERGE_HEAD", head+"\n")
			return landOptions{ids: []int64{1}}
		}, "in progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote, clone, f := landFixture(t)
			opts := tc.setup(t, remote, clone, &f)
			before := remoteRefs(t, remote)
			head := strings.TrimSpace(gitIn(t, clone, "rev-parse", "HEAD"))
			code, out, errs := land(t, f, clone, opts)
			if code == 0 || !strings.Contains(errs, tc.want) {
				t.Fatalf("exit %d, want a refusal containing %q\nstdout: %s\nstderr: %s", code, tc.want, out, errs)
			}
			if got := strings.TrimSpace(gitIn(t, clone, "rev-parse", "HEAD")); got != head {
				t.Errorf("a refusal moved HEAD: %s -> %s", head, got)
			}
			if after := remoteRefs(t, remote); after != before {
				t.Errorf("the remote's refs changed")
			}
		})
	}
}

func TestLandLandedFlagStandsForADependency(t *testing.T) {
	_, clone, f := landFixture(t)
	code, out, errs := land(t, f, clone, landOptions{ids: []int64{2}, landed: []int64{1}})
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "--landed") {
		t.Errorf("the report must show --landed:\n%s", out)
	}
}

func TestLandFailingCheckKeepsCommitsAndPrintsNoPushCommand(t *testing.T) {
	_, clone, f := landFixture(t)
	f.check = "exit 3"
	code, out, _ := land(t, f, clone, landOptions{ids: []int64{1}})
	if code == 0 || strings.Contains(out, "git push") || !strings.Contains(out, "check FAILED") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(gitIn(t, clone, "log", "--format=%s", "-1"), "Add a") {
		t.Errorf("a failing check must leave the commits")
	}
}

func TestLandNoCheckIsShown(t *testing.T) {
	_, clone, f := landFixture(t)
	f.check = ""
	code, out, errs := land(t, f, clone, landOptions{ids: []int64{1}, noCheck: true})
	if code != 0 || !strings.Contains(out, "skipped (--no-check)") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

func TestLandConflictStopsNamingFilesAndLeavesTheTree(t *testing.T) {
	remote, clone, f := landFixture(t)
	// Task 3 edits README.md one way, and main already has it another.
	work := t.TempDir()
	gitIn(t, work, "clone", "-q", remote, ".")
	gitIn(t, work, "checkout", "-q", "-b", "brigada/task-3", "origin/main")
	writeFile(t, work, "README.md", "# from the task\n")
	gitIn(t, work, "add", "-A")
	gitIn(t, work, "commit", "-qm", "worker commit three")
	gitIn(t, work, "push", "-q", "origin", "brigada/task-3")
	f.tasks[3] = engine.TaskInfo{ID: 3, State: "done", RepoURL: remote, BaseRef: "main", Branch: "brigada/task-3"}
	f.land[3] = engine.TaskLandInfo{ID: 3, Title: "Edit readme", Class: "implement", State: "done", Approved: true}
	writeFile(t, clone, "README.md", "# from the user\n")
	gitIn(t, clone, "commit", "-qam", "user edit")
	before := remoteRefs(t, remote)
	start := strings.TrimSpace(gitIn(t, clone, "rev-parse", "HEAD"))

	code, out, errs := land(t, f, clone, landOptions{ids: []int64{1, 3}})
	if code == 0 {
		t.Fatalf("a conflict must stop with a non-zero exit\n%s\n%s", out, errs)
	}
	if !strings.Contains(errs, "README.md") || !strings.Contains(errs, "git reset --hard "+short(start)) && !strings.Contains(errs, "git reset --hard") {
		t.Errorf("the report must name the file and the way back:\n%s", errs)
	}
	if strings.Contains(out+errs, "git push origin") {
		t.Errorf("no push command after a conflict:\n%s\n%s", out, errs)
	}
	if u := strings.TrimSpace(gitIn(t, clone, "diff", "--name-only", "--diff-filter=U")); u != "README.md" {
		t.Errorf("the tree must keep the conflict for the user, unmerged = %q", u)
	}
	data, _ := os.ReadFile(filepath.Join(clone, "README.md"))
	if !strings.Contains(string(data), "<<<<<<<") {
		t.Errorf("conflict markers must be left in README.md:\n%s", data)
	}
	if !strings.Contains(gitIn(t, clone, "log", "--format=%s", "-3"), "Add a") {
		t.Errorf("the earlier task's commit stays")
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "BRIGADA_LAND_MSG_3")); err != nil {
		t.Errorf("the prepared message must be left inside .git: %v", err)
	}
	if after := remoteRefs(t, remote); after != before {
		t.Errorf("the remote's refs changed")
	}
}

func TestLandMessageBounds(t *testing.T) {
	long := strings.Repeat("é", 100)
	tk := &landTask{id: 9, head: "abc", info: engine.TaskInfo{Branch: "b"},
		land: engine.TaskLandInfo{Title: long, Spec: strings.Repeat("s", 2000) + "\x00\r", Summary: strings.Repeat("ü", 5000) + "\x1b[31m"}}
	msg, _ := landMessage(tk)
	if len(msg) > landMessageBytes {
		t.Errorf("message is %d bytes, over %d", len(msg), landMessageBytes)
	}
	if subject := subjectOf(msg); len([]rune(subject)) > landSubjectRunes || !strings.HasSuffix(subject, "…") {
		t.Errorf("subject %q must be cut to %d characters with an ellipsis", subject, landSubjectRunes)
	}
	if !strings.Contains(msg, "[truncated]") || strings.ContainsAny(msg, "\x00\r\x1b") {
		t.Errorf("fields over a bound are marked, control characters stripped:\n%q", msg)
	}
	if !strings.Contains(msg, "Brigada-Author: unknown") || strings.Contains(msg, "Co-Authored-By") {
		t.Errorf("unknown family: Brigada-Author: unknown, no Co-Authored-By:\n%s", msg)
	}
}
