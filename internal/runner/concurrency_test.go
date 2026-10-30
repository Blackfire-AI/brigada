package runner

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// The concurrency tests bound every wait: a worker that expects its
// sibling gives up after eight seconds and a test's own waits end at
// ten, so a sequential regression fails instead of hanging.

// rendezvousWorker starts, then waits (bounded) until a second worker
// has started too; it leaves a "met" marker only if it saw the second.
// Both can only see each other if they run at the same time.
var rendezvousWorker = `
cat > /dev/null
echo '{"type":"assistant","text":"starting"}'
: > "$BRIGADA_TEST_MARKERS/started.$$"
i=0
while [ "$(ls "$BRIGADA_TEST_MARKERS" | grep -c '^started\.')" -lt 2 ] && [ $i -lt 80 ]; do sleep 0.1; i=$((i+1)); done
if [ "$(ls "$BRIGADA_TEST_MARKERS" | grep -c '^started\.')" -ge 2 ]; then : > "$BRIGADA_TEST_MARKERS/met.$$"; fi
echo ok > work.txt
` + fakeResultScript(`{"status":"done","summary":"met the other worker"}`)

// logWorker appends S at its start and E at its end to one shared log:
// two runs that never overlap leave S E S E.
var logWorker = `
cat > /dev/null
echo S >> "$BRIGADA_TEST_MARKERS/log"
sleep 0.15
echo E >> "$BRIGADA_TEST_MARKERS/log"
echo ok > work.txt
` + fakeResultScript(`{"status":"done","summary":"logged"}`)

func countMarkers(t *testing.T, dir, prefix string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the marker directory: %v", err)
	}
	n := 0
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), prefix) {
			n++
		}
	}
	return n
}

func TestRunnerMaxParallelRunsJobsConcurrently(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	var jobs []int64
	for i := 0; i < 2; i++ {
		task := e.seedTask("", "test -f work.txt")
		jobs = append(jobs, e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", "")))
	}
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, rendezvousWorker), 100*time.Millisecond,
		func(c *Config) { c.MaxParallel = 2 })
	defer p.stop(t)

	for _, job := range jobs {
		e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	}
	if got := countMarkers(t, markers, "met."); got != 2 {
		t.Errorf("%d workers saw the other running, want both: the jobs did not run concurrently", got)
	}
	e.assertNoWorktrees()
}

func TestRunnerMaxParallelOneIsSequential(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	var jobs []int64
	for i := 0; i < 2; i++ {
		task := e.seedTask("", "test -f work.txt")
		jobs = append(jobs, e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", "")))
	}
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, logWorker), 100*time.Millisecond,
		func(c *Config) { c.MaxParallel = 1 })
	defer p.stop(t)

	for _, job := range jobs {
		e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	}
	data, err := os.ReadFile(filepath.Join(markers, "log"))
	if err != nil {
		t.Fatalf("read the run log: %v", err)
	}
	if got := string(data); got != "S\nE\nS\nE\n" {
		t.Errorf("run log = %q, want the second job to start only after the first ended (S E S E)", got)
	}
}

func TestRunnerShutdownMidJobLetsTheLeaseLapse(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{LeaseTTL: 700 * time.Millisecond})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", "", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, slowWorker), 100*time.Millisecond,
		func(c *Config) { c.MaxParallel = 2 })
	e.waitUntil("the worker to start", func() bool {
		_, err := os.Stat(filepath.Join(markers, "started"))
		return err == nil
	})
	// Run returns only once its job has ended: stop waits for it.
	p.stop(t)

	if _, err := os.Stat(filepath.Join(markers, "finished")); err == nil {
		t.Fatal("the worker ran to the end through the runner's shutdown")
	}
	// Nothing was completed: the lease lapses and the job is queued for
	// another run, as it always was.
	e.waitUntil("the lease to lapse", func() bool {
		if _, err := e.eng.Sweep(e.ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		info, err := e.eng.Job(e.ctx, job)
		return err == nil && info != nil && info.Status == "queued"
	})
	if len(e.artifacts(job)) != 0 {
		t.Errorf("a shutdown run delivered artifacts: %v", e.artifacts(job))
	}
}

// TestJobWaitingForTheMirrorKeepsItsLease holds the mirror's lock from
// the test for longer than the lease lasts: the leased job waits in its
// set-up, and its heartbeats must keep the lease meanwhile — it is not
// requeued, not leased a second time — and it completes once the lock
// is let go.
func TestJobWaitingForTheMirrorKeepsItsLease(t *testing.T) {
	remote := newRemote(t)
	const leaseTTL = 2 * time.Second
	e := newTestEnv(t, engine.Config{LeaseTTL: leaseTTL})
	e.seedProject(remote)
	task := e.seedTask("", "test -f work.txt")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))
	markerDir(t)

	unlock, err := lockMirror(context.Background(), mirrorPath(e.stateDir, remote))
	if err != nil {
		t.Fatalf("lock the mirror: %v", err)
	}
	released := false
	defer func() {
		if !released {
			unlock()
		}
	}()
	p := e.startRunner(writeWorkerScript(t, editOnlyWorker), 100*time.Millisecond,
		func(c *Config) { c.MaxParallel = 2 })
	defer p.stop(t)

	e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "leased" || j.Status == "running" })
	deadline := time.Now().Add(2*leaseTTL + leaseTTL/2)
	for time.Now().Before(deadline) {
		if _, err := e.eng.Sweep(e.ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st := e.str1(job, "status"); st != "leased" && st != "running" {
		t.Fatalf("job status after waiting past the lease = %q, want it still held", st)
	}
	if n := e.int64Of("SELECT attempts FROM jobs WHERE id = ?", job); n != 1 {
		t.Fatalf("job attempts = %d, want 1: the lease lapsed and was taken again", n)
	}

	unlock()
	released = true
	e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.assertNoWorktrees()
}

func (e *testEnv) str1(job int64, col string) string {
	e.t.Helper()
	var v string
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT "+col+" FROM jobs WHERE id = ?", job).Scan(&v)
	})
	if err != nil {
		e.t.Fatalf("read %s of job %d: %v", col, job, err)
	}
	return v
}

// TestFollowUpWaitsForTheAuthorsWorktree: a review or fix of a task can
// be leased the moment the author's completion is published, while the
// author's worktree still has the result branch checked out. Its set-up
// must wait for that worktree to come down — not fail against the
// checked-out branch — and then succeed.
func TestFollowUpWaitsForTheAuthorsWorktree(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"review", "fix"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			cfg := &Config{StateDir: t.TempDir()}
			remote := newRemote(t)

			author, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
				JobID: 1, TaskID: 4, Attempt: 1, Kind: "work", RepoURL: remote, BaseRef: "main", Branch: "brigada/task-4",
			})
			if err != nil {
				t.Fatalf("prepare the author's workspace: %v", err)
			}
			defer author.remove(ctx)
			git(t, author.dir, "commit", "--allow-empty", "-m", "work")
			if err := author.ensurePushed(ctx); err != nil {
				t.Fatalf("push the author's branch: %v", err)
			}

			grant := &api.JobGrant{JobID: 2, TaskID: 4, Attempt: 1, Kind: kind, RepoURL: remote, BaseRef: "main", Branch: "brigada/task-4"}
			if kind == "review" {
				grant.Branch = ""
				grant.ReviewBranch = "brigada/task-4"
			}
			type result struct {
				ws  *workspace
				err error
			}
			got := make(chan result, 1)
			go func() {
				ws, err := prepareWorkspace(ctx, cfg, grant)
				got <- result{ws, err}
			}()
			select {
			case r := <-got:
				if r.ws != nil {
					r.ws.remove(ctx)
				}
				t.Fatalf("the %s was set up while the author's worktree still held the branch (err %v)", kind, r.err)
			case <-time.After(100 * time.Millisecond):
			}

			author.remove(ctx) // the author's completion is published; its worktree comes down
			select {
			case r := <-got:
				if r.err != nil {
					t.Fatalf("the %s's set-up after the author's removal: %v", kind, r.err)
				}
				defer r.ws.remove(ctx)
				if out := git(t, r.ws.dir, "log", "--oneline", "-1"); !strings.Contains(out, "work") {
					t.Errorf("the %s does not sit on the author's work: %q", kind, out)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("the %s never got its workspace after the author's removal", kind)
			}
		})
	}
}

// TestReLeasedReviewWaitsForTheOldAttemptsWorktree: a review whose
// lease lapsed is leased again (attempt 2) while the first attempt's run
// has not yet noticed. Both attempts run at the task's review path
// (§5.1: the path is the session's, not the job's), so the second's
// set-up waits for the first's worktree to come down instead of racing
// it — and then gets a clean checkout at the same path.
func TestReLeasedReviewWaitsForTheOldAttemptsWorktree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := &Config{StateDir: t.TempDir()}
	remote := newRemote(t)
	author, err := prepareWorkspace(ctx, cfg, &api.JobGrant{
		JobID: 1, TaskID: 4, Attempt: 1, Kind: "work", RepoURL: remote, BaseRef: "main", Branch: "brigada/task-4",
	})
	if err != nil {
		t.Fatalf("prepare the author's workspace: %v", err)
	}
	git(t, author.dir, "commit", "--allow-empty", "-m", "work")
	if err := author.ensurePushed(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}
	author.remove(ctx)

	review := func(attempt int64) *api.JobGrant {
		return &api.JobGrant{JobID: 9, TaskID: 4, Attempt: attempt, Kind: "review", RepoURL: remote,
			BaseRef: "main", ReviewBranch: "brigada/task-4"}
	}
	first, err := prepareWorkspace(ctx, cfg, review(1))
	if err != nil {
		t.Fatalf("prepare the first review attempt: %v", err)
	}

	// The re-leased attempt runs at the same path and must wait: the
	// first attempt's worktree is still live on it.
	type result struct {
		ws  *workspace
		err error
	}
	got := make(chan result, 1)
	go func() {
		ws, err := prepareWorkspace(ctx, cfg, review(2))
		got <- result{ws, err}
	}()
	select {
	case r := <-got:
		if r.ws != nil {
			r.ws.remove(ctx)
		}
		t.Fatalf("the second attempt was set up while the first still held the path (err %v)", r.err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(first.dir, "README.md")); err != nil {
		t.Fatalf("something touched the first attempt's live checkout: %v", err)
	}

	first.remove(ctx) // the first attempt finally sees it lost the lease
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("the second attempt's set-up after the first's removal: %v", r.err)
		}
		defer r.ws.remove(ctx)
		if r.ws.dir != first.dir {
			t.Errorf("the re-leased review moved to %s, want the session's own path %s", r.ws.dir, first.dir)
		}
		if _, err := os.Stat(filepath.Join(r.ws.dir, "README.md")); err != nil {
			t.Errorf("the second attempt has no clean checkout: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second attempt never got its workspace after the first's removal")
	}
}

// TestWorkspacesOfOneRepoDoNotRaceTheMirror prepares and removes the
// workspaces of many jobs of one repo at once, on a state directory
// with no mirror yet: without the per-mirror lock the first clones race
// each other and the worktree adds and prunes race the records.
func TestWorkspacesOfOneRepoDoNotRaceTheMirror(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := &Config{StateDir: t.TempDir()}
	remote := newRemote(t)

	const n = 6
	wss := make([]*workspace, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wss[i], errs[i] = prepareWorkspace(ctx, cfg, &api.JobGrant{
				JobID: int64(100 + i), TaskID: int64(10 + i), Attempt: 1, RepoURL: remote, BaseRef: "main",
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("prepare workspace %d: %v", i, err)
		}
	}
	// The worktrees stand side by side, each on its own branch.
	for i, ws := range wss {
		if _, err := os.Stat(filepath.Join(ws.dir, "README.md")); err != nil {
			t.Errorf("workspace %d has no checkout: %v", i, err)
		}
		if want := fmt.Sprintf("brigada/task-%d", 10+i); ws.branch != want {
			t.Errorf("workspace %d branch = %q, want %q", i, ws.branch, want)
		}
	}
	for _, ws := range wss {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ws.remove(ctx)
		}()
	}
	wg.Wait()
	entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "worktrees"))
	if err == nil && len(entries) != 0 {
		t.Errorf("worktrees left behind: %v", entries)
	}
	if out := git(t, "", "--git-dir", mirrorPath(cfg.StateDir, remote), "worktree", "list"); strings.Count(out, "\n") != 1 {
		t.Errorf("the mirror still lists worktrees:\n%s", out)
	}
}
