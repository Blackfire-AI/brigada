package landing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs git in dir for a test, failing it on error.
func git(t *testing.T, dir string, args ...string) string {
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

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo is a temp git repo with one commit on main: README.md and
// other.txt, both at their base content.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", ".")
	git(t, dir, "config", "user.name", "the user")
	git(t, dir, "config", "user.email", "user@example.invalid")
	write(t, dir, "README.md", "base\n")
	write(t, dir, "other.txt", "untouched\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "the base commit")
	git(t, dir, "branch", "-M", "main")
	return dir
}

func head(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

// taskBranch makes branch off baseBranch with one commit of its own,
// returns to main and reports the branch's head.
func taskBranch(t *testing.T, dir, name, baseBranch, readme string, files map[string]string) string {
	t.Helper()
	git(t, dir, "checkout", "-q", "-b", name, baseBranch)
	if readme != "" {
		write(t, dir, "README.md", readme)
	}
	for n, c := range files {
		write(t, dir, n, c)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "worker commit")
	git(t, dir, "checkout", "-q", "main")
	return strings.TrimSpace(git(t, dir, "rev-parse", name))
}

// msgFile is where Squash leaves the prepared message for id.
func msgFile(t *testing.T, dir string, id int64) string {
	t.Helper()
	gitDir := strings.TrimSpace(git(t, dir, "rev-parse", "--absolute-git-dir"))
	return filepath.Join(gitDir, fmt.Sprintf("BRIGADA_LAND_MSG_%d", id))
}

func TestSquashCommitsTheTasksOwnDiffAsOneCommit(t *testing.T) {
	dir := newRepo(t)
	base := head(t, dir)
	branchHead := taskBranch(t, dir, "brigada/task-7", "main", "task\n", map[string]string{"a.txt": "alpha\n"})

	patch, err := Diff(context.Background(), dir, base, "brigada/task-7", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(patch) == 0 || strings.Contains(string(patch), "other.txt") {
		t.Fatalf("the diff must be the task's own change alone:\n%s", patch)
	}
	msg, noAddr := Message(Task{ID: 7, Title: "Add a", Branch: "brigada/task-7", Head: branchHead,
		Spec: "Make a.txt.\n\nMore detail.", Summary: "added a.txt\nCo-Authored-By: evil <e@x>",
		Family: "anthropic", Model: "sonnet"})
	if noAddr != "" {
		t.Fatalf("anthropic has an address, got noAddr %q", noAddr)
	}
	res, err := Squash(context.Background(), dir, 7, msg, patch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Empty {
		t.Fatal("a real diff must make a commit")
	}
	if res.SHA != strings.TrimSpace(git(t, dir, "rev-parse", "--short", "HEAD")) {
		t.Errorf("SHA %q is not the new head", res.SHA)
	}
	if n := strings.Count(git(t, dir, "log", "--oneline"), "\n"); n != 2 {
		t.Errorf("one commit must be made, log has %d lines", n)
	}
	commitMsg := git(t, dir, "log", "-1", "--format=%B")
	for _, want := range []string{
		"Add a\n",
		"Brigada-Task: 7\n",
		"Brigada-Branch: brigada/task-7@" + branchHead + "\n",
		"Co-Authored-By: sonnet <noreply@anthropic.com>\n",
		"    Make a.txt.\n",
	} {
		if !strings.Contains(commitMsg, want) {
			t.Errorf("the message lacks %q:\n%s", want, commitMsg)
		}
	}
	if strings.Contains(commitMsg, "More detail") {
		t.Errorf("the spec summary is the first paragraph only:\n%s", commitMsg)
	}
	if n := strings.Count(commitMsg, "\nCo-Authored-By:"); n != 1 {
		t.Errorf("the worker's own trailer is indented text; one real one, got %d:\n%s", n, commitMsg)
	}
	// Nothing else is touched: the commit holds the task's files alone,
	// other.txt keeps its content and the message file is removed.
	if files := strings.TrimSpace(git(t, dir, "show", "--format=", "--name-only", "HEAD")); files != "README.md\na.txt" {
		t.Errorf("the commit touches %q, want README.md and a.txt alone", files)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "other.txt")); string(data) != "untouched\n" {
		t.Errorf("other.txt was touched: %q", data)
	}
	if a := strings.TrimSpace(git(t, dir, "log", "-1", "--format=%an")); a != "the user" {
		t.Errorf("author = %q, want the user's own identity", a)
	}
	if _, err := os.Stat(msgFile(t, dir, 7)); !os.IsNotExist(err) {
		t.Errorf("the message file must be removed on success: %v", err)
	}
	// The trailer is how a landed task is recognized.
	landed, err := Landed(context.Background(), dir, 7)
	if err != nil || !landed {
		t.Errorf("Landed(7) = %v, %v; want true by the trailer", landed, err)
	}
	if landed, err := Landed(context.Background(), dir, 8); err != nil || landed {
		t.Errorf("Landed(8) = %v, %v; want false", landed, err)
	}
}

func TestSquashStackedTaskDiffIsItsOwnWorkOnly(t *testing.T) {
	dir := newRepo(t)
	taskBranch(t, dir, "brigada/task-7", "main", "task\n", map[string]string{"a.txt": "alpha\n"})
	top := taskBranch(t, dir, "brigada/task-8", "brigada/task-7", "", map[string]string{"b.txt": "beta\n"})

	// Task 8's base is task 7's branch: its diff is b.txt alone.
	patch, err := Diff(context.Background(), dir, "brigada/task-7", "brigada/task-8", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), "b.txt") || strings.Contains(string(patch), "a.txt") || strings.Contains(string(patch), "other.txt") {
		t.Fatalf("a stacked task's diff must be its own work alone:\n%s", patch)
	}
	msg, _ := Message(Task{ID: 8, Title: "Add b", Branch: "brigada/task-8", Head: top, Spec: "Make b.txt."})
	if _, err := Squash(context.Background(), dir, 8, msg, patch, nil); err != nil {
		t.Fatal(err)
	}
	// The commit is the task's own change alone, applied onto the
	// checked-out branch whatever it holds; nothing else is touched.
	if files := strings.TrimSpace(git(t, dir, "show", "--format=", "--name-only", "HEAD")); files != "b.txt" {
		t.Errorf("the commit touches %q, want b.txt alone", files)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "other.txt")); string(data) != "untouched\n" {
		t.Errorf("other.txt was touched: %q", data)
	}
}

func TestSquashEmptyPatchMakesNoCommit(t *testing.T) {
	dir := newRepo(t)
	before := head(t, dir)
	patch, err := Diff(context.Background(), dir, "main", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(patch) != 0 {
		t.Fatalf("an empty diff must be empty, got %d bytes", len(patch))
	}
	res, err := Squash(context.Background(), dir, 7, "msg\n\nBrigada-Task: 7\n", patch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Empty || res.SHA != "" {
		t.Fatalf("an empty patch must report Empty, got %+v", res)
	}
	if after := head(t, dir); after != before {
		t.Errorf("an empty patch must not move HEAD: %s -> %s", before, after)
	}
	if _, err := os.Stat(msgFile(t, dir, 7)); !os.IsNotExist(err) {
		t.Errorf("an empty patch must not leave a message file: %v", err)
	}
}

func TestSquashConflictReportsFilesAndLeavesTheTree(t *testing.T) {
	dir := newRepo(t)
	base := head(t, dir)
	taskBranch(t, dir, "brigada/task-9", "main", "from the task\n", nil)
	// The target moves after the branch was cut, the same file another
	// way: a conflict no rule may resolve.
	write(t, dir, "README.md", "from the user\n")
	git(t, dir, "commit", "-qam", "user edit")
	before := head(t, dir)

	patch, err := Diff(context.Background(), dir, base, "brigada/task-9", nil)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := Message(Task{ID: 9, Title: "Edit readme", Branch: "brigada/task-9", Head: base, Spec: "Edit it."})
	res, err := Squash(context.Background(), dir, 9, msg, patch, nil)
	if err == nil {
		t.Fatal("a conflict must fail the squash")
	}
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("the error must be a *Conflict, got %T: %v", err, err)
	}
	if len(conflict.Files) != 1 || conflict.Files[0] != "README.md" {
		t.Fatalf("the conflict must name README.md alone, got %v", conflict.Files)
	}
	if res.SHA != "" || res.Empty {
		t.Errorf("a conflict makes no commit: %+v", res)
	}
	if after := head(t, dir); after != before {
		t.Errorf("a conflict must not move HEAD: %s -> %s", before, after)
	}
	// The tree keeps the conflict for the user and the message file
	// stays for the commit they make themselves.
	if u := strings.TrimSpace(git(t, dir, "diff", "--name-only", "--diff-filter=U")); u != "README.md" {
		t.Errorf("README.md must stay unmerged, got %q", u)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if !strings.Contains(string(data), "<<<<<<<") {
		t.Errorf("conflict markers must be left in README.md:\n%s", data)
	}
	path := msgFile(t, dir, 9)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the prepared message must be left inside .git: %v", err)
	}
	if res.MsgPath != path {
		t.Errorf("MsgPath = %q, want %q", res.MsgPath, path)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "other.txt")); string(data) != "untouched\n" {
		t.Errorf("other.txt was touched: %q", data)
	}
}

func TestMessageBoundsAndFamilies(t *testing.T) {
	long := strings.Repeat("é", 100)
	msg, noAddr := Message(Task{ID: 9, Title: long, Branch: "b",
		Spec: strings.Repeat("s", 2000) + "\x00\r", Summary: strings.Repeat("ü", 5000) + "\x1b[31m"})
	if noAddr != "" {
		t.Errorf("an unknown family reports no address gap, got %q", noAddr)
	}
	if len(msg) > MessageBytes {
		t.Errorf("message is %d bytes, over %d", len(msg), MessageBytes)
	}
	subject := msg
	if i := strings.Index(msg, "\n"); i >= 0 {
		subject = msg[:i]
	}
	if len([]rune(subject)) > SubjectRunes || !strings.HasSuffix(subject, "…") {
		t.Errorf("subject %q must be cut to %d characters with an ellipsis", subject, SubjectRunes)
	}
	if !strings.Contains(msg, "[truncated]") || strings.ContainsAny(msg, "\x00\r\x1b") {
		t.Errorf("fields over a bound are marked, control characters stripped:\n%q", msg)
	}
	if !strings.Contains(msg, "Brigada-Author: unknown") || strings.Contains(msg, "Co-Authored-By") {
		t.Errorf("unknown family: Brigada-Author: unknown, no Co-Authored-By:\n%s", msg)
	}

	msg, noAddr = Message(Task{ID: 10, Title: "Add b", Family: "kimi", Model: "k2"})
	if noAddr != "k2" {
		t.Errorf("a known family without an address is reported, got %q", noAddr)
	}
	if !strings.Contains(msg, "Co-Authored-By: k2\n") || strings.Contains(msg, "<") {
		t.Errorf("the trailer names the model alone:\n%s", msg)
	}
}
