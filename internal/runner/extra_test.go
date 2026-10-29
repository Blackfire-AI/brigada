package runner

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// setTaskExtra records extra repositories on the task, as the engine does at
// the task's creation (§5.8).
func (e *testEnv) setTaskExtra(task int64, repos ...engine.ExtraRepo) {
	e.t.Helper()
	b, err := json.Marshal(engine.ExtraReposSpec{Repos: repos, Source: engine.ExtraSourceTask})
	if err != nil {
		e.t.Fatal(err)
	}
	err = e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE tasks SET extra_repos = ? WHERE id = ?", string(b), task)
		return err
	})
	if err != nil {
		e.t.Fatalf("record the task's extra repositories: %v", err)
	}
}

// remoteHead is the commit the remote holds for a branch.
func remoteHead(t *testing.T, remote, branch string) string {
	t.Helper()
	out := git(t, t.TempDir(), "ls-remote", remote, "refs/heads/"+branch)
	f := strings.Fields(out)
	if len(f) < 2 {
		t.Fatalf("remote %s holds no %s: %q", remote, branch, out)
	}
	return f[0]
}

// pushCommit adds a file in a new commit on the remote's main and returns
// the commit.
func pushCommit(t *testing.T, remote, file, content string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "-q", remote, work)
	if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "-A")
	git(t, work, "-c", "user.name=seed", "-c", "user.email=seed@brigada.invalid", "commit", "-qm", "add "+file)
	git(t, work, "push", "-q", remote, "main")
	return remoteHead(t, remote, "main")
}

// extraWorker records what the worker sees of the extra checkout — the
// prompt and the read dirs it arrived with, then the checkout itself —
// tries to write to it and to push from it, then finishes.
var extraWorker = `
cat > "$BRIGADA_TEST_MARKERS/worker-prompt"
printf '%s\n' "$BRIGADA_FAKE_READ_DIRS" > "$BRIGADA_TEST_MARKERS/worker-readdirs"
echo "$BRIGADA_REPO_SCHEMA" > "$BRIGADA_TEST_MARKERS/worker-path"
echo "$BRIGADA_REPO_LIVE" > "$BRIGADA_TEST_MARKERS/worker-live-path"
if echo hacked > "$BRIGADA_REPO_SCHEMA/hack.txt" 2>/dev/null; then : > "$BRIGADA_TEST_MARKERS/write-ok"; else : > "$BRIGADA_TEST_MARKERS/write-denied"; fi
git -C "$BRIGADA_REPO_SCHEMA" push origin HEAD:refs/heads/hack > "$BRIGADA_TEST_MARKERS/push.out" 2>&1 && : > "$BRIGADA_TEST_MARKERS/push-ok"
git -C "$BRIGADA_REPO_SCHEMA" push > "$BRIGADA_TEST_MARKERS/push2.out" 2>&1 && : > "$BRIGADA_TEST_MARKERS/push2-ok"
echo hello > work.txt
` + fakeResultScript(`{"status":"done","summary":"edited"}`)

func TestRunnerExtraReposCheckedOutForSetupWorkerAndCheck(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	extra := newRemote(t)
	pinned := remoteHead(t, extra, "main")
	tip := pushCommit(t, extra, "second.txt", "second\n")

	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	markers := markerDir(t)
	check := fmt.Sprintf(`echo "$BRIGADA_REPO_SCHEMA" > "$TEST_MARKERS_DIR/check-path" &&
test "$(git -C "$BRIGADA_REPO_SCHEMA" rev-parse HEAD)" = %s &&
test ! -f "$BRIGADA_REPO_SCHEMA/second.txt" &&
test "$(git -C "$BRIGADA_REPO_LIVE" rev-parse HEAD)" = %s &&
test -f "$BRIGADA_REPO_LIVE/second.txt" &&
test -z "$(git -C "$BRIGADA_REPO_SCHEMA" remote)" &&
test ! -e hack.txt`, pinned, tip)
	task := e.seedTask("", check)
	e.setTaskSetup(task, engine.SetupSpec{
		Command:        `echo "$BRIGADA_REPO_SCHEMA" > "$TEST_MARKERS_DIR/setup-path"`,
		TimeoutMinutes: 5,
	})
	e.setTaskExtra(task,
		engine.ExtraRepo{Name: "schema", Repo: "example.invalid/org/schema", URL: extra, Ref: pinned},
		engine.ExtraRepo{Name: "live", Repo: "example.invalid/org/schema", URL: extra},
	)
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))

	p := e.startRunner(writeWorkerScript(t, extraWorker), 100*time.Millisecond)
	defer p.stop(t)
	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })

	if res := e.checkResult(done); res.ExitCode != 0 {
		t.Fatalf("check = %+v, want a pass (the checkouts at the pinned and the default ref)", res)
	}
	// The stable path beside the worktree, the same one for the setup, the
	// worker and the check. The worktree is the task's session-stable
	// path (§5.1), not the job's own.
	want := filepath.Join(e.stateDir, "worktrees", fmt.Sprintf("task-%d-author-s0.extra", task), "schema")
	for _, name := range []string{"setup-path", "worker-path", "check-path"} {
		got := strings.Join(markerLines(t, markers, name), "")
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := strings.Join(markerLines(t, markers, "worker-live-path"), ""); got != filepath.Join(filepath.Dir(want), "live") {
		t.Errorf("BRIGADA_REPO_LIVE = %q", got)
	}

	// The prompt names each checkout at its absolute path (§5.8), so a
	// worker with no shell for echo or ls learns it there; and the
	// adapter's read grant carries the same dirs, in the grant's order.
	workerPrompt := strings.Join(markerLines(t, markers, "worker-prompt"), "\n")
	for _, wantLine := range []string{
		"Extra repositories, read-only",
		"schema at " + want + " (also in $BRIGADA_REPO_SCHEMA).",
		filepath.Join(filepath.Dir(want), "live") + " (also in $BRIGADA_REPO_LIVE).",
	} {
		if !strings.Contains(workerPrompt, wantLine) {
			t.Errorf("the worker's prompt lacks %q:\n%s", wantLine, workerPrompt)
		}
	}
	if got := strings.Join(markerLines(t, markers, "worker-readdirs"), "\n"); got != want+"\n"+filepath.Join(filepath.Dir(want), "live") {
		t.Errorf("the job's read dirs = %q, want the two checkouts", got)
	}

	// A write attempt does not reach the remote: the checkout has no remote
	// to push to, the files refuse the write, and the remote is unchanged.
	if _, err := os.Stat(filepath.Join(markers, "push-ok")); err == nil {
		t.Error("the worker pushed from the extra checkout to origin")
	}
	if _, err := os.Stat(filepath.Join(markers, "push2-ok")); err == nil {
		t.Error("the worker pushed from the extra checkout with no remote named")
	}
	if os.Geteuid() != 0 { // root ignores file modes
		if _, err := os.Stat(filepath.Join(markers, "write-denied")); err != nil {
			t.Error("the worker's write into the extra checkout was not refused")
		}
	}
	if out := git(t, t.TempDir(), "ls-remote", extra); strings.Contains(out, "hack") || remoteHead(t, extra, "main") != tip {
		t.Errorf("the extra repository's remote changed:\n%s", out)
	}
	if out := git(t, t.TempDir(), "ls-remote", remote); strings.Contains(out, "hack") {
		t.Errorf("the task's remote gained the worker's branch:\n%s", out)
	}

	// The commits the job saw are in its record.
	res := e.workerResult(done)
	if res.ExtraRepos["schema"] != pinned || res.ExtraRepos["live"] != tip {
		t.Errorf("recorded commits = %v, want schema=%s live=%s", res.ExtraRepos, pinned, tip)
	}
	// Deleted with the worktree, read-only files and all.
	e.assertNoWorktrees()
}

func TestRunnerExtraRepoThatCannotBeFetchedIsAnInfrastructureFailure(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	clk := newManualClock()
	e := newTestEnv(t, engine.Config{Now: clk.Now})
	e.seedProject(remote)
	markers := markerDir(t)
	task := e.seedTask("", "true")
	e.setTaskExtra(task, engine.ExtraRepo{Name: "gone", Repo: "example.invalid/org/gone", URL: "file://" + filepath.Join(t.TempDir(), "no-such.git")})
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))

	p := e.startRunner(writeWorkerScript(t, setupWorker), 100*time.Millisecond)
	defer p.stop(t)
	failed := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "failed" })

	if msg := e.errorResult(failed); msg != "setup failed: extra repository gone could not be checked out" {
		t.Errorf("error = %q, want the name and none of git's output", msg)
	}
	if _, err := os.Stat(filepath.Join(markers, "worker-started")); err == nil {
		t.Error("the worker started without its extra repository")
	}
	// Handled like a failed setup (§5.7): retried, then parked for the
	// foreman, with no agent charged. The retries wait a growing
	// engine-clock delay (§3, issue #46): step the clock forward so the
	// two retries become leasable without sleeping the test for the real
	// 5s + 15s.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if e.strOf("SELECT state FROM tasks WHERE id = ?", task) == "needs_foreman" {
			break
		}
		clk.Advance(5 * time.Second)
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the task to park after its retries")
		}
		time.Sleep(15 * time.Millisecond)
	}
	if n := e.int64Of("SELECT COUNT(*) FROM agents WHERE health != 'ready'"); n != 0 {
		t.Errorf("%d agents were marked unhealthy", n)
	}
	e.assertNoWorktrees()
}

func TestExtraRepoEnvRefusesAMalformedName(t *testing.T) {
	for name, ok := range map[string]bool{"schema": true, "a-b_c1": true, "": false, "Schema": false, "1a": false, "../x": false, "a/b": false} {
		if got := validExtraName(name); got != ok {
			t.Errorf("validExtraName(%q) = %v, want %v", name, got, ok)
		}
	}
}

// TestRunnerWithoutExtrasKeepsThePromptPlain pins the other side of §5.8:
// the paragraph and the read dirs exist only when the job has extra
// repositories; a job without them sees its prompt exactly as the engine
// composed it plus the runner's base note (§5.3, issue #129) alone, and no
// read dir at all.
func TestRunnerWithoutExtrasKeepsThePromptPlain(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	markers := markerDir(t)
	task := e.seedTask("", "true")
	composed := prompt.ComposeWorkerPrompt("Add work.txt.", "", "")
	job := e.submitWork(task, composed)

	plainWorker := `
cat > "$BRIGADA_TEST_MARKERS/worker-prompt"
printf '%s\n' "$BRIGADA_FAKE_READ_DIRS" > "$BRIGADA_TEST_MARKERS/worker-readdirs"
` + fakeResultScript(`{"status":"done","summary":"nothing to read beyond the worktree"}`)
	p := e.startRunner(writeWorkerScript(t, plainWorker), 100*time.Millisecond)
	defer p.stop(t)
	e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })

	want := strings.TrimSpace(prompt.WithWorkerBase(composed, "main", remoteHead(t, remote, "main")))
	if got := strings.Join(markerLines(t, markers, "worker-prompt"), "\n"); got != want {
		t.Errorf("the prompt without extras changed:\n%s", got)
	}
	if got := strings.Join(markerLines(t, markers, "worker-readdirs"), ""); got != "" {
		t.Errorf("the job without extras carried read dirs: %q", got)
	}
}

func TestWithEnvKeepsTheInheritedEnvironment(t *testing.T) {
	t.Setenv("BRIGADA_TEST_INHERIT", "yes")
	got := withEnv(nil, []string{"BRIGADA_REPO_X=/x"})
	var inherited, added bool
	for _, kv := range got {
		inherited = inherited || kv == "BRIGADA_TEST_INHERIT=yes"
		added = added || kv == "BRIGADA_REPO_X=/x"
	}
	if !inherited || !added {
		t.Errorf("withEnv(nil, ...) lost the process environment (inherited=%v added=%v)", inherited, added)
	}
	if withEnv(nil, nil) != nil {
		t.Error("withEnv adds nothing to nothing, and keeps nil meaning inherit")
	}
}
